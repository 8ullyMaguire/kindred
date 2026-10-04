package testcorpus

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The fixture is only useful for testing the tag page's three new filters if
// those filters CAN return different results on it. This asserts the property
// directly, because it is the property the fixture exists to provide and it is
// silent when violated: a fixture where every work is "Explicit" and every work
// is complete makes `rating=E`, `lang=English` and `complete=true` all
// indistinguishable from an implementation that ignores the parameter.
//
// Before this change the INSERT hardcoded language="English" and complete=1
// while the struct had no such fields at all, so a filter test against it
// passed unconditionally.
func TestTheFixtureCanDistinguishTheFiltersItIsUsedFor(t *testing.T) {
	db := writeAndOpen(t)

	// mustMatch is a value the fixture must produce at least one row for; a
	// bucket with no rows makes a filter test assert emptiness, which an
	// implementation that ignores the parameter also does.
	// mustNotMatchAll is a clause that must NOT select every row, which is what
	// distinguishes a working filter from a missing one.
	type probe struct {
		clause string
		empty  bool // the mirror has no such value: zero rows is correct
	}
	all := countWorks(t, db, "")

	for _, p := range []probe{
		{clause: "rating = 'Explicit'"},
		{clause: "rating = 'General Audiences'"},
		{clause: "rating = 'Teen And Up Audiences'"},
		{clause: "rating = 'Mature'"},
		{clause: "complete = 1"},
		{clause: "complete = 0"},
		{clause: "language = 'English'"},
		{clause: "language = 'Spanish'"},
		{clause: "language = 'French'"},
		// Values the mirror does not contain must match nothing -- and are
		// exempt from the "at least one row" rule, which is the whole point
		// of listing them separately.
		{clause: "language = 'Klingon'", empty: true},
		{clause: "rating = 'Not Rated'", empty: true},
	} {
		got := countWorks(t, db, "WHERE "+p.clause)
		if p.empty {
			if got != 0 {
				t.Errorf("%q matched %d works, but the fixture must not produce it", p.clause, got)
			}
			continue
		}
		if got == 0 {
			t.Errorf("no fixture work satisfies %q, so a filter test on it can "+
				"only assert emptiness -- and would pass on an implementation "+
				"that ignores the parameter entirely", p.clause)
		}
		if got == all {
			t.Errorf("%q matches all %d works, so it cannot distinguish a "+
				"working filter from a missing one", p.clause, all)
		}
	}

	// Every rating the fixture claims to produce must actually be present, or
	// a rating filter test has a bucket that is always empty.
	for _, r := range []string{
		"General Audiences", "Teen And Up Audiences", "Mature", "Explicit",
	} {
		if n := countWorks(t, db, "WHERE rating = '"+r+"'"); n == 0 {
			t.Errorf("the fixture claims to produce rating %q but writes none", r)
		}
	}
	for _, l := range []string{"English", "Spanish", "French"} {
		if n := countWorks(t, db, "WHERE language = '"+l+"'"); n == 0 {
			t.Errorf("the fixture claims to produce language %q but writes none", l)
		}
	}
	// Both completion states, and neither a rounding error.
	if c0 := countWorks(t, db, "WHERE complete = 0"); c0 < 4 {
		t.Errorf("only %d in-progress works; a filter test could not distinguish "+
			"them from a dropped row", c0)
	}
	if c1 := countWorks(t, db, "WHERE complete = 1"); c1 < 4 {
		t.Errorf("only %d complete works", c1)
	}
}

func countWorks(t *testing.T, db *sql.DB, where string) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM works"
	if where != "" {
		q += " " + where
	}
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func writeAndOpen(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	c := New(40)
	c.Write(path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
