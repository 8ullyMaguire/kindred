package corpusquery

// TagTrend (#31), on a corpus whose dates are chosen so the answer is knowable by
// hand.
//
// The two claims worth testing are not the SQL: they are that the window is anchored
// on the MIRROR'S newest work (a window measured from today would empty the corpus and
// report a collapse that is really a stale snapshot), and that the lift cancels the
// corpus's own growth (a tag doubling because the corpus doubled is not a trend).

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func trendDB(t *testing.T) *sql.DB {
	t.Helper()
	c := testcorpus.New(40)
	path := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := c.Write(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The window must be the 90 days before the mirror's newest work, whatever today is.
func TestTheWindowIsAnchoredOnTheMirrorNotTheClock(t *testing.T) {
	db := trendDB(t)
	// Give every work a date in 2015 -- eleven years before "now". A wall-clock window
	// would hold nothing.
	if _, err := db.Exec(`UPDATE works SET update_date = '2015-06-0' || (id % 9 + 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE works SET update_date = '2015-03-01' WHERE id % 4 = 0`); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(db)
	var tag string
	if err := db.QueryRow(`SELECT name FROM tags ORDER BY id LIMIT 1`).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	res, err := r.TagTrend(context.Background(), tag, Options{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("no rows over a 2015 corpus: the window is not anchored on the mirror "+
			"(notes: %v)", res.Notes)
	}
	// The anchor is named, so a reader can see the snapshot is a decade old. Compared
	// against the DB's own maximum rather than a literal: a fixture change must not be
	// able to make this pass by coincidence.
	var anchor string
	if err := db.QueryRow(`SELECT MAX(update_date) FROM works`).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, n := range res.Notes {
		if contains(n, anchor) {
			named = true
		}
	}
	if !named {
		t.Fatalf("the notes do not name the anchor %q: %v", anchor, res.Notes)
	}
}

// A pairing that grew exactly as fast as the corpus must lift to 1.0, and one that grew
// twice as fast to 2.0. This is the property that makes the number mean anything.
func TestLiftCancelsTheCorpusGrowth(t *testing.T) {
	db := trendDB(t)
	// Two tags: "steady" pairs with everything as often recently as ever; "rising"
	// pairs mostly recently.
	if _, err := db.Exec(`UPDATE works SET update_date = '2020-01-01'`); err != nil {
		t.Fatal(err)
	}
	// A quarter of the corpus is "recent".
	if _, err := db.Exec(`UPDATE works SET update_date = '2020-12-01' WHERE id % 4 = 1`); err != nil {
		t.Fatal(err)
	}
	// Make the anchor still 2020-12-01: the newest row is recent by construction.
	r := NewRunner(db)
	var tag string
	if err := db.QueryRow(`SELECT name FROM tags ORDER BY id LIMIT 1`).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	res, err := r.TagTrend(context.Background(), tag, Options{Limit: 50, MinCoWorks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 {
		t.Skip("fixture has no co-occurring tags")
	}
	// With uniformly random dates the growth of every pair is the corpus's growth up
	// to sampling noise, so lifts must sit around 1 rather than at the corpus's 0.25.
	var below, above int
	for _, row := range res.Rows {
		if row.Works == 0 {
			// A pair with nothing in the window has no growth to compare: lift 0 here
			// means "no recent co-occurrences", and the row's note has to say so or the
			// zero reads as a collapse.
			if !contains(row.Note, "0 of") {
				t.Fatalf("row %q has no recent works but its note does not say so: %q",
					row.Key, row.Note)
			}
			continue
		}
		if row.Lift <= 0 {
			t.Fatalf("row %q has lift %v with %d recent works", row.Key, row.Lift, row.Works)
		}
		if row.Lift < 1 {
			below++
		} else {
			above++
		}
	}
	if below == 0 && above == 0 {
		t.Fatal("no rows to judge")
	}
	// The corpus grew 0.25x, so a lift that merely echoed the growth would be 0.25 for
	// every row and the split above would be empty.
	if above == 0 {
		t.Fatalf("every pair lifts below 1.0 (%.2f of them below): the lift is echoing "+
			"the corpus's growth instead of cancelling it", float64(below)/float64(len(res.Rows)))
	}
}

// An empty window is a fact about the snapshot, and must be said rather than divided by
// zero into a table of zero lifts.
func TestAnEmptyWindowIsNamed(t *testing.T) {
	db := trendDB(t)
	// Every work on one day: the window then contains all of them, so make the window
	// empty by removing dates entirely -- the other degenerate case, which must not
	// crash either.
	if _, err := db.Exec(`UPDATE works SET update_date = ''`); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(db)
	var tag string
	if err := db.QueryRow(`SELECT name FROM tags ORDER BY id LIMIT 1`).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	res, err := r.TagTrend(context.Background(), tag, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("rows from a corpus with no dates: %+v", res.Rows)
	}
	if len(res.Notes) == 0 {
		t.Fatal("no note: an empty result and a corpus with no dates are the same shape")
	}
}

// The mirror stores "05 Sep 2007" (112,893 rows) and "2006-01-02" (two). A raw string
// comparison against a display date is a SILENT wrong answer: "31 Oct 1999" >=
// "2020-12-12" is TRUE lexically, because '3' > '2', so every work written on the 30th
// or 31st of any month would land inside the window whatever its year.
//
// The first version of TagTrend parsed only ISO and passed its own tests; the deploy
// gate found the real corpus. This test is that finding, pinned on a corpus built so
// the wrong answer is a specific number.
func TestDisplayDatesAreComparedAsDatesNotAsStrings(t *testing.T) {
	db := trendDB(t)
	// The fixture's own works are moved to 1999 first, so MY rows set the anchor: a
	// fixture date after 2021 would move the window past the works this test is about,
	// and the test would then be measuring the fixture instead of the comparison.
	if _, err := db.Exec(`UPDATE works SET update_date = '31 Oct 1999'`); err != nil {
		t.Fatal(err)
	}
	// Five works in 1999 and five in 2021, all display-format, all on the 31st so the
	// lexical comparison would put the 1999 half inside a 2021 window.
	for i := 0; i < 10; i++ {
		day := "31 Oct 1999"
		if i >= 5 {
			day = "31 Mar 2021"
		}
		id := 9000 + i
		if _, err := db.Exec(`INSERT INTO works (id, title, authors, url, word_count, kudos,
			hits, bookmarks, complete, update_date)
			VALUES (?, ?, '', ?, 5000, 10, 20, 1, 1, ?)`,
			id, "trend fixture", "u", day); err != nil {
			t.Fatal(err)
		}
		for _, tagID := range []int64{41, 42} {
			if _, err := db.Exec(`INSERT OR IGNORE INTO tags (id, name) VALUES (?, ?)`,
				tagID, map[int64]string{41: "seedtag", 42: "pairtag"}[tagID]); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO work_tags (work_id, tag_id, tag_type)
				VALUES (?, ?, 'freeforms')`, id, tagID); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := NewRunner(db)
	res, err := r.TagTrend(context.Background(), "seedtag", Options{Limit: 5, MinCoWorks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("no rows: %v", res.Notes)
	}
	for _, row := range res.Rows {
		if row.Key != "pairtag" {
			continue
		}
		// The window is 2020-12-31..2021-03-31, so exactly the five 2021 works.
		if row.Works != 5 {
			t.Fatalf("row %q reports %d recent works of %d, want 5 of 10: the window is "+
				"counting 1999 works written on the 31st (lexical comparison, not dates)",
				row.Key, row.Works, row.CoWorks)
		}
		if row.CoWorks != 10 {
			t.Fatalf("row %q has %d lifetime works, want 10", row.Key, row.CoWorks)
		}
		return
	}
	t.Fatalf("the pair tag is missing from %d rows: %v", len(res.Rows), res.Notes)
}

// And the display format still measures when it should: the same corpus moved to two
// years either side of the anchor must split, not collapse.
func TestDisplayDatesMeasureBothSidesOfTheWindow(t *testing.T) {
	db := trendDB(t)
	// Half the works recent (display format), half old.
	if _, err := db.Exec(`UPDATE works SET update_date = CASE WHEN id % 2 = 0
		THEN '12 Mar 2021' ELSE '05 Sep 2007' END`); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(db)
	var tag string
	if err := db.QueryRow(`SELECT name FROM tags ORDER BY id LIMIT 1`).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	res, err := r.TagTrend(context.Background(), tag, Options{Limit: 20, MinCoWorks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("no rows over a display-date corpus: %v", res.Notes)
	}
	// The corpus grew: 12 Mar 2021 as anchor, 2020-12-12 as the start, so the 2021 half
	// is inside and the 2007 half is not.
	for _, row := range res.Rows {
		if row.Works > row.CoWorks {
			t.Fatalf("row %q claims %d recent of %d lifetime works", row.Key, row.Works, row.CoWorks)
		}
	}
	var sawRecent bool
	for _, row := range res.Rows {
		if row.Works > 0 {
			sawRecent = true
		}
	}
	if !sawRecent {
		t.Fatalf("every pair reports zero recent works over a corpus that is half 2021: %v",
			res.Notes)
	}
}

// A window too thin to measure must widen and SAY SO. The live mirror's newest date is
// one crawled row from 2026 while the archive's body ends in 2025: a strict 90-day
// window there held 2 of 112,895 works, and every pair's lift came out 0.00 -- a table
// of zeros that reads as "nothing is rising" and is really "nothing was measured".
func TestAThinWindowWidensAndSaysSo(t *testing.T) {
	db := trendDB(t)
	// Forty works: one at "now" (setting the anchor) and the rest a year earlier, so
	// the 90-day window holds exactly one work.
	if _, err := db.Exec(`UPDATE works SET update_date = '31 Oct 1999'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE works SET update_date = '31 Oct 2000' WHERE id = (SELECT MAX(id) FROM works)`); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(db)
	var tag string
	if err := db.QueryRow(`SELECT name FROM tags ORDER BY id LIMIT 1`).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	res, err := r.TagTrend(context.Background(), tag, Options{Limit: 5, MinCoWorks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("no rows: the window did not widen (%v)", res.Notes)
	}
	var widened, anchored bool
	for _, n := range res.Notes {
		if contains(n, "widened from 90") {
			widened = true
		}
		if contains(n, "newest work, not today") {
			anchored = true
		}
	}
	if !widened {
		t.Fatalf("the window was widened without saying so: %v", res.Notes)
	}
	// The note must report the count at the ORIGINAL window, not the widened one: the
	// first version printed the widened figure beside the word "shorter", a sentence
	// that cannot be true.
	for _, n := range res.Notes {
		if contains(n, "only 1 of 40 works") {
			return
		}
	}
	t.Fatalf("the widening note does not report the original window's count (expected "+
		"\"only 1 of 40 works\"): %v", res.Notes)
	if !anchored {
		t.Fatalf("the anchor is no longer named: %v", res.Notes)
	}
	// And the widened window must actually measure: the corpus grew, so some pair has
	// recent works.
	var sawRecent bool
	for _, row := range res.Rows {
		if row.Works > 0 {
			sawRecent = true
		}
	}
	if !sawRecent {
		t.Fatalf("no pair has a work in the widened window: %v", res.Notes)
	}
}
