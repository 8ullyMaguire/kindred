package corpus_test

// A regression test with two sides, because the defect it guards had two
// halves that hid each other.
//
// The live mirror stores works.update_date as AO3's display date ("17 Sep
// 2008"). The parser accepted only ISO layouts, so EVERY real work produced
// update_date = 0; the recency signal reads 0 as "undated" and skips the
// candidate, so recency was inert on the whole corpus. No test caught it
// because the shared test fixture wrote "2026-01-15" — an ISO date the
// parser happily accepted. The fixture validated the wrong behaviour.
//
// So this test asserts the property that was false in production, using the
// SAME fixture the rest of the suite uses: a work loaded from the fixture
// carries a parsed update date. It fails if the parser drops the display
// layout again, and it fails if the fixture drifts back to a format the live
// corpus does not contain.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func TestAFixtureWorkCarriesAParsedUpdateDate(t *testing.T) {
	dir := t.TempDir()
	path, err := testcorpus.New(20).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var raw string
	if err := db.QueryRow(`SELECT update_date FROM works WHERE id = 1`).Scan(&raw); err != nil {
		t.Fatalf("update_date: %v", err)
	}

	// The FORMAT is asserted, not just parseability. The live mirror stores
	// AO3's display date and nothing else, so a fixture in any other format
	// stops exercising the parser path production actually uses -- which is
	// how this defect survived a green suite. An ISO fixture passes a
	// "does it parse" check while never testing the layout that matters.
	if _, err := time.Parse("2 Jan 2006", raw); err != nil {
		t.Fatalf("the fixture stores update_date %q, which is not the live mirror's format (%q); "+
			"a fixture in another format cannot catch a parser that rejects production dates",
			raw, "17 Sep 2008")
	}

	ent, err := corpus.NewAO3(db).Entity(context.Background(), 1)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	got := ent.Stats["update_date"]
	if got <= 0 {
		t.Fatalf("update_date %q parsed to %v (0 means 'undated', so the recency signal skips this work); the fixture must speak the mirror's format and the parser must accept it", raw, got)
	}
	// Epoch days, not seconds: a seconds value would be ~86400x too large and
	// would make every work look like the far future to the recency signal.
	if got > 100_000 {
		t.Errorf("update_date %q parsed to %v epoch days, which is implausible; the parser may be returning seconds", raw, got)
	}
}
