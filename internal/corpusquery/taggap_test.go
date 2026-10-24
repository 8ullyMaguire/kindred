package corpusquery

// TagGap (#34), on a corpus small enough to count by hand.
//
// The claims that matter are the ones that separate a gap analysis from a list of
// popular tags:
//
//  1. the evidence comes from works WITHOUT the seed (a candidate carried only by the
//     seed's own works has no support, or the number says nothing the seed did not
//     already say);
//  2. a tag the seed already carries at or above the corpus rate is clamped out, however
//     heavy its neighbourhood support;
//  3. the row's own/works/corpus numbers are the ones the ranking used;
//  4. a seed too small to have a distribution is refused by name rather than ranked.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// gapDB builds a corpus with a seed tag ("seed"), two neighbours ("n1", "n2"), a tag the
// neighbourhood wants and the seed lacks ("wanted"), a tag the seed already carries
// heavily ("covered"), and a tag carried ONLY by the seed's works ("inline").
//
// Counts, by construction:
//
//	seed's works:        40 (works 1..40), each carrying "seed" + one posted tag
//	wanted:               0 of the seed's works, 60 works elsewhere
//	covered:             30 of the seed's works (75%), 60 works elsewhere (75% of the
//	                     neighbourhood) -- so lift ~1.0 and no gap
//	inline:              20 of the seed's works, 0 elsewhere
//	n1/n2:               the neighbourhood tags, on every seed work plus 40 more each
//
// "wanted" is on 60 works that carry n1 or n2 and never "seed", which is exactly the
// second-hop shape the query is for.
func gapDB(t *testing.T, seedWorks int) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testcorpus.Schema); err != nil {
		t.Fatal(err)
	}

	// Tags.
	tags := []string{"seed", "n1", "n2", "wanted", "covered", "inline"}
	for i, name := range tags {
		if _, err := db.Exec(`INSERT INTO tags (id, name) VALUES (?, ?)`, i+1, name); err != nil {
			t.Fatal(err)
		}
	}
	const (
		tSeed    = 1
		tN1      = 2
		tN2      = 3
		tWanted  = 4
		tCovered = 5
		tInline  = 6
	)

	// Works 1..seedWorks are the seed's. 101..260 are the neighbourhood's.
	works := 260
	for i := 1; i <= works; i++ {
		if _, err := db.Exec(`INSERT INTO works (id, url, title, authors, update_date)
			VALUES (?, '', '', '', '2020-01-01')`, i); err != nil {
			t.Fatal(err)
		}
	}
	add := func(work, tag int) {
		if _, err := db.Exec(`INSERT OR IGNORE INTO work_tags (work_id, tag_id, tag_type)
			VALUES (?, ?, 'freeform')`, work, tag); err != nil {
			t.Fatal(err)
		}
	}
	for w := 1; w <= seedWorks; w++ {
		add(w, tSeed)
	}
	for w := 1; w <= seedWorks; w += 2 {
		add(w, tN1)
	}
	for w := 2; w <= seedWorks; w += 2 {
		add(w, tN2)
	}
	// covered: three quarters of the seed's works.
	for w := 1; w <= seedWorks*3/4; w++ {
		add(w, tCovered)
	}
	// inline: 12 of the seed's works, plus 400 works that carry NOTHING else.
	//
	// This shape is chosen so that ONLY the query's NOT EXISTS can explain its absence,
	// and it took two attempts to find one:
	//
	//   * 20 works, all the seed's -- excluded by tagGapMinCorpusWorks, so the mutant
	//     that deletes NOT EXISTS survived (nothing reached the support column).
	//   * 30 works inside the seed plus 40 outside -- then lift = 4.7, the clamp kills
	//     it, and the absence is explained by the clamp instead.
	//
	// With own = 12 of 40 (0.30) and a corpus rate of 412/840 (0.49), lift is 0.61: the
	// clamp does NOT apply, the floor does not apply, and the only remaining reason for
	// the row to be missing is that its support comes entirely from works that carry the
	// seed -- which is the thing being tested.
	for w := 1; w <= 12; w++ {
		add(w, tInline)
	}
	for w := 441; w <= 840; w++ {
		add(w, tInline)
	}

	// The neighbourhood's own works: 160 works, none carrying "seed".
	next := seedWorks + 1
	neighWorks := 60
	for i := 0; i < neighWorks; i++ {
		add(next, tN1)
		next++
	}
	for i := 0; i < neighWorks; i++ {
		add(next, tN2)
		next++
	}
	// wanted: 60 of the neighbourhood's works, spread over both neighbours.
	for i := 0; i < neighWorks; i++ {
		add(seedWorks+1+i, tWanted)
	}
	// covered: 60 of the neighbourhood's works too, so its corpus rate matches the
	// seed's and the clamp has something to clamp.
	for i := 0; i < neighWorks; i++ {
		add(seedWorks+1+neighWorks+i, tCovered)
	}
	// Every neighbourhood work also carries n1 or n2 (already true above for the ones we
	// just wrote, but be explicit about the second batch).
	for i := 0; i < neighWorks; i++ {
		add(seedWorks+1+neighWorks+i, tN2)
	}
	return db
}

