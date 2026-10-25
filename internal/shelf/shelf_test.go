package shelf

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/harvest"
)

// build creates a small corpus with three fandoms and one reader.
//
//   - "dragon" works carry the reader's tags (gamer, explicit) and must rank first
//   - "flowers" works carry none of them and must rank last
//   - "dragon (manga)" is a VARIANT: its works are a subset of "dragon"'s, which is exactly the
//     shape AO3 produces when one fandom answers to several tag names
//
// The numbers are small on purpose: every assertion here is about an ordering or a difference,
// and a fixture that is large enough to hide a missing term would defeat that.
func build(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:shelf_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE works(id INTEGER PRIMARY KEY, url TEXT, title TEXT, authors TEXT, summary TEXT,
		   rating TEXT, word_count INTEGER, hits INTEGER, kudos INTEGER, bookmarks INTEGER,
		   chapters TEXT, language TEXT, complete INTEGER, update_date TEXT)`,
		`CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE work_tags(work_id INTEGER, tag_id INTEGER, tag_type TEXT,
		   PRIMARY KEY(work_id, tag_id, tag_type))`,
		`CREATE TABLE users(id INTEGER PRIMARY KEY, username TEXT)`,
		`CREATE TABLE user_work_interactions(user_id INTEGER, work_id INTEGER, interaction_type TEXT,
		   seed_work_id INTEGER, timestamp TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	tags := map[string]int{
		"dragon": 1, "dragon (manga)": 2, "flowers": 3,
		"gamer": 4, "explicit": 5, "smut": 6, "fluff": 7, "tragedy": 8,
		"dragon/hunter": 9, "dragon/rival": 10,
	}
	for name, id := range tags {
		if _, err := db.Exec(`INSERT INTO tags(id, name) VALUES (?, ?)`, id, name); err != nil {
			t.Fatal(err)
		}
	}

	type w struct {
		id     int64
		fandom string
		extra  []string
		kudos  int
	}
	works := []w{
		// The reader's own bookmarks: dragon works tagged gamer+explicit.
		{1, "dragon", []string{"gamer", "explicit"}, 3},
		{2, "dragon", []string{"gamer", "explicit", "smut"}, 1},
		{3, "dragon", []string{"gamer", "explicit"}, 5},
		// Near-duplicates of 3: same tags, so MMR must not return all three first.
		{4, "dragon", []string{"gamer", "explicit"}, 4},
		{5, "dragon", []string{"gamer", "explicit"}, 2},
		// A second cluster: relevant, but sharing three of four tags with the first. MMR can only
		// trade relevance for novelty when the two are COMPARABLE -- a work with no taste tag is
		// rejected by any lambda, and a fixture like that would prove nothing. These two score
		// ~92% of the first cluster's works, so the trade is real and the penalty decides.
		{6, "dragon", []string{"gamer", "explicit", "tragedy"}, 900},
		{7, "dragon", []string{"gamer", "explicit", "tragedy"}, 800},
		// Unrelated fandom.
		{8, "flowers", []string{"fluff"}, 5000},
		{9, "flowers", []string{"tragedy"}, 4000},
		{10, "flowers", []string{"fluff"}, 3000},
		// Tag-stuffed: the same two taste tags as work 3 plus twenty irrelevant ones. It must
		// score BELOW work 3, or the denominator is not doing its job.
		{12, "dragon", []string{"gamer", "explicit"}, 6},
		// The variant fandom shares works 1-3 with "dragon".
		{11, "dragon (manga)", []string{"gamer", "explicit"}, 2},
	}
	for _, x := range works {
		if _, err := db.Exec(`INSERT INTO works(id, url, title, authors, rating, word_count, kudos, complete)
			VALUES (?, ?, ?, 'A', 'Explicit', 1000, ?, 1)`,
			x.id, "https://archiveofourown.org/works/"+itoa(x.id), "Work "+itoa(x.id), x.kudos); err != nil {
			t.Fatal(err)
		}
		types := append([]string{x.fandom}, x.extra...)
		for _, name := range types {
			if _, err := db.Exec(`INSERT INTO work_tags(work_id, tag_id, tag_type) VALUES (?, ?, 'freeforms')`,
				x.id, tags[name]); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The variant fandom also lists works 1 and 2 under its own name, so its work set is a
	// subset of "dragon"'s and the variant test has something to fire on.
	for _, id := range []int64{1, 2} {
		if _, err := db.Exec(`INSERT INTO work_tags(work_id, tag_id, tag_type) VALUES (?, 2, 'freeforms')`,
			id); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.Exec(`INSERT INTO users(id, username) VALUES (7, 'reader')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2, 3} {
		if _, err := db.Exec(`INSERT INTO user_work_interactions(user_id, work_id, interaction_type)
			VALUES (7, ?, 'bookmarked')`, id); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestTagStuffingIsPunished(t *testing.T) {
	db := build(t)
	// Twenty irrelevant tags on work 12, inserted after the corpus is built.
	for i := 0; i < 20; i++ {
		name := "filler" + itoa(int64(i))
		var id int
		if err := db.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id); err != nil {
			if _, err := db.Exec(`INSERT INTO tags(name) VALUES (?)`, name); err != nil {
				t.Fatal(err)
			}
			db.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id)
		}
		if _, err := db.Exec(`INSERT INTO work_tags(work_id, tag_id, tag_type) VALUES (12, ?, 'freeforms')`, id); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Build(context.Background(), db, catalogue(), []int64{7},
		Options{Fandoms: 1, PerFandom: 40, MinFandomWorks: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var plain, stuffed float64
	for _, w := range res.Fandoms[0].Works {
		switch w.ID {
		case 3:
			plain = w.Score
		case 12:
			stuffed = w.Score
		}
	}
	if plain == 0 || stuffed == 0 {
		t.Fatalf("both works must be on the shelf to compare: plain=%.3f stuffed=%.3f", plain, stuffed)
	}
	// The two carry the SAME taste tags; only the twenty irrelevant ones differ. Equality here
	// means the denominator counts matches instead of the work's tags, which makes tag-stuffing
	// free -- exactly the behaviour the denominator exists to prevent.
	if stuffed >= plain {
		t.Fatalf("a tag-stuffed work scores %.3f against %.3f for the same tags without the stuffing", stuffed, plain)
	}
}

func itoa(n int64) string {
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

func catalogue() *harvest.Catalogue {
	return &harvest.Catalogue{
		Media: []string{"Anime *a* Manga"},
		Fandoms: []harvest.Fandom{
			{Name: "dragon", Path: "dragon", Media: "Anime *a* Manga", Works: 900},
			{Name: "dragon (manga)", Path: "dragon-(manga)", Media: "Anime *a* Manga", Works: 400},
			{Name: "flowers", Path: "flowers", Media: "Anime *a* Manga", Works: 800},
		},
	}
}

// TestBuildRanksTheReadersFandomFirst is the whole point: the fandom whose works carry the
// reader's tags must outrank one whose works do not, whatever their AO3 sizes are.
func TestBuildRanksTheReadersFandomFirst(t *testing.T) {
	db := build(t)
	res, err := Build(context.Background(), db, catalogue(), []int64{7},
		Options{Fandoms: 3, PerFandom: 5, MinFandomWorks: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Fandoms) == 0 {
		t.Fatalf("no fandom at all: %+v", res.Notes)
	}
	if res.Fandoms[0].Name != "dragon" {
		t.Fatalf("rank 1 = %q, want dragon: %v", res.Fandoms[0].Name, names(res.Fandoms))
	}
	// A fandom whose works share NO tag with the reader is not "ranked last", it is not ranked:
	// there is no evidence to put a number on, and a shelf that lists it anyway is a popularity
	// list wearing the reader's name. The floor decides membership; overlap decides eligibility.
	for _, f := range res.Fandoms {
		if f.Name == "flowers" {
			t.Fatalf("a fandom with no shared tag was recommended: %v", names(res.Fandoms))
		}
	}
}

// TestVariantFandomIsDroppedAndSaysSo: "dragon (manga)" shares works with "dragon", so it must
// not take one of the reader's shelf slots -- and the drop must be reported, because a shelf that
// silently holds 99 fandoms when asked for 100 is a bug nobody can see.
func TestVariantFandomIsDroppedAndSaysSo(t *testing.T) {
	db := build(t)
	res, err := Build(context.Background(), db, catalogue(), []int64{7},
		Options{Fandoms: 3, PerFandom: 5, MinFandomWorks: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range res.Fandoms {
		if f.Name == "dragon (manga)" {
			t.Fatalf("the variant fandom kept a slot: %v", names(res.Fandoms))
		}
	}
	found := false
	for _, n := range res.Notes {
		if contains(n, "dragon (manga)") && contains(n, "variant") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the drop was not reported in Notes: %v", res.Notes)
	}
}

// TestNearDuplicateWorksAreExcluded: works 3, 4 and 5 carry IDENTICAL tags, so a page filled
// from the score list returns the same story three times. MMR alone does not prevent that -- its
// penalty only reorders works of comparable relevance -- so the pass drops a work whose tags are
// ~all shared with something already picked.
func TestNearDuplicateWorksAreExcluded(t *testing.T) {
	db := build(t)
	// PerFandom large enough to include every work, so the only thing deciding membership is the
	// duplicate rule: a smaller k would pass for the wrong reason.
	res, err := Build(context.Background(), db, catalogue(), []int64{7},
		Options{Fandoms: 1, PerFandom: 7, MinFandomWorks: 1, Lambda: 0.75})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Fandoms) == 0 {
		t.Fatal("no fandom")
	}
	ws := res.Fandoms[0].Works
	// 3, 4 and 5 are indistinguishable; at most one may appear.
	var identical int
	for _, w := range ws {
		if w.ID == 3 || w.ID == 4 || w.ID == 5 {
			identical++
		}
	}
	if identical > 1 {
		ids := make([]int64, len(ws))
		for i, w := range ws {
			ids[i] = w.ID
		}
		t.Fatalf("%d identical-tag works on one page (%v); the duplicate rule did not run", identical, ids)
	}
	// The shelf must still be FILLED: excluding duplicates that leaves five works is a different
	// bug, and a fixture with nothing else to offer would hide it.
	if len(ws) < 4 {
		t.Fatalf("only %d work(s) survived the duplicate rule; it is excluding too much", len(ws))
	}
}

// TestIsVariantDistinguishesASpellingFromACrossover pins the two shapes the rule exists to tell
// apart. It is a table rather than a corpus test because the interesting cases are arithmetic.
func TestIsVariantDistinguishesASpellingFromACrossover(t *testing.T) {
	set := func(ids ...int64) map[int64]struct{} {
		m := map[int64]struct{}{}
		for _, id := range ids {
			m[id] = struct{}{}
		}
		return m
	}
	cases := []struct {
		name     string
		a, b     Fandom
		sa, sb   map[int64]struct{}
		isVarian bool
	}{
		// Two spellings of one fandom: comparable sizes, half the works shared.
		{"dxd spelling", Fandom{Name: "a", MirrorWorks: 735}, Fandom{Name: "b", MirrorWorks: 359},
			set(1, 2, 3, 4, 5, 6), set(1, 2, 3, 7), true},
		// A small crossover-dependent fandom: fully contained, an order of magnitude smaller.
		{"elfen lied is not fate", Fandom{Name: "a", MirrorWorks: 800}, Fandom{Name: "b", MirrorWorks: 30},
			set(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), set(1, 2, 3), false},
		// Unrelated: no shared works at all.
		{"unrelated", Fandom{Name: "a", MirrorWorks: 500}, Fandom{Name: "b", MirrorWorks: 400},
			set(1, 2, 3), set(4, 5, 6), false},
	}
	for _, c := range cases {
		if got := isVariant(c.a, c.b, c.sa, c.sb); got != c.isVarian {
			t.Errorf("%s: isVariant = %v, want %v", c.name, got, c.isVarian)
		}
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func names(fs []Fandom) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}
