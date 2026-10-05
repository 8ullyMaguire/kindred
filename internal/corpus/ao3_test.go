package corpus

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// The fixture mirrors the real schema's column names and types but not
// its constraints. The corpus is opened read-only and queried by column
// name, so constraints are irrelevant to what kindred does with it.
const fixtureSchema = `
CREATE TABLE works(
	id INTEGER PRIMARY KEY, title TEXT NOT NULL, url TEXT NOT NULL, summary TEXT,
	authors TEXT NOT NULL, word_count INTEGER DEFAULT 0, hits INTEGER DEFAULT 0,
	kudos INTEGER DEFAULT 0, bookmarks INTEGER, chapters TEXT, language TEXT,
	complete INTEGER DEFAULT 0, update_date TEXT, first_seen TEXT, rating TEXT);
CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL COLLATE NOCASE);
CREATE TABLE work_tags(work_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, tag_type TEXT NOT NULL,
	PRIMARY KEY(work_id, tag_id));
CREATE TABLE cooccurrence_edges(tag_a_id INTEGER NOT NULL, tag_b_id INTEGER NOT NULL,
	cooccur_count INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(tag_a_id, tag_b_id));
`

func newFixture(t *testing.T) *AO3 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(fixtureSchema); err != nil {
		t.Fatal(err)
	}
	return NewAO3(db)
}

