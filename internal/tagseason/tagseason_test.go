package tagseason

// The tests that matter here are the two the model can fail in opposite directions: it must NOT
// call a tag seasonal when its December count is merely a bigger December (volume), and it must
// not call a tag seasonal when it spiked in ONE year (consistency). A fixture where every month
// has the same number of works would make lift trivially 1 and hide the first failure.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// seasonFixture writes works with dates and tags: works[tagname] = list of "02 Jan 2006" dates,
// and monthVolume[month] = how many EXTRA works (tags "filler") were posted that month.
func seasonFixture(t *testing.T, dated map[string][]string, monthVolume map[int]int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "corpus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE works (id INTEGER PRIMARY KEY, title TEXT, update_date TEXT)`,
		`CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE work_tags (work_id INTEGER, tag_id INTEGER, tag_type TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	nextWork := int64(1)
	nextTag := int64(1)
	for name, dates := range dated {
		if _, err := tx.Exec(`INSERT INTO tags (id, name) VALUES (?, ?)`, nextTag, name); err != nil {
			t.Fatal(err)
		}
		tag := nextTag
		nextTag++
		for _, d := range dates {
			w := nextWork
			nextWork++
			if _, err := tx.Exec(`INSERT INTO works (id, title, update_date) VALUES (?, 'w', ?)`, w, d); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`INSERT INTO work_tags (work_id, tag_id, tag_type) VALUES (?, ?, 'freeforms')`, w, tag); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Filler works: they carry a filler tag and set the month's denominator.
	if _, err := tx.Exec(`INSERT INTO tags (id, name) VALUES (0, 'filler')`); err != nil {
		t.Fatal(err)
	}
	months := [...]string{"", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	for m, n := range monthVolume {
		for i := 0; i < n; i++ {
			w := nextWork
			nextWork++
			if _, err := tx.Exec(`INSERT INTO works (id, title, update_date) VALUES (?, 'w', ?)`,
				w, "02 "+months[m]+" 2021"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`INSERT INTO work_tags (work_id, tag_id, tag_type) VALUES (?, 0, 'freeforms')`, w); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// dates builds n works in each of the given months across the given years.
func dates(years []int, months []int, per int, day string) []string {
	name := [...]string{"", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	var out []string
	for _, y := range years {
		for _, m := range months {
			for i := 0; i < per; i++ {
				out = append(out, day+" "+name[m]+" "+itoa(y))
			}
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestLoadCountsCountsEachWorkOnce(t *testing.T) {
	db := seasonFixture(t, map[string][]string{
		"christmas": {"24 Dec 2021", "24 Dec 2021", "01 Jan 2022"},
	}, nil)
	// The same work carrying the same tag under two types must count once.
	if _, err := db.Exec(`INSERT INTO work_tags (work_id, tag_id, tag_type) VALUES (1, 1, 'fandoms')`); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCounts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	// Two distinct works carry "christmas" (the fixture made three rows, two dates identical but
	// different works).
	if got := c.ByMonth[1][12]; got != 2 {
		t.Fatalf("december count = %d, want 2 (the duplicated (work, tag) row must not count twice)", got)
	}
	if c.Total != 3 {
		t.Fatalf("total dated works = %d, want 3", c.Total)
	}
}

func TestAVolumeSpikeIsNotSeasonality(t *testing.T) {
	// December has five times the works of any other month, and "coffee shop au" is in 10% of
	// works EVERY month. Its raw December count is much larger; its lift is 1.0 and it is not
	// seasonal. A model that ranked by volume would name it.
	var coffee []string
	for m := 1; m <= 12; m++ {
		name := [...]string{"", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}[m]
		for i := 0; i < 5; i++ {
			coffee = append(coffee, "02 "+name+" 2021")
		}
	}
	volume := map[int]int{12: 500}
	for m := 1; m <= 11; m++ {
		volume[m] = 100
	}
	db := seasonFixture(t, map[string][]string{"coffee shop au": coffee}, volume)
	c, err := LoadCounts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	got := Build(c, Opts{MinSupport: 10, MinMonthWorks: 3, MinLift: 1.5, MinYearShare: 0.5, MinYearWorks: 2, MinYears: 1})
	for _, s := range got {
		if s.TagID == 1 {
			t.Fatalf("a tag spread evenly across months was called seasonal (%+v)", s)
		}
	}
}

func TestASingleYearSpikeIsNotSeasonality(t *testing.T) {
	// Three years, a DIFFERENT month each: December 2021, June 2019, October 2020. Every year has
	// a real peak and no two agree, so consistency is 1/3 and the tag is not seasonal. (The first
	// version of this fixture had June twice, which made "June" a correct answer -- the model was
	// right and the test was wrong.)
	var d []string
	d = append(d, dates([]int{2021}, []int{12}, 40, "02")...)
	d = append(d, dates([]int{2019}, []int{6}, 40, "02")...)
	d = append(d, dates([]int{2020}, []int{10}, 40, "02")...)
	volume := map[int]int{}
	for m := 1; m <= 12; m++ {
		volume[m] = 400
	}
	db := seasonFixture(t, map[string][]string{"one christmas": d}, volume)
	c, err := LoadCounts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	got := Build(c, Opts{MinSupport: 50, MinMonthWorks: 20, MinLift: 1.2, MinYearShare: 0.6, MinYearWorks: 10, MinYears: 1})
	for _, s := range got {
		if s.TagID == 1 {
			t.Fatalf("a one-year spike was called seasonal (%+v)", s)
		}
	}
}

// A tag that exists for one year only, entirely in December, is 100% "consistent" and must still
// be refused: it is a tag that arrived, not a season. Found on the live archive, where "project
// hail mary (2026)" was a May tag with 1/1 years agreeing.
func TestAOneYearTagIsNotSeasonal(t *testing.T) {
	var d []string
	d = append(d, dates([]int{2026}, []int{5}, 60, "02")...)
	volume := map[int]int{}
	for m := 1; m <= 12; m++ {
		volume[m] = 300
	}
	db := seasonFixture(t, map[string][]string{"a new film (2026)": d}, volume)
	c, err := LoadCounts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	got := Build(c, Opts{MinSupport: 50, MinMonthWorks: 20, MinLift: 1.2, MinYearShare: 0.6,
		MinYearWorks: 10, MinYears: 3})
	for _, s := range got {
		if s.TagID == 1 {
			t.Fatalf("a tag that exists in ONE year was called seasonal (%+v)", s)
		}
	}
}

func TestAConsistentDecemberTagIsFound(t *testing.T) {
	// Three years, December in each, nothing else: consistency 3/3.
	var d []string
	d = append(d, dates([]int{2019, 2020, 2021}, []int{12}, 30, "02")...)
	volume := map[int]int{}
	for m := 1; m <= 12; m++ {
		volume[m] = 300
	}
	db := seasonFixture(t, map[string][]string{"christmas": d}, volume)
	c, err := LoadCounts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	got := Build(c, Opts{MinSupport: 50, MinMonthWorks: 20, MinLift: 1.2, MinYearShare: 0.6, MinYearWorks: 10, MinYears: 1})
	var found *Season
	for i := range got {
		if got[i].TagID == 1 {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("a tag that appears only in December for three years was not found")
	}
	if found.Month != 12 {
		t.Fatalf("month = %d, want December", found.Month)
	}
	if found.YearShare != 1 {
		t.Fatalf("year share = %.2f, want 1.0", found.YearShare)
	}
	if found.Lift <= 1.2 {
		t.Fatalf("lift = %.2f, want above the threshold", found.Lift)
	}
}

func TestParseMonthAcceptsNamesAndNumbers(t *testing.T) {
	for in, want := range map[string]int{"12": 12, "dec": 12, "December": 12, "jan": 1, "october": 10} {
		got, err := ParseMonth(in)
		if err != nil {
			t.Fatalf("ParseMonth(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseMonth(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "13", "smarch", "0"} {
		if _, err := ParseMonth(bad); err == nil {
			t.Fatalf("ParseMonth(%q) returned no error", bad)
		}
	}
}

func TestParseAO3DateRejectsWhatItCannotRead(t *testing.T) {
	if _, _, ok := parseAO3Date("2020-01-02"); ok {
		t.Fatal("an ISO date was accepted")
	}
	if _, _, ok := parseAO3Date(""); ok {
		t.Fatal("an empty date was accepted")
	}
	y, m, ok := parseAO3Date("07 Dec 2019")
	if !ok || y != 2019 || m != 12 {
		t.Fatalf("parsed %d-%d ok=%v, want 2019-12", y, m, ok)
	}
}