func TestTagGapRanksWhatTheNeighbourhoodWantsAndTheSeedLacks(t *testing.T) {
	db := gapDB(t, 40)
	res, err := NewRunner(db).TagGap(context.Background(), "seed", Options{Limit: 10, MinCoWorks: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("no gaps found on a corpus with one: %v", res.Notes)
	}
	top := res.Rows[0]
	if top.Key != "wanted" {
		var got []string
		for _, r := range res.Rows {
			got = append(got, r.Key)
		}
		t.Fatalf("top gap is %q, want \"wanted\" (rows: %v; notes: %v)", top.Key, got, res.Notes)
	}
	// The row's own numbers must be the ones the ranking used: none of the seed's works
	// carry it, and lift is 0.
	if top.CoWorks != 0 {
		t.Fatalf("\"wanted\" reports %d of the seed's works carrying it, want 0", top.CoWorks)
	}
	if top.Lift != 0 {
		t.Fatalf("\"wanted\" lift is %v, want 0 (the seed never carries it)", top.Lift)
	}
	if top.Works == 0 {
		t.Fatal("\"wanted\" reports no corpus works: the row could not be checked by hand")
	}
}

// "covered" is carried by three quarters of the seed's works and three quarters of the
// neighbourhood's: lift ~1.0, so it is already covered and must not be a gap, even though
// its support is as heavy as "wanted"'s.
func TestTagGapClampsWhatTheSeedAlreadyCarries(t *testing.T) {
	db := gapDB(t, 40)
	// A LIMIT large enough that truncation cannot hide a bad row: with Limit 20 a
	// negative-gap row sorted last and was cut, and the mutant that removed the
	// exclusion SURVIVED. Every row is also checked for a positive score, which is the
	// property the list is supposed to have.
	res, err := NewRunner(db).TagGap(context.Background(), "seed", Options{Limit: 50, MinCoWorks: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		if r.Score <= 0 {
			t.Fatalf("row %q is in a gap list with score %v: a tag the seed already "+
				"covers as often as the corpus does has no gap", r.Key, r.Score)
		}
		if r.Key == "covered" {
			t.Fatalf("a tag the seed carries at %.2f of the corpus rate is listed as a "+
				"gap (lift %v, score %v): the clamp is not applied", r.Lift, r.Lift, r.Score)
		}
		if r.Key == "inline" {
			t.Fatalf("\"inline\" is listed as a gap, but only the seed's own works carry "+
				"it: the support is coming from inside the set being described (support %d)",
				r.CoWorks)
		}
	}
}

// A seed with too few works is refused by name: with a handful of works every lift is 0
// or 1 and the ranking is noise with two decimal places.
func TestTagGapRefusesASeedTooSmallToDescribe(t *testing.T) {
	db := gapDB(t, 5)
	res, err := NewRunner(db).TagGap(context.Background(), "seed", Options{Limit: 10, MinCoWorks: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("a five-work seed produced %d rows", len(res.Rows))
	}
	found := false
	for _, n := range res.Notes {
		if len(n) > 0 && contains(n, "too few works") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal does not say why: %v", res.Notes)
	}
}

// An unknown tag is a named absence, not an empty ranking.
func TestTagGapNamesAnUnknownTag(t *testing.T) {
	db := gapDB(t, 40)
	res, err := NewRunner(db).TagGap(context.Background(), "nosuchtag", Options{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 || len(res.Notes) == 0 {
		t.Fatalf("unknown tag gave %d rows and %d notes", len(res.Rows), len(res.Notes))
	}
	// And an empty tag name is an error, not an empty result.
	if _, err := NewRunner(db).TagGap(context.Background(), "  ", Options{}); err == nil {
		t.Fatal("an empty tag was accepted")
	}
}
