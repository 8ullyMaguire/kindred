// Package store is kindred's SQL data layer.
//
// Two databases, never one: the corpus is an existing read-only mirror
// (1.7 GB, on the pool, measured at 45.8 MB/s over the 8-way mergerfs
// mount versus 2.7 GB/s on local NVMe), and kindred.db is our own state
// on local disk. The corpus is attached read-only and never rewritten;
// a copy is what made the previous Python deployment workable and what
// would have cost 1.7 GB per host.
//
// The store is the seam for a later Postgres port (SPEC §5) and the only
// place that speaks SQL. Handlers never query; signals never query.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Domain errors. The API maps these to status codes, ported from the
// proven concord pattern.
var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("already exists")
	ErrInvalid   = errors.New("invalid")
)

// StableSalt returns the operator's stable pseudonymisation salt,
// generating and storing one on first use.
//
// It is opt-in via --stable-salt, and it is not a secret: the salt is
// published in every manifest, so what it buys is cross-dump linkage, not
// protection. Generating it lazily means enabling the mode once works
// without a separate setup step, and every later dump reuses it.
func (s *Store) StableSalt(ctx context.Context) ([]byte, error) {
	raw, err := s.Meta(ctx, "stable_salt")
	if err == nil && raw != "" {
		decoded, derr := hex.DecodeString(raw)
		if derr == nil && len(decoded) > 0 {
			return decoded, nil
		}
	}
	// No salt yet: create one and remember it.
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate stable salt: %w", err)
	}
	if err := s.SetMeta(ctx, "stable_salt", hex.EncodeToString(salt)); err != nil {
		return nil, err
	}
	return salt, nil
}

// EmbeddingCount reports how many embeddings are stored.
func (s *Store) EmbeddingCount(ctx context.Context) (int, error) {
	var n int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE kind = 'tag'`).Scan(&n); err != nil {
		return 0, err
	}
	return int(n), nil
}

// EmbeddingStats reports how many embeddings are stored and at what width.
//
// Width matters: a mean width across a mixed set hides the case where a
// host has 32-dim vectors for some entities and 64 for others, and a cosine
// over mismatched vectors is not a cosine at all.
func (s *Store) EmbeddingStats(ctx context.Context) (map[string]any, error) {
	var n int64
	var minDim, maxDim sql.NullInt64
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(dim), MAX(dim) FROM embeddings WHERE kind = 'tag'`).Scan(&n, &minDim, &maxDim)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"count": n}
	if minDim.Valid {
		out["min_dim"] = minDim.Int64
		out["max_dim"] = maxDim.Int64
	}
	return out, nil
}

// Store holds both databases.
type Store struct {
	DB     *sql.DB // kindred's own state, read-write, local disk
	Corpus *sql.DB // the mirror, read-only
	Path   string

	// agreementCacheMu guards agreementCache, ownerVector and ownerKey.
	//
	// It guards ownerVector and ownerKey as well because a refresh ticker
	// swaps them while requests are judging, and assigning a map field is
	// not atomic: an unsynchronised reader during a refresh is a
	// concurrent map read and write, which is a fatal runtime error rather
	// than a race the detector merely flags. Same reason Engine guards its
	// ratings. Use SetOwnerVector to replace them and ownerState to read a
	// consistent pair.
	agreementCacheMu sync.Mutex
	// agreementCache maps voterKey to cached agreement with the owner vector.
	agreementCache map[string]agreementCacheEntry
	// ownerVector is the owner's taste vector, built from profile tags and
	// learned weights. Nil means owner features are disabled.
	ownerVector map[int64]float64
	// ownerKey is the arena owner key the configured owner session resolves
	// to, when a deployment named one. Empty means no owner is configured.
	ownerKey string
}

// Open opens kindred.db (creating it and its directory) and, if corpusPath
// is non-empty, attaches the corpus read-only.
func Open(ctx context.Context, path, corpusPath string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: empty kindred db path", ErrInvalid)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	// The pragmas that bound memory belong on the WRITING connection, not
	// only the read-only corpus one.
	//
	// The state database is where the 634,231 embedding rows land, and it
	// opened with only busy_timeout, journal_mode and foreign_keys. SQLite
	// then used its default page cache — around 2,000 pages, but growing
	// toward whatever the connection wants — and grew the WAL freely during
	// the bulk insert. Measured: the store step added 46 MB on top of the
	// eigensolver's 279, which is 325 against a 220 MB cap.
	//
	// mmap_size=0 matters most: a write connection that mmaps the database
	// counts the mapped pages as resident, and this database reaches
	// 216 MB. Every byte of that would be charged to the process.
	db, err := sql.Open("sqlite", path+
		"?_pragma=busy_timeout(5000)"+
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(ON)"+
		"&_pragma=mmap_size(0)"+
		"&_pragma=cache_size(-1024)"+
		"&_pragma=temp_store(1)")
	if err != nil {
		return nil, fmt.Errorf("open kindred db: %w", err)
	}
	// One writer. The corpus is read-mostly and the indexer is the only
	// writer; a second writer on SQLite is a lock, not concurrency.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.ExecContext(ctx, `
		PRAGMA mmap_size=0;
		PRAGMA cache_size=-1024;
		PRAGMA wal_autocheckpoint=256;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("pragmas: %w", err)
	}

	s := &Store{DB: db, Path: path}

	if corpusPath != "" {
		if err := s.openCorpus(ctx, corpusPath); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) openCorpus(ctx context.Context, path string) error {
	// mode=ro is the guarantee, not a convention: the corpus is someone
	// else's data and nothing in kindred may write to it.
	//
	// The pragmas are the memory story, and mmap_size=0 is the one that
	// matters: SQLite's default behaviour maps the database into the
	// process address space, and on a 1.7 GB corpus that is a 1.7 GB
	// mapping the kernel can fault in. Measured on a 16 GB host, the Go
	// heap never exceeded 73 MB while peak RSS reached 553 MB — the
	// difference was the corpus mapping, not the program. cache_size is
	// the page cache in KiB, negative for "kibibytes rather than pages".
	dsn := "file:" + path +
		"?mode=ro" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=query_only(ON)" +
		"&_pragma=mmap_size(0)" +
		"&_pragma=cache_size(-2048)" +
		"&_pragma=temp_store(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open corpus: %w", err)
	}
	// The corpus gets a real read pool: unlike our own db, it has no
	// single-writer constraint, and concurrent readers are the whole
	// reason for a pool.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return fmt.Errorf("corpus %s: %w", path, err)
	}
	// Applied after the connection exists: pragmas are per-connection and
	// the pool can open more than one, so the DSN alone is not enough to
	// guarantee every pooled connection is configured.
	for _, pragma := range []string{
		"PRAGMA mmap_size=0",
		"PRAGMA cache_size=-2048",
		"PRAGMA temp_store=1",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return fmt.Errorf("corpus pragma %q: %w", pragma, err)
		}
	}
	if err := assertCorpusShape(ctx, db); err != nil {
		db.Close()
		return err
	}
	s.Corpus = db
	return nil
}

