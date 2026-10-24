package corpusquery

// TagGap (#34), on a corpus small enough to count by hand.
//
// WHY THE FIXTURES ARE SHAPED THE WAY THEY ARE. Two of the three properties here were
// passing VACUOUSLY in the first two versions, and both times the cause was the same: the
// neighbourhood is the seed's top THIRTY co-occurring tags, and a fixture with four tags
// put every candidate inside the neighbourhood, where the candidate query then excluded it
// by `c.tag_id NOT IN (neighbours)`.
//
//	* "covered" (a tag the seed carries heavily) co-occurs with the seed by definition, so
//	  with four tags it WAS a neighbour and never reached the lift test. The test passed
//	  and proved nothing.
//	* "insider" was a neighbour too, so it never reached the support column, and the
//	  mutant that deletes the NOT EXISTS survived.
//
// Both fixtures therefore carry thirty FILLER tags that co-occur with the seed on every
// one of its works, which fills the neighbourhood and pushes the tags under test into the
// candidate set where they belong. That is also what the real mirror looks like: a tag's
// neighbourhood is full of rating, warning and pairing tags before it gets to anything
// interesting.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

const (
	gapFillers    = 30   // tags that fill the neighbourhood
	gapFillerBase = 1000 // filler tag ids start here
)

// addFillers gives every work in seedWorks these 30 tags, so the neighbourhood is full.
func addFillers(t *testing.T, db *sql.DB, seedWorks []int) {
	t.Helper()
	for i := 0; i < gapFillers; i++ {
		id := gapFillerBase + i
		if _, err := db.Exec(`INSERT INTO tags (id, name) VALUES (?, ?)`,
			id, fmt.Sprintf("filler-%02d", i)); err != nil {
			t.Fatal(err)
		}
		for _, w := range seedWorks {
			if _, err := db.Exec(`INSERT OR IGNORE INTO work_tags (work_id, tag_id, tag_type)
				VALUES (?, ?, 'freeform')`, w, id); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Main fixture arithmetic, all of it checkable against the inserts below.
//
//	works            1000 total
//	seed's works      100 (1..100): "seed" + all 30 fillers
//	hood works        400 (101..500): the 30 fillers
//	outside works     400 (501..900): "inline" only
//	tagless           100 (901..1000)
//
//	neighbourhood = the 30 fillers (co-occurrence 100 each); everything below ranks 31st
//	                or lower and is therefore a CANDIDATE
//	neighWorks    = 400
//
//	wanted:  0 of the seed's works, 300 of the hood's. Neighbourhood rate 0.75 against a
//	         corpus rate of 0.30 -> enrichment 2.5; lift 0 -> the clear top gap.
//	covered: 50 of the seed's works (0.50) and 200 of the hood's (0.50 against a corpus
//	         rate of 0.25 -> enrichment 2.0, so enrichment passes it). Lift 2.0: the seed
//	         carries it MORE than the archive, so it is excluded by the lift and by
//	         nothing else.
//	inline:  10 of the seed's works and 400 works outside the neighbourhood, none inside
//	         it: honest support 0. Whether a mutant that drops the NOT EXISTS would be
//	         caught here is arithmetic -- 10 of 400 is a rate too small to clear
//	         enrichment -- which is what the second, smaller fixture is for.
func TestTagGapRanksWhatTheNeighbourhoodWantsAndTheSeedLacks(t *testing.T) {
	db := gapDB(t)
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
	if top.CoWorks != 0 {
		t.Fatalf("\"wanted\" reports %d of the seed's works carrying it, want 0", top.CoWorks)
	}
	if top.Lift != 0 {
		t.Fatalf("\"wanted\" lift is %v, want 0 (the seed never carries it)", top.Lift)
	}
	if top.Score <= 0 {
		t.Fatalf("\"wanted\" scores %v", top.Score)
	}
}

// "covered" passes the enrichment test (the neighbourhood carries it twice as often as the
// archive) and must still be excluded, because the seed already carries it more than the
// archive does. This is the lift test, isolated.
func TestTagGapDropsWhatTheSeedAlreadyCovers(t *testing.T) {
	db := gapDB(t)
	// A LIMIT large enough that truncation cannot hide a bad row: with Limit 20 a
	// negative-gap row sorted last and was cut, and the mutant that removed the exclusion
	// SURVIVED.
	res, err := NewRunner(db).TagGap(context.Background(), "seed", Options{Limit: 50, MinCoWorks: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		if r.Score <= 0 {
			t.Fatalf("row %q is in a gap list with score %v: only positive gaps belong "+
				"in one", r.Key, r.Score)
		}
		if r.Key == "familiar" {
			t.Fatalf("\"familiar\" is listed with score %v: the seed covers it %.2fx the "+
				"corpus rate AND the neighbourhood under-carries it, so both factors of the "+
				"score are negative and their product is positive -- the shape that shipped "+
				"live as \"2.94x the corpus rate\" in a gap list", r.Key, r.Score)
		}
		if r.Key == "covered" {
			t.Fatalf("\"covered\" is listed with lift %v: the seed carries it %.2fx the "+
				"corpus rate, so it has no gap", r.Lift, r.Lift)
		}
	}
}

// A tag the NEIGHBOURHOOD does not over-carry is not a gap either, however rare it is in
// the seed. This is the test the first live run needed: raw support ranked the archive's
// sex tags at the top of every seed's list, identically.
func TestTagGapRequiresTheNeighbourhoodToOverCarry(t *testing.T) {
	db := gapDB(t)
	res, err := NewRunner(db).TagGap(context.Background(), "seed", Options{Limit: 50, MinCoWorks: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		for _, bad := range []string{"inline", "spread"} {
			if r.Key == bad {
				t.Fatalf("%q is listed with score %v although the neighbourhood carries it "+
					"LESS often than the archive does: a tag outside the genre is not a "+
					"gap in this tag, however rare it is in the seed", bad, r.Score)
			}
		}
	}
}

// BOTH factors negative is not "no gap": their product is POSITIVE, and a score computed
// as one product accepts it. This is the shape that shipped live -- hurt/comfort's top row
// was "implied/referenced child abuse, 2.94x the corpus rate", a tag its own works carry
// nearly three times more often than the archive, listed as something the tag LACKS.
//
// Its own test rather than a line in the one above, because that one stops at the first
// bad row and "spread" (a single negative factor) is caught before this case is reached.
func TestTagGapRefusesATagWhoseBothFactorsAreNegative(t *testing.T) {
	db := gapDB(t)
	res, err := NewRunner(db).TagGap(context.Background(), "seed", Options{Limit: 50, MinCoWorks: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		if r.Key == "familiar" {
			t.Fatalf("\"familiar\" is listed with score %v and lift %.2f: the seed already "+
				"covers it and the neighbourhood under-carries it, so both factors of the "+
				"score are negative and their product is positive", r.Score, r.Lift)
		}
	}
}

// A seed with too few works is refused by name: with a handful of works every lift is 0 or
// 1 and the ranking is noise with two decimal places.
func TestTagGapRefusesASeedTooSmallToDescribe(t *testing.T) {
	db := gapDB(t)
	if _, err := db.Exec(`INSERT INTO tags (id, name) VALUES (99, 'tiny')`); err != nil {
		t.Fatal(err)
	}
	for w := 1; w <= 5; w++ {
		if _, err := db.Exec(`INSERT OR IGNORE INTO work_tags (work_id, tag_id, tag_type)
			VALUES (?, 99, 'freeform')`, w); err != nil {
			t.Fatal(err)
		}
	}
	res, err := NewRunner(db).TagGap(context.Background(), "tiny", Options{Limit: 10, MinCoWorks: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("a five-work seed produced %d rows", len(res.Rows))
	}
	found := false
	for _, n := range res.Notes {
		if contains(n, "too few works") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal does not say why: %v", res.Notes)
	}
}

// An unknown tag is a named absence, not an empty ranking.
func TestTagGapNamesAnUnknownTag(t *testing.T) {
	db := gapDB(t)
	res, err := NewRunner(db).TagGap(context.Background(), "nosuchtag", Options{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 || len(res.Notes) == 0 {
		t.Fatalf("unknown tag gave %d rows and %d notes", len(res.Rows), len(res.Notes))
	}
	if _, err := NewRunner(db).TagGap(context.Background(), "  ", Options{}); err == nil {
		t.Fatal("an empty tag was accepted")
	}
}

// The NOT EXISTS is the load-bearing clause of the second hop, and this corpus is built so
// that deleting it changes the answer:
//
//	works 260: the seed's 40 (1..40, "seed" + 30 fillers), the hood 20 (41..60, one filler
//	each), others 200 (61..260)
//	insider: 12 of the SEED's works and 100 of the others; no hood work carries it
//	own rate 0.30 against a corpus rate of 112/260 = 0.43 -> lift 0.70, so the lift test
//	passes it and cannot be what keeps it out
//	honest support 0 -> not a candidate
//	mutant support 12 of 20 neighbourhood works = 0.60 against 0.43 -> enriched, so the
//	row appears
//
// The small numbers are the point: in the main fixture, 12 of 400 neighbourhood works is a
// rate too small to clear enrichment, so a mutant would survive there for the wrong
// reason.
func TestTagGapIgnoresSupportFromInsideTheSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testcorpus.Schema); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"seed", "n1", "insider"} {
		if _, err := db.Exec(`INSERT INTO tags (id, name) VALUES (?, ?)`, i+1, name); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 260; i++ {
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
	seedWorks := make([]int, 0, 40)
	for w := 1; w <= 40; w++ {
		add(w, 1) // seed
		seedWorks = append(seedWorks, w)
	}
	addFillers(t, db, seedWorks)
	for w := 1; w <= 12; w++ {
		add(w, 3) // insider, inside the seed
	}
	for w := 41; w <= 60; w++ {
		// The neighbourhood: one filler each, so neighWorks is 20 and a support of 12 is
		// a rate large enough for the mutant to clear enrichment.
		add(w, gapFillerBase)
	}
	for w := 61; w <= 160; w++ {
		add(w, 3) // insider elsewhere, nowhere near the neighbourhood
	}

	res, err := NewRunner(db).TagGap(context.Background(), "seed", Options{
		Limit: 50, MinCoWorks: 5,
		// The floor is what keeps this fixture small; the hop is what is under test.
		MinCorpusWorks: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		if r.Key == "insider" {
			t.Fatalf("\"insider\" is listed as a gap with support %d: the only works "+
				"carrying both it and a neighbourhood tag are the seed's OWN, so the "+
				"evidence came from inside the set being described", r.CoWorks)
		}
	}
}

// gapDB builds the main fixture. Split out so the arithmetic above reads next to the code
// that makes it true.
func gapDB(t *testing.T) *sql.DB {
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

	for i, name := range []string{"seed", "wanted", "covered", "inline", "spread", "familiar"} {
		if _, err := db.Exec(`INSERT INTO tags (id, name) VALUES (?, ?)`, i+1, name); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 1000; i++ {
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
	const (
		tSeed     = 1
		tWanted   = 2
		tCovered  = 3
		tInline   = 4
		tSpread   = 5
		tFamiliar = 6
	)
	seedWorks := make([]int, 0, 100)
	for w := 1; w <= 100; w++ {
		add(w, tSeed)
		seedWorks = append(seedWorks, w)
	}
	addFillers(t, db, seedWorks)
	// The neighbourhood's own works carry the fillers too: that is what makes them the
	// genre around the tag rather than tags that merely appear beside it.
	for w := 101; w <= 500; w++ {
		add(w, gapFillerBase)
	}
	// wanted: 300 of the neighbourhood's works, none of the seed's.
	for w := 101; w <= 400; w++ {
		add(w, tWanted)
	}
	// covered: 50 of the seed's works and 200 of the neighbourhood's.
	for w := 51; w <= 100; w++ {
		add(w, tCovered)
	}
	for w := 101; w <= 300; w++ {
		add(w, tCovered)
	}
	// inline: 10 of the seed's works and 400 works that carry nothing else.
	for w := 1; w <= 10; w++ {
		add(w, tInline)
	}
	for w := 501; w <= 900; w++ {
		add(w, tInline)
	}
	// spread: 100 of the neighbourhood's works (rate 0.25) and 400 works outside it
	// (corpus rate 0.50). It has real support and the neighbourhood UNDER-carries it, so
	// it must be excluded by the enrichment factor and by nothing else -- the gap is
	// negative, and dropping enrichment from the score would promote it.
	for w := 101; w <= 200; w++ {
		add(w, tSpread)
	}
	for w := 501; w <= 900; w++ {
		add(w, tSpread)
	}
	for w := 501; w <= 960; w++ {
		add(w, tFamiliar)
	}
	// familiar: the case where BOTH factors are on the wrong side, so their product is
	// positive and a single sign check would accept it. 70 of the seed's works and 60 of
	// the neighbourhood's, 460 more outside: corpus 590 of 1000 -> the seed carries it at
	// 0.70 against 0.59 (lift 1.19, already covered) and the neighbourhood at 0.15 against
	// 0.59 (enrichment 0.25, under-carried).
	//
	// The 60 hood works matter twice: the candidate query's floor is 50 SUPPORT, so 40 --
	// the first version of this fixture -- meant familiar was never a candidate and the
	// test passed without ever exercising the guards.
	for w := 1; w <= 70; w++ {
		add(w, tFamiliar)
	}
	for w := 101; w <= 160; w++ {
		add(w, tFamiliar)
	}
	return db
}
