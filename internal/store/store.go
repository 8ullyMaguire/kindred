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
	"database/sql"
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
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
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
	// mapping the kernel can fault in. Measured on the 16 GB host, the Go
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
`

// Migrate applies the schema. Idempotent, so it is safe on every start.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
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