// assertCorpusShape fails fast with an actionable message when the mirror
// is not shaped like a corpus. A wrong --corpus path otherwise surfaces
// much later as a confusing "no such column" from deep inside a signal.
//
// It takes the handle as an argument rather than reading s.Corpus: s.Corpus
// is assigned by the caller *after* this returns, so a method reading it
// would always see nil and panic on every startup with a corpus.
func assertCorpusShape(ctx context.Context, corpus *sql.DB) error {
	var n int
	err := corpus.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
		 ('works','tags','work_tags','cooccurrence_edges')`).Scan(&n)
	if err != nil {
		return fmt.Errorf("probe corpus: %w", err)
	}
	if n < 4 {
		return fmt.Errorf("%w: corpus has %d of the 4 required tables "+
			"(works, tags, work_tags, cooccurrence_edges) — is this a kindred corpus?",
			ErrInvalid, n)
	}
	return nil
}

func (s *Store) Close() error {
	var first error
	if s.Corpus != nil {
		if err := s.Corpus.Close(); err != nil {
			first = err
		}
	}
	if s.DB != nil {
		if err := s.DB.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// OpenMemory returns a hermetic store for tests: an in-memory kindred.db
// with migrations applied, and no corpus. Tests that need a corpus build
// one in a temp dir (NewTestCorpus) rather than sharing a file — a shared
// temp db means CreateUser succeeds in one test and fails on a unique
// violation in another, which is a failure nobody caused.
func OpenMemory(ctx context.Context) (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Path: ":memory:"}
	if err := s.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ---------------------------------------------------------------- schema

const schema = `
CREATE TABLE IF NOT EXISTS graph_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS kinds (
	kind        TEXT PRIMARY KEY,
	entity_count INTEGER NOT NULL DEFAULT 0,
	updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS entity_index (
	entity_id INTEGER PRIMARY KEY,
	kind      TEXT NOT NULL,
	corpus_id INTEGER NOT NULL,
	tag_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_entity_index_kind ON entity_index(kind);

CREATE TABLE IF NOT EXISTS entity_tags (
	entity_id INTEGER NOT NULL,
	tag_id    INTEGER NOT NULL,
	tag_type  TEXT NOT NULL DEFAULT 'freeforms',
	weight    REAL NOT NULL DEFAULT 1.0,
	PRIMARY KEY (entity_id, tag_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_entity_tags_tag ON entity_tags(tag_id);

CREATE TABLE IF NOT EXISTS edges (
	tag_a   INTEGER NOT NULL,
	tag_b   INTEGER NOT NULL,
	weight  REAL NOT NULL,
	PRIMARY KEY (tag_a, tag_b)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS embeddings (
	entity_id INTEGER PRIMARY KEY,
	dim       INTEGER NOT NULL,
	vec       BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS tunes (
	name    TEXT PRIMARY KEY,
	weights TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS build_log (
	at      TEXT NOT NULL DEFAULT (datetime('now')),
	stage   TEXT NOT NULL,
	detail  TEXT NOT NULL
);

-- ------------------------------------------------------------------ arena
--
-- The pairwise comparison arena. A user is shown two works, picks one or
-- neither, and works acquire a Glicko-2 rating from those judgements.
--
-- TWO DESIGN CONSTRAINTS, both from the privacy requirement
-- ("no username, work id or tag id in a snapshot"):
--
-- 1. owner_key is a HASH of whatever identifies the comparator, never the
--    identifier itself. A session cookie would be fine in memory and a
--    privacy leak in the database, and a dump of this file is the thing
--    that travels. There is no table anywhere that can produce a list of
--    users, by construction rather than by policy.
--
-- 2. A comparison records the SESSION it came from, and the session is
--    discarded on expiry. The comparison rows are the durable evidence;
--    the identity that made them is not retained past its usefulness.

-- One judged comparison. Unrated: the presentation happened but no choice
-- was recorded yet, so the row exists only to stop the same pair being
-- shown twice. This is the fatigue record, and it is written at
-- PRESENTATION time rather than at judgement time -- otherwise a user who
-- abandons a pair is shown it again, which is the single most reliable way
-- to make an arena feel broken.
CREATE TABLE IF NOT EXISTS arena_comparisons (
	id          INTEGER PRIMARY KEY,
	-- The id space work_a/work_b are read in (arena.EntityKind). Works keep the
	-- field names they had when they were the only kind; this column is what
	-- makes the arena generic. Defaulted here AND named by every writer, so a
	-- row can never hold an empty kind.
	kind        TEXT NOT NULL DEFAULT 'ao3_work',
	work_a      INTEGER NOT NULL,
	work_b      INTEGER NOT NULL,
	choice      TEXT,              -- 'a', 'b', or NULL when unrated
	session_key TEXT NOT NULL,
	owner_key   TEXT NOT NULL,      -- hashed, never the identifier
	strategy    TEXT NOT NULL,      -- how this pair was chosen
	presented_at TEXT NOT NULL DEFAULT (datetime('now')),
	judged_at   TEXT,
	-- The voter's agreement with the owner at the moment of the vote, so a
	-- later change to the agreement formula cannot rewrite the past. 1.0 is
	-- the pre-existing value for rows judged before this column existed: they
	-- were counted at full weight, and a migration must not silently
	-- re-weight history it cannot recompute.
	agreement   REAL NOT NULL DEFAULT 1.0,
	-- A pair is unordered, so the low id is stored in work_a. Enforced
	-- here rather than trusted from the caller: the no-repeat query
	-- depends on it, and a pair stored the other way round is a pair
	-- shown twice.
	CHECK (work_a < work_b),
	CHECK (choice IS NULL OR choice IN ('a', 'b', 'neither'))
);
-- The no-repeat query. Covering, because it only needs these two columns
-- and reading the whole row to check the choice is what makes it slow.
CREATE INDEX IF NOT EXISTS idx_arena_pairs
	ON arena_comparisons(owner_key, work_a, work_b);
-- idx_arena_pairs_kind (the no-repeat query for one kind) is created by the
-- migration instead of here: this block runs BEFORE the migration, so an index
-- on the kind column would fail -- "no such column: kind" -- on every database
-- written before that column existed.
CREATE INDEX IF NOT EXISTS idx_arena_pending
	ON arena_comparisons(session_key) WHERE judged_at IS NULL;

-- Materialised arena candidates, one row per (kind, entity).
--
-- Why a table rather than a query: the corpus-side loaders are slow by nature.
-- Grouping the mirror's 3,890,504 tag rows measured 2.2s on 2026-10-14, and an
-- arena page cannot pay that. The loaders run on a refresh; this is what a page
-- reads, and a random sample from here is instant.
--
-- It is deliberately the HEAD of the distribution rather than a uniform sample
-- (most-carried tags, most-seen bylines): a ranking of tags nobody uses is not a
-- ranking anyone wants, and the head is also what fits in a table.
CREATE TABLE IF NOT EXISTS entity_candidates (
    kind         TEXT    NOT NULL,
    entity_id    INTEGER NOT NULL,
    name         TEXT    NOT NULL,
    -- The evidence behind the candidate: how many works carry the tag, how
    -- often the byline appeared in the sample. A sampled count is labelled as
    -- one by the loader rather than presented as a total.
    weight       INTEGER NOT NULL DEFAULT 0,
    refreshed_at TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (kind, entity_id)
);

-- One row per kind that has been loaded, including kinds that came back EMPTY.
--
-- Without this, "no candidates" and "never loaded" are the same state and a
-- kind the mirror has almost none of ('characters' held 304 tag rows when this
-- was written) would re-run its 2.2s query on every page view forever. The row
-- also carries the count, so a switcher can offer the kinds the corpus can
-- actually serve -- a measurement, not a promise.
CREATE TABLE IF NOT EXISTS entity_candidate_refreshes (
    kind         TEXT PRIMARY KEY,
    candidates   INTEGER NOT NULL DEFAULT 0,
    refreshed_at TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- Current Glicko-2 rating per entity of any kind. One row per (kind, entity_id),
-- updated in batch. kind='ao3_work' maps to the legacy arena_ratings rows
-- for backward compatibility.
CREATE TABLE IF NOT EXISTS arena_entities (
    kind       TEXT    NOT NULL,
    entity_id  INTEGER NOT NULL,
    mu         REAL    NOT NULL DEFAULT 1500.0,
    phi        REAL    NOT NULL DEFAULT 350.0,
    sigma      REAL    NOT NULL DEFAULT 0.06,
    comparisons INTEGER NOT NULL DEFAULT 0,
    wins       INTEGER NOT NULL DEFAULT 0,
    losses     INTEGER NOT NULL DEFAULT 0,
    draws      INTEGER NOT NULL DEFAULT 0,
    period     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (kind, entity_id)
);

-- One comparison row per (kind, winner, loser, owner_key, period). Used
-- by the agreement-weighted learning path; agreement is stored at vote time
-- so later formula changes cannot rewrite the past.
CREATE TABLE IF NOT EXISTS arena_entity_comparisons (
    kind          TEXT    NOT NULL,
    winner_id     INTEGER NOT NULL,
    loser_id      INTEGER NOT NULL,
    owner_key     TEXT    NOT NULL,
    agreement     REAL    NOT NULL DEFAULT 1.0,
    period        INTEGER NOT NULL,
    created_at    TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (kind, winner_id, loser_id, owner_key, period)
);

-- Append-only history per entity, so a rating change can be explained.
-- one row per (kind, entity_id, period).
CREATE TABLE IF NOT EXISTS arena_entity_history (
    kind         TEXT    NOT NULL,
    entity_id    INTEGER NOT NULL,
    period       INTEGER NOT NULL,
    mu           REAL,
    phi          REAL,
    sigma        REAL,
    PRIMARY KEY (kind, entity_id, period)
);

-- Current Glicko-2 rating per work. One row per work, updated in batch.
-- Kept as a compat layer; new code should use arena_entities.
--
-- Declared ONCE. It was declared twice here, in identical form, and the
-- duplicate was invisible because the statement is CREATE TABLE IF NOT
-- EXISTS: the second one runs, does nothing, and reports success. Two
-- copies that can drift are worse than one, because the next edit lands on
-- whichever copy the editor found first and the other keeps describing the
-- old table.
--
-- phi (the rating deviation) is the confidence and is what stops a single
-- comparison from creating a celebrity.
CREATE TABLE IF NOT EXISTS arena_ratings (
	work_id     INTEGER PRIMARY KEY,
	mu          REAL NOT NULL DEFAULT 1500.0,
	phi         REAL NOT NULL DEFAULT 350.0,
	sigma       REAL NOT NULL DEFAULT 0.06,
	comparisons INTEGER NOT NULL DEFAULT 0,
	wins        INTEGER NOT NULL DEFAULT 0,
	losses      INTEGER NOT NULL DEFAULT 0,
	draws       INTEGER NOT NULL DEFAULT 0,
	-- The batch at the end of which this rating was computed. Ratings
	-- from different periods must never be mixed in one Update, which is
	-- the whole reason this column exists.
	period      INTEGER NOT NULL DEFAULT 0,
	updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_arena_ratings_top ON arena_ratings(mu DESC, phi ASC);
-- The leaderboard filters on this, so it has to be indexed rather than
-- sorted in Go over the whole table.
CREATE INDEX IF NOT EXISTS idx_arena_ratings_confident
	ON arena_ratings(comparisons, mu DESC);

-- What one comparator has learned: how much they care about each tag.
--
-- This is the part that makes the arena personal rather than a global
-- leaderboard, and it is the reason the arena exists next to the tag
-- graph rather than instead of it. The graph knows which tags co-occur;
-- this knows which tags THIS person discriminates on.
--
-- weight is a signed preference and n is the evidence behind it. Without
-- n, a single lucky comparison would put a tag at full strength, and the
-- recommender would then trust it as much as a hundred comparisons.
CREATE TABLE IF NOT EXISTS arena_user_tag_weights (
	owner_key TEXT NOT NULL,
	tag_id    INTEGER NOT NULL,
	weight    REAL NOT NULL DEFAULT 0.0,
	n         INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (owner_key, tag_id)
) WITHOUT ROWID;

-- Sessions, so a pair can be resumed across a page reload and so fatigue
-- can be counted. owner_key here too; session_key is the cookie value and
-- is never logged or exported.
CREATE TABLE IF NOT EXISTS arena_sessions (
	session_key TEXT PRIMARY KEY,
	owner_key   TEXT NOT NULL,
	created_at  TEXT NOT NULL DEFAULT (datetime('now')),
	last_seen   TEXT NOT NULL DEFAULT (datetime('now')),
	comparisons INTEGER NOT NULL DEFAULT 0,
	skips       INTEGER NOT NULL DEFAULT 0
);

-- Append-only history, so a rating change can be explained. Without it,
-- "why is this work ranked 4th" has no answer and a ranking that cannot
-- explain itself is the first thing users stop trusting.
CREATE TABLE IF NOT EXISTS arena_rating_history (
	work_id  INTEGER NOT NULL,
	period   INTEGER NOT NULL,
	mu       REAL NOT NULL,
	phi      REAL NOT NULL,
	sigma    REAL NOT NULL,
	at       TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (work_id, period)
) WITHOUT ROWID;

-- Explicit blocks, separate from the learned weights above.
--
-- arena_user_tag_weights says what the arena INFERRED. A negative weight is a
-- guess from a handful of pairwise choices, and it is lossy: a tag the reader
-- has never been shown a reason to reject will have no weight, and a tag they
-- mildly dislike can sit near zero forever because nothing in a ranking period
-- forces the question.
--
-- A block is a statement, not an inference. It needs no evidence and is not
-- damped by n. The two must not be conflated: folding a block into the weight
-- table would make "block" mean "decide this is very negative", so unblocking
-- could not restore a tag the reader actually likes, and a block would be
-- silently overwritten by the next batch.
--
-- at is kept so the page can say when it was blocked, and so a future
-- "unblock what I have not touched in a year" sweep has something to read.
CREATE TABLE IF NOT EXISTS arena_user_blocked_tags (
	owner_key TEXT NOT NULL,
	tag_id    INTEGER NOT NULL,
	at        TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (owner_key, tag_id)
) WITHOUT ROWID;

/* An algorithm A/B judgement: two named tunes shown side by side for one
   seed, and which list the reader preferred.

   THIS TABLE IS SEPARATE FROM arena_comparisons ON PURPOSE, and the reason
   is the single constraint that deferred this surface for a day.

   arena_comparisons records a preference between two CORPUS ENTITIES, and
   its judgements feed the tag learner: choosing work A over work B is
   evidence about the tags those works carry. An ALGORITHM is not a corpus
   entity. Its id is a tune name, its "candidates" are the same works seen
   twice through different weight vectors, and a preference between them is
   evidence about the WEIGHTS, not about any tag. Writing those votes into
   arena_comparisons would make the learner read "this reader prefers the
   tune that weights recency" as "this reader likes the tags shared by this
   pair of works" — a true-looking number about the wrong thing, which is
   the failure mode this project spends most of its tests preventing.

   So: one row per judgement, the two algorithm NAMES as the evidence, and
   nothing here is ever read by the tag learner. winner is 'a', 'b' or
   'neither' — a draw is a real answer and is stored rather than dropped,
   because dropping draws would quietly inflate every algorithm's win rate
   as the reader grows more indifferent.

   seed_kind + seed_id are recorded so the standings can be broken down by
   kind later. The seed is not part of the primary key: the same reader may
   compare the same two algorithms on the same seed more than once, and
   collapsing those would hide a reader changing their mind. */
CREATE TABLE IF NOT EXISTS algo_comparisons (
	owner_key TEXT NOT NULL,
	seed_kind TEXT NOT NULL,
	seed_id   INTEGER NOT NULL,
	algo_a    TEXT NOT NULL,
	algo_b    TEXT NOT NULL,
	winner    TEXT NOT NULL CHECK (winner IN ('a','b','neither')),
	at        TEXT NOT NULL DEFAULT (datetime('now'))
);

-- The standings read: count wins per algorithm, newest-heavy readers last
-- (no windowing yet; a whole-table scan over this table is small).
CREATE INDEX IF NOT EXISTS idx_algo_comparisons_algo
	ON algo_comparisons (algo_a, algo_b, winner);

/* Interleaved impressions: the online half of the evaluation loop.

   The arena above asks a reader to compare two lists SIDE BY SIDE, which costs
   a comparison and shows them one list that is, by construction, the worse
   one. Interleaving asks the same question with one list: both algorithms
   contribute, every item is tagged with the arm that contributed it, and what
   the reader does next (marks it seen, opens it) is the vote.

   One row per SHOWN item per arm, which is what makes the attribution exact:
   the credit is a flag on the row that carried it, not a guess about which
   experiment was running when the click arrived.

   arm_a/arm_b are the labels the caller used ("default", a tune name), and
   arm is which SIDE contributed this row. The label on every row is redundant
   with the pair and deliberate: the standings query groups by name without a
   join, and a rename in the caller cannot make two rows disagree.

   credited is 0/1 rather than a counter because one impression earns at most
   one credit. Without the index below the credit lookup is a scan per seen
   mark; with it the lookup is the primary key of the work on the page. */
CREATE TABLE IF NOT EXISTS interleave_impressions (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_key TEXT NOT NULL,
	seed_kind TEXT NOT NULL,
	seed_id   INTEGER NOT NULL,
	arm_a     TEXT NOT NULL,
	arm_b     TEXT NOT NULL,
	work_id   INTEGER NOT NULL,
	arm       TEXT NOT NULL CHECK (arm IN ('a','b')),
	slot      INTEGER NOT NULL,
	credited  INTEGER NOT NULL DEFAULT 0,
	shown_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_interleave_credit
	ON interleave_impressions (owner_key, work_id, credited);

CREATE INDEX IF NOT EXISTS idx_interleave_arms
	ON interleave_impressions (arm_a, arm_b, arm);

-- Retention. An impression log with no window grows for the life of the
-- deployment at one row per slot per render, and the standings it feeds are
-- about what readers are doing NOW. Thirty days is the window: RecordInterleave
-- prunes anything older, so the table has a bound and the standings have a
-- stated period rather than "all time", which would mean "since the last
-- restart that happened to prune".
CREATE INDEX IF NOT EXISTS idx_interleave_shown
	ON interleave_impressions (shown_at);

/* Offline evaluation runs.

   The eval command measures recall@K and NDCG@K against held-out bookmarks and
   used to print them and exit. The numbers are the offline half of the
   evaluation loop -- the arena answers "which algorithm does a reader prefer
   today", this answers "did the last change make the recommender better" --
   and a number that is not stored can only be compared with a memory.

   k is a column, not a constant: recall@10 and recall@50 are different
   measurements, and a table that puts them in one column invites a comparison
   that is not one. mode is a column for the same reason: a lite run skips the
   embedding signal, so lite and full are two recommenders. */
/* Where a run was weak and where it was strong, grouped by a corpus
   dimension. A run's recall is one number for the whole mirror; a fandom
   whose held-out works are never found is invisible in it, and that is the
   number that tells a reader whether the recommender works for THEIR corner
   of the archive. One row per (run, axis, group).

   n and hits are stored rather than only the rate: a rate over four works is
   not comparable with one over four hundred, and the page has to be able to
   print both. */
CREATE TABLE IF NOT EXISTS eval_slices (
	run_id INTEGER NOT NULL,
	axis   TEXT NOT NULL,
	name   TEXT NOT NULL,
	n      INTEGER NOT NULL,
	hits   INTEGER NOT NULL,
	PRIMARY KEY (run_id, axis, name)
);

CREATE TABLE IF NOT EXISTS eval_runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	ran_at      TEXT NOT NULL DEFAULT (datetime('now')),
	k           INTEGER NOT NULL,
	readers     INTEGER NOT NULL,
	bookmarks   INTEGER NOT NULL,
	evaluated   INTEGER NOT NULL,
	failures    INTEGER NOT NULL DEFAULT 0,
	held_out    INTEGER NOT NULL,
	hits        INTEGER NOT NULL,
	recall_at_k REAL NOT NULL,
	ndcg_at_k   REAL NOT NULL,
	coverage    REAL NOT NULL DEFAULT 0,
	mode        TEXT NOT NULL DEFAULT 'full',
	/* degraded is the comma-separated list of signals that skipped for every
	   candidate in this run. A run whose idf_overlap never executed measured a
	   different recommender from the one in production, and the number alone
	   cannot say so -- three runs on 2026-10-23 came back byte-identical
	   because the evaluator had no document-frequency table. */
	degraded    TEXT NOT NULL DEFAULT '',
	/* The candidate-generation measurement. pool_recall is the share of
	   held-out works that were IN the candidate set at pool_depth; recall_at_k
	   can never exceed it, so when pool_recall is small the ceiling is the
	   candidate set and no re-weighting can help. Zero means the run did not
	   ask for it. */
	pool_depth  INTEGER NOT NULL DEFAULT 0,
	pool_hits   INTEGER NOT NULL DEFAULT 0,
	pool_recall REAL NOT NULL DEFAULT 0
);

/* Works a reader has already been shown or has marked as read.

   The id is stored as TEXT because the same store serves several kinds
   (ao3_work, tag, author) and a work id is not comparable with a tag id:
   the kinds namespace is kept in the string, exactly as seeds are
   ("ao3_work:1").

   shown_at and read_at are separate because they answer different
   questions. "Do not show me this again" is about the RECOMMENDER repeating
   itself, which is a different complaint from "I have read this", which is
   about the work itself. Collapsing them would mean a reader cannot ask for
   one without getting the other. */
CREATE TABLE IF NOT EXISTS seen_works (
	owner_key TEXT NOT NULL,
	entity_id TEXT NOT NULL,
	kind      TEXT NOT NULL DEFAULT '',
	shown_at  TEXT NOT NULL DEFAULT (datetime('now')),
	read_at   TEXT,
	PRIMARY KEY (owner_key, entity_id)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS seen_works_owner_shown
	ON seen_works (owner_key, shown_at);

/* reader_visits is the per-reader VISIT CURSOR (#65).

   seen_works answers "has this reader met this work"; it cannot answer "what
   arrived since they last looked", because a set has no time order. This table
   stores two instants per reader: when the current visit began, and when the
   one before it began. "New since your last visit" is then a comparison of
   corpus first_seen against previous_at, which is exactly what a set cannot
   express.

   Three instants, each answering a different question: previous_at is the
   BOUNDARY the list is measured from (the start of the visit before this one),
   current_at is when this visit began, and last_at is when we last heard from
   the reader. last_at is separate from current_at because the gap that decides
   "a new visit" has to be measured from ACTIVITY, not from the start of the
   visit: a reader who browses for an hour is one visit, and a schema that
   measured from current_at would split them into two.

   Not a log of visits: the product needs the previous one and nothing older,
   and a log would grow forever to answer a question about its last two rows.

   The roll is done in NoteVisit with the gap as a parameter, not by a trigger,
   because "a new visit" is a product decision (how long away counts as away)
   and a trigger would freeze it into the schema.
*/
/* embed_runs fingerprints the embedding table at each build (#76).

   kindred embed rewrites all 634k rows, and nothing compared the old space with
   the new one: a rebuild that moved every vector was indistinguishable from one
   that reproduced it. Every stored reader weight was trained against the OLD
   positions, so that difference silently invalidates what the site learned.

   A fingerprint rather than a hash of the table: a hash answers "did anything
   change", cannot say whether the space moved or merely reshuffled, and changes
   when one tag is added. rows/dim/mean_norm/centroid/sample answer the question
   the operator actually has.
*/
CREATE TABLE IF NOT EXISTS embed_runs (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	rows      INTEGER NOT NULL,
	dim       INTEGER NOT NULL,
	mean_norm REAL NOT NULL,
	centroid  TEXT NOT NULL,
	sample    TEXT NOT NULL,
	built_at  TEXT NOT NULL
);

/* algo_choices is one reader's algorithm per KIND (#per-kind algorithms).

   The primary key is (owner_key, kind) and there is no global preference row:
   a reader who prefers co-bookmark CF for works has not said anything about
   tags, and a global default that a kind inherits is how a setting ends up
   applying to a surface nobody chose it for. No row means "follow the arm".
*/
CREATE TABLE IF NOT EXISTS algo_choices (
	owner_key TEXT NOT NULL,
	kind      TEXT NOT NULL,
	algo      TEXT NOT NULL,
	chosen_at TEXT NOT NULL,
	PRIMARY KEY (owner_key, kind)
) WITHOUT ROWID;

/* algo_impressions is the A/B log: one row per list served.

   "arm" says WHY this algorithm ran -- "default", "challenger" or "choice" --
   and it is what keeps the two questions apart: an experiment's arms can be
   compared with each other, and a reader's own choice can be looked at without
   being counted as an arm. "credited" is set when something on the list was
   acted on (a work marked read), so a click rate is a property of the SHOW
   that produced the list rather than of the algorithm in the abstract.

   No index on (owner_key, kind, seed_id) beyond the implicit rowid: the
   crediting query walks backwards from the newest row for one reader, and the
   table is written once per related-list request.
*/
CREATE TABLE IF NOT EXISTS algo_impressions (
	id        INTEGER PRIMARY KEY,
	owner_key TEXT NOT NULL,
	kind      TEXT NOT NULL,
	algo      TEXT NOT NULL,
	arm       TEXT NOT NULL,
	seed_id   INTEGER NOT NULL,
	credited  INTEGER NOT NULL DEFAULT 0,
	shown     TEXT NOT NULL DEFAULT '',
	shown_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS algo_impressions_owner
	ON algo_impressions (owner_key, kind, seed_id, id);

CREATE TABLE IF NOT EXISTS reader_visits (
	owner_key   TEXT PRIMARY KEY,
	previous_at TEXT NOT NULL,
	current_at  TEXT NOT NULL,
	last_at     TEXT NOT NULL,
	visits      INTEGER NOT NULL DEFAULT 1
) WITHOUT ROWID;

/* work_feedback is a reader's like or dislike on a work.

   Separate from seen_works because the two answer different questions and
   neither implies the other: seen_works records "I was shown this / I read
   this", work_feedback records "I think this is good or bad". A reader who
   read a fic and disliked it wants the DISLIKE remembered as a negative
   signal, which a read flag cannot express -- and marking it unread later
   must not silently erase the dislike.

   polarity is CHECKed to ±1 rather than left as a free integer: a value of 2
   or 0 would flow into the tag learning as a gradient step of the wrong size
   or the wrong sign, and the CHECK turns that at the write boundary instead
   of at the point where the weights are already wrong.

   The PRIMARY KEY (owner_key, work_id) is what makes a double-submitted form
   harmless: re-posting a like updates the row rather than applying the
   learning twice. The separate history table keeps every CHANGE so a reader
   who flips a dislike to a like can have the original taught back.

   Learned tag weights are NOT stored here. They go to arena_user_tag_weights
   through the same damped averaging the arena already uses, so a reader's taste
   is one set of numbers rather than two that were each adjusted separately. */
CREATE TABLE IF NOT EXISTS work_feedback (
	owner_key TEXT NOT NULL,
	work_id INTEGER NOT NULL,
	polarity INTEGER NOT NULL CHECK (polarity IN (-1, 1)),
	source TEXT NOT NULL DEFAULT 'web',
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (owner_key, work_id)
) WITHOUT ROWID;

/* work_feedback_history is the append-only log of stance changes.

   Kept because a like that becomes a dislike is not a correction of a typo --
   it is new evidence, and the tag weights have to be taught BOTH directions or
   the first press leaves damage behind. Reading the previous stance from here
   is how LearnFromFeedback knows to reverse before it re-applies. */
CREATE TABLE IF NOT EXISTS work_feedback_history (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_key TEXT NOT NULL,
	work_id INTEGER NOT NULL,
	polarity INTEGER NOT NULL CHECK (polarity IN (-1, 1)),
	source TEXT NOT NULL DEFAULT '',
	changed_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS work_feedback_history_owner
	ON work_feedback_history (owner_key, work_id, changed_at DESC);

/* crawl_queue is serve's auto-crawler checkpoint (SPEC auto-crawl).

   The mirror grows from two inputs: works readers ask for that this mirror
   does not hold, and works it holds with metadata missing. Each is queued as
   ONE row -- per-work granularity, which is the checkpoint: done_at marks a
   work finished, restart continues from exactly the rows not yet done, and
   the skip rule lives in the worker (existing metadata is never refetched).
   A batch resume file would redo the finished half and could not express
   "reader asked for this one work".

   attempts parks a job after repeated failure instead of retrying a dead
   URL every poll; a fresh request re-arms it (see EnqueueCrawl). */
CREATE TABLE IF NOT EXISTS crawl_queue (
	url         TEXT PRIMARY KEY,
	source      TEXT NOT NULL,
	enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
	attempts    INTEGER NOT NULL DEFAULT 0,
	last_error  TEXT,
	done_at     TEXT
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS crawl_queue_pending ON crawl_queue (done_at);

/* discovery_pages is the WALK's checkpoint, deliberately a separate table from
   crawl_queue because it answers a different question about a different kind of
   URL. crawl_queue asks "have we mirrored this WORK?"; discovery_pages asks
   "have we read this LISTING page?".

   Without it a walk had no memory at all: discover.Options took no
   already-fetched input, the walk's visited set is per-run and in memory, and
   --state records WORK urls so it could not skip a page even when passed.
   Measured cost of that gap: a 40-page tag is ~20 minutes against AO3's 30 s
   crawl delay, repaid in full on every re-run.

   The skip is "is there a row", not "was it fetched recently", for the same
   reason crawl_queue.done_at is exact: an exact answer cannot be wrong the way
   a recency window can, and re-reading a page is cheap next to fetching a work.
   fetched_at is carried for the operator ("when did we last walk this tag?")
   and for a future --older-than pass, not for the skip itself. */
CREATE TABLE IF NOT EXISTS discovery_pages (
	url         TEXT PRIMARY KEY,
	fetched_at  TEXT NOT NULL DEFAULT (datetime('now')),
	works_found INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
`

// Migrate applies the schema. Idempotent, so it is safe on every start.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// `CREATE TABLE IF NOT EXISTS` does nothing for a table that already
	// exists, so a schema change silently applies only to fresh databases.
	// That is the worst shape for a migration: the code that reads the new
	// column works against a new database and fails against an old one,
	// which is a failure only operators upgrading ever see.
	//
	// So each shape change is applied to existing tables too, by rebuilding
	// the table from itself. The cost is one table copy at startup; the
	// alternative is a version skew that outlives the code.
	return s.migrate(ctx)
}

// migrate brings an existing database up to the current schema.
func (s *Store) migrate(ctx context.Context) error {
	// embeddings gained a `kind` column: entity ids are per-kind, so work
	// 1 and tag 1 must not share a primary key.
	has, err := s.columnExists(ctx, "embeddings", "kind")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.DB.ExecContext(ctx, `
			BEGIN;
			CREATE TABLE embeddings_new (
			  kind       TEXT NOT NULL DEFAULT 'tag',
			  entity_id  INTEGER NOT NULL,
			  dim        INTEGER NOT NULL,
			  vec        BLOB NOT NULL,
			  PRIMARY KEY (kind, entity_id)
			);
			INSERT INTO embeddings_new(kind, entity_id, dim, vec)
			  SELECT 'tag', entity_id, dim, vec FROM embeddings;
			DROP TABLE embeddings;
			ALTER TABLE embeddings_new RENAME TO embeddings;
			COMMIT;`); err != nil {
			return fmt.Errorf("migrate embeddings: %w", err)
		}
	}

	// eval_runs gained a `degraded` column: a run must be able to say which
	// signals never executed. Without it, an evaluation missing the
	// document-frequency table records the same recall as a complete one, and
	// the two are indistinguishable in the numbers -- which is how three
	// identical runs were read as "the new signal changed nothing" rather than
	// "the instrument was blind".
	has, err = s.columnExists(ctx, "eval_runs", "degraded")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.DB.ExecContext(ctx,
			`ALTER TABLE eval_runs ADD COLUMN degraded TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate eval_runs.degraded: %w", err)
		}
	}

	// eval_runs gained the candidate-generation columns, for the same reason
	// it gained `degraded`: a recall number whose ceiling is the candidate set
	// is not comparable with one whose ceiling is the ranking, and only these
	// columns say which it is.
	for _, col := range []struct{ name, decl string }{
		{"pool_depth", "INTEGER NOT NULL DEFAULT 0"},
		{"pool_hits", "INTEGER NOT NULL DEFAULT 0"},
		{"pool_recall", "REAL NOT NULL DEFAULT 0"},
	} {
		has, err = s.columnExists(ctx, "eval_runs", col.name)
		if err != nil {
			return err
		}
		if !has {
			if _, err := s.DB.ExecContext(ctx,
				`ALTER TABLE eval_runs ADD COLUMN `+col.name+` `+col.decl); err != nil {
				return fmt.Errorf("migrate eval_runs.%s: %w", col.name, err)
			}
		}
	}

	// algo_impressions gained a `shown` column: without the ids a list actually
	// carried, a click cannot be attributed to the list that produced it, and
	// "credited" would have to be guessed from the reader's most recent request.
	has, err = s.columnExists(ctx, "algo_impressions", "shown")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.DB.ExecContext(ctx,
			`ALTER TABLE algo_impressions ADD COLUMN shown TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate algo_impressions.shown: %w", err)
		}
	}

	// arena_comparisons gained an `agreement` column: a judgement records the
	// voter's agreement with the owner as it stood at vote time, so changing the
	// agreement formula later cannot rewrite history. The writes in `arena.go`
	// name the column, so an existing database without it fails every judgement
	// with "no such column: agreement" -- a failure only operators upgrading
	// ever see, which is the shape this whole function exists to prevent.
	has, err = s.columnExists(ctx, "arena_comparisons", "agreement")
	if err != nil {
		return err
	}
	if !has {
		// The indexes are recreated here, not left to `schema`: dropping the
		// table drops them with it, and `CREATE INDEX IF NOT EXISTS` in the
		// schema block has already run by now, so without this the migration
		// would quietly cost the no-repeat query its covering index.
		if _, err := s.DB.ExecContext(ctx, `
			BEGIN;
			CREATE TABLE arena_comparisons_new (
			  id           INTEGER PRIMARY KEY,
			  work_a       INTEGER NOT NULL,
			  work_b       INTEGER NOT NULL,
			  choice       TEXT,
			  session_key  TEXT NOT NULL,
			  owner_key    TEXT NOT NULL,
			  strategy     TEXT NOT NULL,
			  presented_at TEXT NOT NULL DEFAULT (datetime('now')),
			  judged_at    TEXT,
			  agreement    REAL NOT NULL DEFAULT 1.0,
			  CHECK (work_a < work_b),
			  CHECK (choice IS NULL OR choice IN ('a', 'b', 'neither'))
			);
			INSERT INTO arena_comparisons_new
			  (id, work_a, work_b, choice, session_key, owner_key, strategy, presented_at, judged_at)
			  SELECT id, work_a, work_b, choice, session_key, owner_key, strategy, presented_at, judged_at
			  FROM arena_comparisons;
			DROP TABLE arena_comparisons;
			ALTER TABLE arena_comparisons_new RENAME TO arena_comparisons;
			CREATE INDEX IF NOT EXISTS idx_arena_pairs
			  ON arena_comparisons(owner_key, work_a, work_b);
			CREATE INDEX IF NOT EXISTS idx_arena_pending
			  ON arena_comparisons(session_key) WHERE judged_at IS NULL;
			COMMIT;`); err != nil {
			return fmt.Errorf("migrate arena_comparisons.agreement: %w", err)
		}
	}

	// arena_comparisons gained a `kind`: every kind's arena writes through this
	// one table (see arena.EntityKind), and a row without a kind cannot say
	// which id space its pair is in. The column is NOT NULL with a default, so
	// the copy below is what gives historical rows their kind -- they were all
	// works, which is exactly what the default says.
	has, err = s.columnExists(ctx, "arena_comparisons", "kind")
	if err != nil {
		return err
	}
	if !has {
		// The indexes are recreated here for the same reason the agreement
		// migration recreates them: dropping the table drops them, and the
		// pairs-kind index is only declared in the schema block, which has
		// already run by now.
		if _, err := s.DB.ExecContext(ctx, `
			BEGIN;
			CREATE TABLE arena_comparisons_new (
			  id           INTEGER PRIMARY KEY,
			  kind         TEXT NOT NULL DEFAULT 'ao3_work',
			  work_a       INTEGER NOT NULL,
			  work_b       INTEGER NOT NULL,
			  choice       TEXT,
			  session_key  TEXT NOT NULL,
			  owner_key    TEXT NOT NULL,
			  strategy     TEXT NOT NULL,
			  presented_at TEXT NOT NULL DEFAULT (datetime('now')),
			  judged_at    TEXT,
			  agreement    REAL NOT NULL DEFAULT 1.0,
			  CHECK (work_a < work_b),
			  CHECK (choice IS NULL OR choice IN ('a', 'b', 'neither'))
			);
			INSERT INTO arena_comparisons_new
			  (id, kind, work_a, work_b, choice, session_key, owner_key, strategy, presented_at, judged_at, agreement)
			  SELECT id, 'ao3_work', work_a, work_b, choice, session_key, owner_key, strategy, presented_at, judged_at, agreement
			  FROM arena_comparisons;
			DROP TABLE arena_comparisons;
			ALTER TABLE arena_comparisons_new RENAME TO arena_comparisons;
			CREATE INDEX IF NOT EXISTS idx_arena_pairs
			  ON arena_comparisons(owner_key, work_a, work_b);
			CREATE INDEX IF NOT EXISTS idx_arena_pending
			  ON arena_comparisons(session_key) WHERE judged_at IS NULL;
			COMMIT;`); err != nil {
			return fmt.Errorf("migrate arena_comparisons.kind: %w", err)
		}
	}
	// Created after the column check, for both a migrated and a fresh database:
	// the schema block above runs first and cannot name a column that a
	// pre-kind database does not have yet.
	if _, err := s.DB.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_arena_pairs_kind
		   ON arena_comparisons(owner_key, kind, work_a, work_b)`); err != nil {
		return fmt.Errorf("create idx_arena_pairs_kind: %w", err)
	}
	return nil
}

// columnExists reports whether a table has a column. It queries the table
// rather than tracking a version number, so a database created by an older
// build, a partial migration, or a manual edit is all handled the same way.
func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---------------------------------------------------------------- meta

// SetMeta records a build fact (node counts, edge counts, build time).
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO graph_meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Meta reads a build fact, returning "" when absent.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM graph_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// MetaInt reads a build fact as an integer.
func (s *Store) MetaInt(ctx context.Context, key string) (int, error) {
	v, err := s.Meta(ctx, key)
	if err != nil || v == "" {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return 0, fmt.Errorf("meta %q is not an integer: %q", key, v)
	}
	return n, nil
}

// Log records an ingest or build stage. Every stage that touches rows logs
// how many it read and how many it kept: a stage that reports 0 kept is
// broken, and a silent 0 is indistinguishable from a legitimate prune
// unless the input count is recorded beside it.
func (s *Store) Log(ctx context.Context, stage, detail string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO build_log(stage, detail) VALUES(?, ?)`, stage, detail)
	return err
}

// MetaIntDefault reads a build fact, falling back to def.
func (s *Store) MetaIntDefault(ctx context.Context, key string, def int) int {
	n, err := s.MetaInt(ctx, key)
	if err != nil {
		return def
	}
	return n
}

// Now returns RFC3339 for build stamps, in one place so stamps sort.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }
