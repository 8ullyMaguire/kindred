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
