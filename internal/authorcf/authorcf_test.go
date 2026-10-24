package authorcf

// The co-readership index and the holdout measurement, on a corpus small enough to count by
// hand.
//
// The properties worth testing are the ones that decide whether the measurement is honest:
// readers are PEOPLE (a reader with six of the seed's works is one reader), a candidate the
// evidence never named cannot be scored, and truth comes only from the HELD-OUT readers --
// an experiment that scores against its own evidence proves nothing.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// fixture builds a corpus with four authors and readers whose appetites are known.
//
//	author A: works 1..3, readers 1..10
//	author B: works 4..6, readers 1..8   (heavy overlap with A)
//	author C: works 7..9, readers 9..12  (light overlap: only readers 9,10)
//	author D: works 10..12, readers 13..15 (no overlap)
func fixture(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
CREATE TABLE works(id INTEGER PRIMARY KEY, url TEXT NOT NULL, title TEXT NOT NULL,
  authors TEXT NOT NULL, summary TEXT, rating TEXT, word_count INTEGER DEFAULT 0,
  hits INTEGER DEFAULT 0, kudos INTEGER DEFAULT 0, bookmarks INTEGER, chapters TEXT,
  language TEXT, complete INTEGER DEFAULT 0, update_date TEXT, first_seen TEXT,
  last_updated TEXT);
CREATE TABLE user_work_interactions(user_id INTEGER NOT NULL, work_id INTEGER NOT NULL,
  interaction_type TEXT NOT NULL DEFAULT 'bookmarked', seed_work_id INTEGER, timestamp TEXT);` + "\n" +
		// works, three each, one byline each
		`INSERT INTO works (id, url, title, authors) VALUES
  (1,'','w1','Author A'),(2,'','w2','Author A'),(3,'','w3','Author A'),
  (4,'','w4','Author B'),(5,'','w5','Author B'),(6,'','w6','Author B'),
  (7,'','w7','Author C'),(8,'','w8','Author C'),(9,'','w9','Author C'),
  (10,'','w10','Author D'),(11,'','w11','Author D'),(12,'','w12','Author D');`)
	if err != nil {
		t.Fatal(err)
	}
	bookmark := func(user, work int64) {
		if _, err := db.Exec(`INSERT INTO user_work_interactions (user_id, work_id, interaction_type, timestamp)
			VALUES (?, ?, 'bookmarked', '2024-01-01')`, user, work); err != nil {
			t.Fatal(err)
		}
	}
	for u := int64(1); u <= 10; u++ {
		bookmark(u, 1)
	}
	for u := int64(1); u <= 8; u++ {
		bookmark(u, 4)
	}
	for u := int64(9); u <= 12; u++ {
		bookmark(u, 7)
	}
	for u := int64(13); u <= 15; u++ {
		bookmark(u, 10)
	}
	return db
}

func TestIndexCountsReadersNotWorks(t *testing.T) {
	idx, err := Build(context.Background(), fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	// A reader who bookmarked THREE of author A's works is one reader of A.
	if got := idx.Readers("Author A"); got != 10 {
		t.Fatalf("author A has %d readers, want 10", got)
	}
	pairs := idx.CoReaders("Author A", 10)
	if len(pairs) == 0 {
		t.Fatal("no co-readers of author A")
	}
	if pairs[0].Key != Key("Author B") || pairs[0].Readers != 8 {
		t.Fatalf("top co-reader is %q with %d readers, want Author B with 8",
			pairs[0].Author, pairs[0].Readers)
	}
	// C shares exactly readers 9 and 10.
	var cReaders int
	for _, p := range pairs {
		if p.Key == Key("Author C") {
			cReaders = p.Readers
		}
	}
	if cReaders != 2 {
		t.Fatalf("author C shares %d readers with A, want 2", cReaders)
	}
	// D shares none, so it must not appear at all -- not appear with zero.
	for _, p := range pairs {
		if p.Key == Key("Author D") {
			t.Fatalf("author D is listed with %d shared readers, want absent", p.Readers)
		}
	}
}

func TestCoReadersFromVotesOncePerReader(t *testing.T) {
	db := fixture(t)
	// Reader 1 reads BOTH seeds and C, which is the case the dedupe exists for. Without a
	// reader who spans both seeds, a per-seed counter and a per-reader set agree and the test
	// passes with the dedupe deleted -- it did, until this row was added.
	if _, err := db.Exec(`INSERT INTO user_work_interactions (user_id, work_id, interaction_type, timestamp)
		VALUES (1, 7, 'bookmarked', '2024-01-01')`); err != nil {
		t.Fatal(err)
	}
	idx, err := Build(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	// Reader 1 read A, B and C; readers 9 and 10 read A and C. Starting from [A, B], C must count reader 9 ONCE (9 read
	// both) rather than twice, and the two seeds themselves must not be candidates.
	pairs := idx.CoReadersFrom([]string{"Author A", "Author B"}, 10)
	var c Pair
	for _, p := range pairs {
		if p.Key == Key("Author C") {
			c = p
		}
		if p.Key == Key("Author A") || p.Key == Key("Author B") {
			t.Fatalf("%q is listed as its own co-reader", p.Author)
		}
	}
	// Readers of A (1..10) or B (1..8): 1..10. Of those, who read C? 1, 9, 10 -> 3 DISTINCT
	// people. A counter that increments per seed occurrence answers 4, because reader 1 is
	// reached through A and again through B.
	if c.Readers != 3 {
		t.Fatalf("C counts %d readers from [A,B], want 3 (reader 1 read both seeds)", c.Readers)
	}
}

// The measurement must score only against HELD-OUT readers. A proxy that returns nothing
// must score zero and report that it ranked nothing -- the shape that stops "the proxy lost"
// from being an artefact of an error.
func TestMeasureScoresAgainstHeldOutReadersOnly(t *testing.T) {
	idx, err := Build(context.Background(), fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	// A proxy that knows nothing.
	blind := func(string, int) ([]string, error) { return nil, nil }
	params := Params{Seeds: 4, HoldoutPercent: 20, Candidates: 50, K: 5, MinReaders: 5}
	rep := Measure(idx, blind, params)
	if len(rep.Seeds) == 0 {
		t.Fatalf("nothing evaluated (skipped: %v)", rep.Skipped)
	}
	tot := rep.Totals()
	if tot.Truth == 0 {
		t.Fatalf("no truth pairs over %d seeds: the holdout is empty", tot.Seeds)
	}
	if tot.ProxyHits != 0 || tot.ProxyRanked != 0 {
		t.Fatalf("a blind proxy scored %d hits and ranked %d candidates", tot.ProxyHits, tot.ProxyRanked)
	}
	if tot.CFHits == 0 {
		t.Fatalf("co-readership found none of %d truth pairs on a fixture where it must", tot.Truth)
	}
}

// A proxy that returns the whole author list in a fixed order gets exactly the recall its
// order deserves, and ProxyRanked says how many candidates it could place.
func TestMeasureRanksOnlyWhatTheProxylReturns(t *testing.T) {
	idx, err := Build(context.Background(), fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	all := func(string, int) ([]string, error) {
		return []string{"Author A", "Author B", "Author C", "Author D"}, nil
	}
	params := Params{Seeds: 4, HoldoutPercent: 20, Candidates: 50, K: 2, MinReaders: 5}
	rep := Measure(idx, all, params)
	tot := rep.Totals()
	if tot.ProxyRanked == 0 {
		t.Fatal("the proxy ranked nothing although it returned every author")
	}
	if tot.ProxyHits == 0 {
		t.Fatalf("a proxy returning every author in a sensible order found 0 of %d truth pairs at k=2",
			tot.Truth)
	}
}

// The orphan account and Anonymous are bylines, not people: 1,889 and 1,650 works in the
// mirror, read by 1,585 distinct readers for the first. Every rule that counts readers
// includes them, so a list ranked by readers would open with orphan_account almost anywhere.
func TestNotAPersonIsNeverACandidate(t *testing.T) {
	db := fixture(t)
	// Author E is the orphan account, read by everyone.
	if _, err := db.Exec(`INSERT INTO works (id, url, title, authors) VALUES (13,'','w13','orphan_account')`); err != nil {
		t.Fatal(err)
	}
	for u := int64(1); u <= 10; u++ {
		if _, err := db.Exec(`INSERT INTO user_work_interactions (user_id, work_id, interaction_type, timestamp)
			VALUES (?, 13, 'bookmarked', '2024-01-01')`, u); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := Build(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range idx.CoReaders("Author A", 10) {
		if p.Key == Key("orphan_account") {
			t.Fatalf("the orphan account is offered as a co-reader with %d readers", p.Readers)
		}
	}
	for _, p := range idx.CoReadersFrom([]string{"Author A"}, 10) {
		if p.Key == Key("orphan_account") {
			t.Fatalf("the orphan account is offered by CoReadersFrom with %d readers", p.Readers)
		}
	}
	// As a seed it is allowed: "who reads the orphan account" is a fair question.
	if len(idx.CoReaders("orphan_account", 10)) == 0 {
		t.Fatal("the orphan account cannot be used as a seed")
	}
}
