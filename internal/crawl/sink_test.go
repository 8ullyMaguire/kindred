package crawl

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// newMirrorDB opens a corpus-mirror-shaped database for the sink to write into.
func newMirrorDB(t *testing.T) *sql.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mirror.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testcorpus.Schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func fixtureWork() *ParsedWork {
	return &ParsedWork{
		ID: 4242, Title: "Written Work", Authors: "auth",
		URL: "https://archiveofourown.org/works/4242", WordCount: 12345,
		Kudos: 99, Hits: 1000, Bookmarks: int64Ptr(42), Language: "English",
		Complete: true, Rating: "Explicit",
		Tags: []ParsedTag{
			{Name: "dark", Type: "freeforms"},
			{Name: "Harry Potter", Type: "fandoms"},
		},
	}
}

func int64Ptr(v int64) *int64 { return &v }

func TestSQLSinkWritesWorkAndItsTags(t *testing.T) {
	db := newMirrorDB(t)
	s := NewSQLSink(db)
	if err := s.Save(context.Background(), fixtureWork()); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM works WHERE id=4242`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Written Work" {
		t.Errorf("title = %q", title)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_tags WHERE work_id=4242`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("tags = %d, want 2", n)
	}
	// The category AO3 gave the tag must survive the round trip, because it is
	// the only authoritative statement of what the tag is.
	var typ string
	if err := db.QueryRow(`SELECT wt.tag_type FROM work_tags wt
		JOIN tags t ON t.id=wt.tag_id WHERE t.name='Harry Potter'`).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "fandoms" {
		t.Errorf("tag_type = %q, want fandoms", typ)
	}
}

// Re-crawling must not duplicate tags. An insert-only write is how a mirror
// accumulates 3.9M rows where 3.8M distinct pairs exist, and the corpus queries
// then count the same work twice.
func TestSQLSinkIsIdempotent(t *testing.T) {
	db := newMirrorDB(t)
	s := NewSQLSink(db)
	w := fixtureWork()
	for i := 0; i < 3; i++ {
		if err := s.Save(context.Background(), w); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	var works, tags int
	if err := db.QueryRow(`SELECT COUNT(*) FROM works WHERE id=4242`).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_tags WHERE work_id=4242`).Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if works != 1 {
		t.Errorf("works = %d after 3 saves, want 1", works)
	}
	if tags != 2 {
		t.Errorf("tags = %d after 3 saves, want 2", tags)
	}
}

// A page that omits the bookmarks meta tag means UNKNOWN. Nulling a
// previously-known count destroys information the mirror already had.
func TestSQLSinkDoesNotNullKnownBookmarksOnARecrawl(t *testing.T) {
	db := newMirrorDB(t)
	s := NewSQLSink(db)

	withBkmk := fixtureWork()
	if err := s.Save(context.Background(), withBkmk); err != nil {
		t.Fatal(err)
	}

	without := fixtureWork()
	without.Bookmarks = nil // the page had no meta tag
	if err := s.Save(context.Background(), without); err != nil {
		t.Fatal(err)
	}

	var got sql.NullInt64
	if err := db.QueryRow(`SELECT bookmarks FROM works WHERE id=4242`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != 42 {
		t.Errorf("bookmarks = %v, want the previously-known 42", got)
	}
}

// But a work whose FIRST crawl had no bookmarks must stay NULL, not become 0.
// The corpus column is NULLable and a later sort that treats NULL as 0 ranks
// the emptiest works highest.
//
// This test is the ONLY thing keeping that true. The live mirror measured 0 NULLs
// in 112,935 rows on 2026-10-05 -- it had 112,890 the day before, and was
// rewritten with zeros -- so nothing in the real data exercises this path any
// more. A test that guards a behaviour no fixture reaches is decoration; this
// one writes the NULL itself.
func TestSQLSinkKeepsNullBookmarksNull(t *testing.T) {
	db := newMirrorDB(t)
	s := NewSQLSink(db)
	w := fixtureWork()
	w.Bookmarks = nil
	if err := s.Save(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM works WHERE id=4242 AND bookmarks IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("NULL bookmarks was written as 0")
	}
}

func TestSQLSinkRejectsWorkWithoutAnID(t *testing.T) {
	db := newMirrorDB(t)
	s := NewSQLSink(db)
	// A row with no id cannot be joined to anything and is how a failed parse
	// becomes a permanent row nobody can find again.
	if err := s.Save(context.Background(), &ParsedWork{Title: "no id"}); err == nil {
		t.Error("saved a work with no id")
	}
	if err := s.Save(context.Background(), nil); err == nil {
		t.Error("saved a nil work")
	}
}

// One work's bad tags must not cost the others: 689 good works should not be
// lost because one page was malformed.
func TestSaveAllContinuesPastAFailure(t *testing.T) {
	db := newMirrorDB(t)
	s := NewSQLSink(db)

	good1 := fixtureWork()
	good2 := fixtureWork()
	good2.ID = 4243
	bad := fixtureWork()
	bad.ID = 0 // will be rejected

	saved, failed := SaveAll(context.Background(), s, []*ParsedWork{good1, bad, good2})
	if saved != 2 {
		t.Errorf("saved = %d, want 2", saved)
	}
	if len(failed) != 1 {
		t.Errorf("failed = %d, want 1", len(failed))
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM works WHERE id IN (4242,4243)`).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
}