func seed(t *testing.T, a *AO3) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		// 1: NULL bookmarks, the trap. 2: a real bookmark count.
		`INSERT INTO works(id,title,url,summary,authors,word_count,hits,kudos,bookmarks,language,complete,update_date,first_seen,rating)
		 VALUES(1,'Slow Burn','https://ao3.example/1','a summary','Author One',50000,1000,300,NULL,'en',1,'2026-01-15','2025-01-01','General')`,
		`INSERT INTO works(id,title,url,summary,authors,word_count,hits,kudos,bookmarks,language,complete,update_date,first_seen,rating)
		 VALUES(2,'Fluff','https://ao3.example/2','other summary','Author Two',2000,50,10,42,'en',0,'2026-02-20','2026-02-01','General')`,
		`INSERT INTO works(id,title,url,summary,authors,word_count,hits,kudos,bookmarks,language,complete,update_date,first_seen,rating)
		 VALUES(3,'Orphan','https://ao3.example/3','no tags at all','Author Three',9000,0,1,0,'en',1,'2026-03-01','2026-03-01','Mature')`,
		`INSERT INTO tags(id,name) VALUES(1,'slow burn'),(2,'fluff'),(3,'enemies to lovers')`,
		`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(1,1,'freeforms'),(1,3,'freeforms'),(2,2,'freeforms'),(2,3,'relationships')`,
		`INSERT INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES(1,2,5),(1,3,9)`,
	}
	for _, s := range stmts {
		if _, err := a.DB.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

func TestCandidateRowsLoadsEntitiesAndTags(t *testing.T) {
	a := newFixture(t)
	seed(t, a)
	ents, err := a.CandidateRows(context.Background(), []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("got %d entities, want 2", len(ents))
	}
	for _, e := range ents {
		if e.Kind != AO3Kind {
			t.Fatalf("entity %d kind = %q, want %q", e.ID, e.Kind, AO3Kind)
		}
		if e.Title == "" {
			t.Fatalf("entity %d has no title", e.ID)
		}
	}
	if len(ents[0].Tags) != 2 {
		t.Fatalf("work 1 has %d tags, want 2", len(ents[0].Tags))
	}
	// tag_type must be carried through as a label, not branched on.
	sawRelationship := false
	for _, tg := range ents[1].Tags {
		if tg.Type == "relationships" {
			sawRelationship = true
		}
	}
	if !sawRelationship {
		t.Fatal("tag_type was not preserved; a signal that filters on type cannot work")
	}
}

// TestNullBookmarksIsNotZero is the explicit NULL/zero distinction.
// bookmarks is NULLable and uncrawled works leave it NULL; collapsing NULL to
// zero makes an unsorted column silently rank emptiest works highest. The
// fixture writes one NULL in seven precisely so this path cannot rot.
func TestNullBookmarksIsNotZero(t *testing.T) {
	a := newFixture(t)
	seed(t, a)
	ents, err := a.CandidateRows(context.Background(), []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	var nullWork, realWork Entity
	for _, e := range ents {
		switch e.ID {
		case 1:
			nullWork = e
		case 2:
			realWork = e
		}
	}
	if got := nullWork.Stats["has_bookmarks"]; got != 0 {
		t.Fatalf("work with NULL bookmarks has has_bookmarks=%v, want 0", got)
	}
	if _, present := nullWork.Stats["bookmarks"]; present {
		t.Fatal("a NULL bookmark count was written as a number; NULL and 0 must be distinguishable")
	}
	if got := realWork.Stats["has_bookmarks"]; got != 1 {
		t.Fatalf("work with 42 bookmarks has has_bookmarks=%v, want 1", got)
	}
	if got := realWork.Stats["bookmarks"]; got != 42 {
		t.Fatalf("bookmarks = %v, want 42", got)
	}
}

func TestEntityUsesTheSamePathAsThePool(t *testing.T) {
	// A single-entity path that builds an Entity differently from the
	// pool path is how a field ends up populated in one and missing in
	// the other. Compare every stat.
	a := newFixture(t)
	seed(t, a)
	single, err := a.Entity(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := a.CandidateRows(context.Background(), []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	if single.ID != pool[0].ID || single.Title != pool[0].Title ||
		len(single.Tags) != len(pool[0].Tags) || len(single.Stats) != len(pool[0].Stats) {
		t.Fatalf("single and pool disagree: single=%+v pool=%+v", single, pool[0])
	}
	if single.Stats["has_bookmarks"] != pool[0].Stats["has_bookmarks"] {
		t.Fatal("has_bookmarks differs between the single and pool paths")
	}
}

func TestEntityMissingIsNotFound(t *testing.T) {
	a := newFixture(t)
	seed(t, a)
	if _, err := a.Entity(context.Background(), 9999); err == nil {
		t.Fatal("a missing work did not error")
	}
}

func TestCandidateRowsWithNoIDs(t *testing.T) {
	a := newFixture(t)
	got, err := a.CandidateRows(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("empty ids returned %v, %v", got, err)
	}
}

func TestCandidateRowsDedupesIDs(t *testing.T) {
	a := newFixture(t)
	seed(t, a)
	got, err := a.CandidateRows(context.Background(), []int64{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("duplicate ids produced %d entities", len(got))
	}
}

func TestGraphTagsMeasuresRatherThanTrustsMetadata(t *testing.T) {
	// The real cooccurrence_graph_meta claimed 20,262 nodes while the
	// edges referenced 123,047. GraphTags must derive the node set from
	// the edges, so a stale metadata row cannot be inherited.
	a := newFixture(t)
	seed(t, a)
	set, n, err := a.GraphTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("graph tags = %d, want 3 (tags 1,2,3)", n)
	}
	for _, want := range []int32{1, 2, 3} {
		if !set[want] {
			t.Fatalf("tag %d missing from the graph set", want)
		}
	}
	// A tag in the tags table but never in an edge must not be counted.
	if _, err := a.DB.Exec(`INSERT INTO tags(id,name) VALUES(9,'unreferenced')`); err != nil {
		t.Fatal(err)
	}
	_, n2, err := a.GraphTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 3 {
		t.Fatalf("an unreferenced tag changed the count to %d", n2)
	}
}

func TestEdgeIterStreamsEachEdgeOnce(t *testing.T) {
	a := newFixture(t)
	seed(t, a)
	var got []Edge
	if err := a.EdgeIter(context.Background(), func(e Edge) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("streamed %d edges, want 2", len(got))
	}
	if got[0].A != 1 || got[0].B != 2 || got[0].Count != 5 {
		t.Fatalf("edge = %+v, want {1,2,5}", got[0])
	}
}

func TestCountersMatchTheFixture(t *testing.T) {
	a := newFixture(t)
	seed(t, a)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		fn   func(context.Context) (int, error)
		want int
	}{
		"works":     {a.CountWorks, 3},
		"tags":      {a.CountTags, 3},
		"work_tags": {a.CountWorkTags, 4},
		"edges":     {a.CountEdges, 2},
	} {
		got, err := tc.fn(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Fatalf("%s = %d, want %d", name, got, tc.want)
		}
	}
}

func TestDateParsing(t *testing.T) {
	// Zero means "unknown" and must not be mistaken for "very old".
	for _, in := range []string{"2026-01-15", "2026-01-15T10:00:00Z", "2026-01-15 10:00:00", "garbage", ""} {
		if got := parseDate(in); got == 0 && in != "garbage" && in != "" {
			t.Fatalf("parseDate(%q) = 0, want a real date", in)
		}
	}
	if got := parseDate("garbage"); got != 0 {
		t.Fatalf("parseDate(garbage) = %v, want 0", got)
	}
}

func TestIntersectPicksTheSmallerSide(t *testing.T) {
	a := TagSet{1: 1, 2: 2, 3: 3}
	b := TagSet{2: 0.5, 3: 0.25, 4: 1}
	// Whichever side is iterated, the result must be identical — the
	// smaller-side optimisation must not change the answer.
	shared, weight := Intersect(a, b)
	shared2, weight2 := Intersect(b, a)
	if shared != shared2 || weight != weight2 {
		t.Fatalf("order-dependent: (%d,%v) vs (%d,%v)", shared, weight, shared2, weight2)
	}
	if shared != 2 {
		t.Fatalf("shared = %d, want 2 (tags 2 and 3)", shared)
	}
	// min(2, 0.5) + min(3, 0.25) = 0.75
	if weight < 0.74 || weight > 0.76 {
		t.Fatalf("weight = %v, want ~0.75 (the smaller weight on each side)", weight)
	}
}

func TestIntersectDisjointAndEmpty(t *testing.T) {
	if s, w := Intersect(TagSet{1: 1}, TagSet{2: 1}); s != 0 || w != 0 {
		t.Fatalf("disjoint sets returned %d, %v", s, w)
	}
	if s, w := Intersect(nil, TagSet{1: 1}); s != 0 || w != 0 {
		t.Fatalf("nil side returned %d, %v", s, w)
	}
	if s, w := Intersect(TagSet{1: 1}, nil); s != 0 || w != 0 {
		t.Fatalf("nil side returned %d, %v", s, w)
	}
}

func TestIntersectIdenticalSets(t *testing.T) {
	a := TagSet{1: 1, 2: 2, 3: 3}
	s, w := Intersect(a, a)
	if s != 3 {
		t.Fatalf("shared = %d, want 3", s)
	}
	if w != 6 {
		t.Fatalf("weight = %v, want 6", w)
	}
}
