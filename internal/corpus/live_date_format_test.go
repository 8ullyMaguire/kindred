package corpus

// A LIVE check on the production date format, because the defect it guards was
// invisible to every hermetic test: the mirror stores works.update_date as
// AO3's display date ("17 Sep 2008"), the parser accepted only ISO layouts, and
// the shared fixture wrote ISO too. So the suite was green while every real
// work parsed as undated and the recency signal skipped the entire corpus.
//
// Skipped unless KINDRED_LIVE_CORPUS names the mirror, so the hermetic suite
// does not depend on a 1.7 GB file. Sampling is by id range: three contiguous
// windows rather than a full scan, which costs seconds instead of minutes.

import (
	"database/sql"
	"os"
	"testing"

	_ "modernc.org/sqlite"
)

func TestLiveUpdateDatesParseToEpochDays(t *testing.T) {
	path := os.Getenv("KINDRED_LIVE_CORPUS")
	if path == "" {
		t.Skip("KINDRED_LIVE_CORPUS not set")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var checked, bad int
	var first string
	for _, lo := range []int64{1, 50_000, 500_000} {
		rows, err := db.Query(`SELECT update_date FROM works
			WHERE id BETWEEN ? AND ? AND update_date IS NOT NULL AND update_date != ''
			LIMIT 50`, lo, lo+20_000)
		if err != nil {
			t.Fatalf("query from %d: %v", lo, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			checked++
			if parseDate(s) <= 0 {
				bad++
				if first == "" {
					first = s
				}
			}
		}
		rows.Close()
	}
	if checked == 0 {
		t.Fatal("sampled no dated works; the mirror or the id ranges are wrong")
	}
	if bad > 0 {
		t.Fatalf("%d of %d sampled update_date values did not parse (first %q); "+
			"an unparsed date is 0, which the recency signal reads as 'undated' and skips",
			bad, checked, first)
	}
	t.Logf("%d sampled update_date values parsed to epoch days", checked)
}
