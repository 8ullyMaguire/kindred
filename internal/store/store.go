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
	work_a      INTEGER NOT NULL,
	work_b      INTEGER NOT NULL,
	choice      TEXT,              -- 'a', 'b', or NULL when unrated
	session_key TEXT NOT NULL,
	owner_key   TEXT NOT NULL,      -- hashed, never the identifier
	strategy    TEXT NOT NULL,      -- how this pair was chosen
	presented_at TEXT NOT NULL DEFAULT (datetime('now')),
	judged_at   TEXT,
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
CREATE INDEX IF NOT EXISTS idx_arena_pending
	ON arena_comparisons(session_key) WHERE judged_at IS NULL;

-- Current Glicko-2 rating per work. One row per work, updated in batch.
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
