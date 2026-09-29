package store

import (
	"context"
	"path/filepath"
	"testing"
)

func setup(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenMemoryAppliesSchema(t *testing.T) {
	s := setup(t)
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 7 {
		t.Fatalf("only %d tables created, want the full schema", n)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := setup(t)
	// Migrations run on every start; running twice must be a no-op, not
	// a "table already exists" error that stops the service booting.
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMetaRoundTripAndAbsent(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	if got, err := s.Meta(ctx, "nope"); err != nil || got != "" {
		t.Fatalf("absent meta = %q, %v; want empty and no error", got, err)
	}
	if err := s.SetMeta(ctx, "node_count", "123047"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(ctx, "node_count", "123050"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Meta(ctx, "node_count")
	if err != nil || got != "123050" {
		t.Fatalf("meta = %q, %v; want the second write to win", got, err)
	}
	if n, err := s.MetaInt(ctx, "node_count"); err != nil || n != 123050 {
		t.Fatalf("MetaInt = %d, %v", n, err)
	}
}

func TestMetaIntRejectsGarbage(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	if err := s.SetMeta(ctx, "k", "not-a-number"); err != nil {
		t.Fatal(err)
	}
	// A silent 0 here would make a broken build look like an empty one.
	if _, err := s.MetaInt(ctx, "k"); err == nil {
		t.Fatal("MetaInt accepted a non-integer and returned no error")
	}
	if got := s.MetaIntDefault(ctx, "k", 42); got != 42 {
		t.Fatalf("MetaIntDefault = %d, want the fallback 42", got)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), "", ""); err == nil {
		t.Fatal("Open accepted an empty db path")
	}
}

func TestOpenCorpusFailsFastOnWrongShape(t *testing.T) {
	// A wrong --corpus path must say so immediately, not surface later
	// as a confusing "no such column" from inside a signal.
	dir := t.TempDir()
	bad := filepath.Join(dir, "not-a-corpus.db")
	if err := createTinyDB(t, bad, `CREATE TABLE unrelated(x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), filepath.Join(dir, "k.db"), bad)
	if err == nil {
		t.Fatal("Open accepted a db that is not a corpus")
	}
	if !contains(err.Error(), "required tables") {
		t.Fatalf("error %q does not explain the shape problem", err)
	}
}

func TestOpenWithCorpusSetsCorpusDB(t *testing.T) {
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus.db")
	if err := createTinyDB(t, corpus, corpusSchema); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), filepath.Join(dir, "k.db"), corpus)
	if err != nil {
		t.Fatalf("Open with corpus: %v", err)
	}
	defer s.Close()
	if s.Corpus == nil {
		t.Fatal("Corpus is nil after a successful open")
	}
	// The corpus must be genuinely read-only, not merely opened that way.
	if _, err := s.Corpus.Exec(`CREATE TABLE should_fail(x INTEGER)`); err == nil {
		t.Fatal("the corpus accepted a write; mode=ro is not in force")
	}
}

func TestLogRecordsStageDetail(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	if err := s.Log(ctx, "ingest works", "rows_in=10 rows_kept=9 rows_dropped=1"); err != nil {
		t.Fatal(err)
	}
	var stage, detail string
	if err := s.DB.QueryRow(`SELECT stage, detail FROM build_log LIMIT 1`).Scan(&stage, &detail); err != nil {
		t.Fatal(err)
	}
	if stage != "ingest works" || detail == "" {
		t.Fatalf("log = %q / %q", stage, detail)
	}
}

const corpusSchema = `
CREATE TABLE works(id INTEGER PRIMARY KEY, title TEXT, word_count INTEGER, kudos INTEGER, hits INTEGER, bookmarks INTEGER, update_date TEXT, first_seen TEXT, authors TEXT, summary TEXT, url TEXT, chapters TEXT, language TEXT, complete INTEGER, rating TEXT);
CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT);
CREATE TABLE work_tags(work_id INTEGER, tag_id INTEGER, tag_type TEXT);
CREATE TABLE cooccurrence_edges(tag_a_id INTEGER, tag_b_id INTEGER, cooccur_count INTEGER);
`

func createTinyDB(t *testing.T, path, body string) error {
	t.Helper()
	s, err := Open(context.Background(), path, "")
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = s.DB.Exec(body)
	return err
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
