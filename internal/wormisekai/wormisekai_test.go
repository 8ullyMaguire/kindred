package wormisekai

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The fixture mirrors the real mirror's schema and column names. The mirror
// stores update_date as "DD Mon YYYY" (AO3's own display format), which the
// fixture reproduces -- a fixture with an ISO date would pass a parser that
// the real mirror breaks.
const fixtureSchema = `
CREATE TABLE works(
	id INTEGER PRIMARY KEY, title TEXT NOT NULL, url TEXT NOT NULL, summary TEXT,
	authors TEXT NOT NULL, word_count INTEGER DEFAULT 0, hits INTEGER DEFAULT 0,
	kudos INTEGER DEFAULT 0, bookmarks INTEGER, chapters TEXT, language TEXT,
	complete INTEGER DEFAULT 0, update_date TEXT, first_seen TEXT, rating TEXT);
CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL COLLATE NOCASE);
CREATE TABLE work_tags(work_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, tag_type TEXT NOT NULL,
	PRIMARY KEY(work_id, tag_id));
`

func newFixture(t *testing.T) *Runner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mirror.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(fixtureSchema); err != nil {
		t.Fatal(err)
	}
	return NewRunner(db)
}

func seed(t *testing.T, r *Runner, now time.Time) {
	t.Helper()
	addTag := func(name string) int64 {
		if _, err := r.DB.Exec(`INSERT OR IGNORE INTO tags(name) VALUES(?)`, name); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := r.DB.QueryRow(`SELECT id FROM tags WHERE name=?`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	addWork := func(id int64, title string, kudos int64, ageDays int) {
		ud := now.AddDate(0, 0, -ageDays).Format("2 Jan 2006")
		if _, err := r.DB.Exec(`INSERT INTO works(id,title,url,authors,kudos,word_count,rating,complete,update_date)
			VALUES(?,?,?,?,?,?,?,?,?)`,
			id, title, "https://ao3.example/"+title, "Author", kudos, 50000, "General Audiences", 1, ud); err != nil {
			t.Fatal(err)
		}
	}
	tagWork := func(workID, tagID int64, tagType string) {
		if _, err := r.DB.Exec(`INSERT OR IGNORE INTO work_tags(work_id,tag_id,tag_type) VALUES(?,?,?)`,
			workID, tagID, tagType); err != nil {
			t.Fatal(err)
		}
	}

	worm := addTag("worm - fandom")
	isekai := addTag("isekai")
	pony := addTag("pocket monsters | pokemon - all media types")
	altuni := addTag("alternate universe - canon divergence")
	starwars := addTag("star wars - all media types")
	addTag("reincarnation")
	addTag("crossover")

	// 1: recent worm + isekai. Should be returned as "inside".
	addWork(1, "RecentWormIsekai", 500, 10)
	tagWork(1, worm, "fandoms")
	tagWork(1, isekai, "freeforms")

	// 2: old worm + isekai (400 days). Must be filtered by the recency window.
	addWork(2, "OldWormIsekai", 9000, 400)
	tagWork(2, worm, "fandoms")
	tagWork(2, isekai, "freeforms")

	// 3: recent worm + isekai + a second fandom. Should be "into".
	addWork(3, "WormIntoPokemon", 300, 20)
	tagWork(3, worm, "fandoms")
	tagWork(3, isekai, "freeforms")
	tagWork(3, pony, "fandoms")

	// 4: recent worm, no concept tag, and only a freeform right-hand side.
	// "alternate universe - canon divergence" is NOT a fandom, so this is
	// neither "inside" nor "into".
	addWork(4, "WormAUOnly", 400, 15)
	tagWork(4, worm, "fandoms")
	tagWork(4, altuni, "freeforms")

	// 5: recent worm + a canon fandom, no concept tag. "into" but it is
	// older than 3 in kudos; it still belongs in "into".
	addWork(5, "WormIntoStarWars", 100, 30)
	tagWork(5, worm, "fandoms")
	tagWork(5, starwars, "fandoms")
}

// TestRunFiltersByRecency is the load-bearing test: the old work must be
// excluded even though it has more kudos than every recent work. If the
// recency filter is dropped, this test fails.
func TestRunFiltersByRecency(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	r := newFixture(t)
	seed(t, r, now)

	res, err := r.Run(context.Background(), Options{Now: now, RecentDays: 365, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range res.Items {
		if w.ID == 2 {
			t.Fatalf("work 2 is %d days old and must be excluded by the 365d window", w.AgeDays)
		}
	}
	// 1 (inside), 3 (into), 5 (into) qualify. 4 is neither.
	got := map[int64]bool{}
	for _, w := range res.Items {
		got[w.ID] = true
	}
	for _, id := range []int64{1, 3, 5} {
		if !got[id] {
			t.Errorf("work %d should be in the result, got %v", id, got)
		}
	}
	if got[4] {
		t.Errorf("work 4 has no concept tag and no other fandom; it must not be in the result")
	}
}

// TestDirections checks the inside/into classification.
func TestDirections(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	r := newFixture(t)
	seed(t, r, now)

	res, err := r.Run(context.Background(), Options{Now: now, RecentDays: 365, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	dir := map[int64]Direction{}
	fandom := map[int64]string{}
	for _, w := range res.Items {
		dir[w.ID] = w.Direction
		fandom[w.ID] = w.OtherFandom
	}
	if dir[1] != DirectionInside {
		t.Errorf("work 1 direction = %q, want %q", dir[1], DirectionInside)
	}
	if dir[3] != DirectionInto {
		t.Errorf("work 3 direction = %q, want %q", dir[3], DirectionInto)
	}
	if dir[5] != DirectionInto {
		t.Errorf("work 5 direction = %q, want %q", dir[5], DirectionInto)
	}
	if fandom[3] != "pocket monsters | pokemon" {
		t.Errorf("work 3 other fandom = %q, want %q", fandom[3], "pocket monsters | pokemon")
	}
}

// TestConceptTagsReported checks that the matched tags are reported.
func TestConceptTagsReported(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	r := newFixture(t)
	seed(t, r, now)

	res, err := r.Run(context.Background(), Options{Now: now, RecentDays: 365, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range res.Items {
		if w.ID == 1 && len(w.ConceptTags) == 0 {
			t.Errorf("work 1 must report the concept tag that matched")
		}
	}
}

// TestOtherFandom is the unit test for the fandom-shape rule.
//
// Every case here is a tag name measured on the live mirror, and the
// expected verdict was read off the measurement rather than reasoned about:
// the fandom rows are ones the measurement admitted, the rejected rows are
// the freeform namespaces and type markers it excluded.
func TestOtherFandom(t *testing.T) {
	cases := []struct {
		tag  string
		want string
		ok   bool
	}{
		// Admitted: real fandoms the measurement found on Worm works.
		{"pocket monsters | pokemon - all media types", "pocket monsters | pokemon", true},
		{"star wars - all media types", "star wars", true},
		{"harry potter - j. k. rowling", "harry potter", true},
		{"a song of ice and fire - george r. r. martin", "a song of ice and fire", true},
		{"the dresden files - jim butcher", "the dresden files", true},
		{"mass effect - all media types", "mass effect", true},
		{"celestial forge cyoa - lord roustabout", "celestial forge cyoa", true},
		{"cthulhu mythos - h. p. lovecraft", "cthulhu mythos", true},

		// Rejected: the Worm tag itself.
		{"worm - fandom", "", false},
		{"worm - wildbow", "", false},
		{"parahumans series - wildbow", "", false},

		// Rejected: freeform namespaces measured on Worm works.
		{"alternate universe - canon divergence", "", false},
		{"alternate universe - no powers", "", false},
		{"alternate universe - college/university", "", false},
		{"post-traumatic stress disorder - ptsd", "", false},
		{"abandoned work - unfinished and discontinued", "", false},
		{"not canon compliant - book 2: ward", "", false},

		// Rejected: type markers on the right-hand side.
		{"oc - character", "", false},
		{"si - character", "", false},
		{"x - freeform", "", false},
		{"celestial forge - fandom", "", false},

		// Rejected: no separator at all.
		{"", "", false},
		{"no separator here", "", false},
	}
	for _, c := range cases {
		got, ok := OtherFandom(c.tag)
		if ok != c.ok || got != c.want {
			t.Errorf("OtherFandom(%q) = (%q, %v), want (%q, %v)", c.tag, got, ok, c.want, c.ok)
		}
	}
}

// TestLimit checks the limit is applied.
func TestLimit(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	r := newFixture(t)
	seed(t, r, now)

	res, err := r.Run(context.Background(), Options{Now: now, RecentDays: 365, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(res.Items))
	}
	if res.Meta.Matched != 3 {
		t.Errorf("meta.Matched = %d, want 3 (the count before the limit)", res.Meta.Matched)
	}
	if res.Meta.Returned != 1 {
		t.Errorf("meta.Returned = %d, want 1", res.Meta.Returned)
	}
}

// TestSortedByScore checks the ranking is descending.
func TestSortedByScore(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	r := newFixture(t)
	seed(t, r, now)

	res, err := r.Run(context.Background(), Options{Now: now, RecentDays: 365, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(res.Items); i++ {
		if res.Items[i-1].Score < res.Items[i].Score {
			t.Fatalf("items not sorted by score: %v then %v",
				res.Items[i-1].Score, res.Items[i].Score)
		}
	}
}
