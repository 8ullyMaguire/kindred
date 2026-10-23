package eval

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return db
}

// fakeRecommender returns a fixed order and records the seeds it was given.
// It is how the arithmetic is tested without a corpus, a graph or the engine.
type fakeRecommender struct {
	order []int64
	seen  [][]int64
	err   error
}

func (f *fakeRecommender) Recommend(ctx context.Context, seeds []int64, n int) ([]int64, error) {
	f.seen = append(f.seen, append([]int64(nil), seeds...))
	if f.err != nil {
		return nil, f.err
	}
	out := f.order
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

func TestLoadBookmarksCountsOnlyDistinctBookmarkedRows(t *testing.T) {
	dir := t.TempDir()
	// Build a tiny corpus with the trap rows the real mirror carries: a
	// duplicate bookmark and a 'bookmarker' row, which is about a work
	// carrying a user id rather than this user's choice.
	c := testcorpus.New(4)
	c.Works = []testcorpus.Work{
		{ID: 1, Title: "one", Authors: "a"},
		{ID: 2, Title: "two", Authors: "b"},
		{ID: 3, Title: "three", Authors: "c"},
		{ID: 4, Title: "four", Authors: "d"},
	}
	c.Users = []testcorpus.User{{ID: 10, Username: "ten", BookmarkCount: 2}}
	c.Interactions = []testcorpus.Interaction{
		{UserID: 10, WorkID: 1},
		{UserID: 10, WorkID: 1}, // duplicate row, as the live table has
		{UserID: 10, WorkID: 2},
		{UserID: 10, WorkID: 3, InteractionType: "bookmarker"},
	}
	c.Tags = nil
	c.WorkTags = nil
	c.Edges = nil
	path := filepath.Join(dir, "corpus.db")
	if _, err := c.Write(path); err != nil {
		t.Fatal(err)
	}
	db := openDB(t, path)
	defer db.Close()

	byUser, users, err := LoadBookmarks(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Fatalf("users = %d, want 1", users)
	}
	got := byUser[10]
	if len(got) != 2 {
		t.Fatalf("bookmarks = %v, want [1 2]: duplicates and 'bookmarker' must not count", got)
	}
	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("bookmarks = %v, want [1 2]", got)
	}
}

func TestSplitIsDeterministicAndDisjoint(t *testing.T) {
	byUser := map[int64][]int64{
		1: {1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		2: {11, 12, 13},
		3: {14}, // too few: cannot supply both a seed and a held-out item
	}
	a := SplitBookmarks(byUser, 0.2)
	b := SplitBookmarks(byUser, 0.2)

	if a.TooFew != 1 {
		t.Fatalf("TooFew = %d, want 1: a one-bookmark reader cannot be split", a.TooFew)
	}
	for user := range byUser {
		if len(a.Train[user]) != len(b.Train[user]) || len(a.Test[user]) != len(b.Test[user]) {
			t.Fatalf("user %d split differs between runs: %v/%v vs %v/%v",
				user, a.Train[user], a.Test[user], b.Train[user], b.Test[user])
		}
		seen := map[int64]bool{}
		for _, id := range a.Train[user] {
			seen[id] = true
		}
		for _, id := range a.Test[user] {
			if seen[id] {
				t.Fatalf("user %d: work %d is in both train and test", user, id)
			}
		}
	}
	if got := len(a.Test[1]); got != 2 {
		t.Fatalf("held out %d of 10 at ratio .2, want 2", got)
	}
	if got := len(a.Test[2]); got != 1 {
		t.Fatalf("held out %d of 3 at ratio .2, want at least 1", got)
	}
}

func TestRunComputesRecallAndNDCG(t *testing.T) {
	split := Split{
		Train: map[int64][]int64{1: {100}, 2: {200}},
		Test:  map[int64][]int64{1: {3, 4}, 2: {9}},
		Users: 2, Bookmarks: 3,
	}
	// Reader 1's held-out works are at ranks 2 and 4; reader 2's only
	// held-out work is absent, which is a genuine miss rather than a
	// failure.
	rec := &fakeRecommender{order: []int64{1, 3, 5, 4, 6}}
	rep, err := Run(context.Background(), rec, split, 5)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Evaluated != 2 || rep.Hits != 2 || rep.HeldOut != 3 {
		t.Fatalf("evaluated=%d hits=%d held=%d, want 2/2/3", rep.Evaluated, rep.Hits, rep.HeldOut)
	}
	if len(rec.seen) != 2 || rec.seen[0][0] != 100 || rec.seen[1][0] != 200 {
		t.Fatalf("recommender saw %v, want each reader's own train set", rec.seen)
	}

	wantRecall := 0.5 // (1/2 + 0/1) / 2
	if math.Abs(rep.RecallAtK-wantRecall) > 1e-9 {
		t.Fatalf("recall = %v, want %v", rep.RecallAtK, wantRecall)
	}
	// DCG for reader 1: 1/log2(3) + 1/log2(5). IDCG (2 relevant): 1 + 1/log2(3).
	wantNDCG := (1/math.Log2(3) + 1/math.Log2(5)) / (1 + 1/math.Log2(3)) / 2
	if math.Abs(rep.NDCGAtK-wantNDCG) > 1e-9 {
		t.Fatalf("ndcg = %v, want %v", rep.NDCGAtK, wantNDCG)
	}
	if rep.Coverage != 0.5 {
		t.Fatalf("coverage = %v, want 0.5", rep.Coverage)
	}
}

func TestRunCountsAFailureRatherThanAMiss(t *testing.T) {
	split := Split{
		Train: map[int64][]int64{1: {100}},
		Test:  map[int64][]int64{1: {3}},
		Users: 1,
	}
	rep, err := Run(context.Background(), &fakeRecommender{err: context.DeadlineExceeded}, split, 5)
	if err == nil {
		t.Fatal("every recommendation failed but Run reported success")
	}
	if rep.Failures != 1 || rep.Evaluated != 0 {
		t.Fatalf("failures=%d evaluated=%d, want 1/0", rep.Failures, rep.Evaluated)
	}
}

func TestRunIgnoresDuplicatesInTheRecommendation(t *testing.T) {
	split := Split{
		Train: map[int64][]int64{1: {100}},
		Test:  map[int64][]int64{1: {3}},
		Users: 1,
	}
	rep, err := Run(context.Background(), &fakeRecommender{order: []int64{3, 3, 3}}, split, 5)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hits != 1 {
		t.Fatalf("hits = %d, want 1: a repeated id must not be paid twice", rep.Hits)
	}
}

// TestPlacementsRecordWhereEachHeldOutWorkLanded pins the per-work detail the
// slices are grouped from. Without it, idea 46/71 could only be faked: a slice
// computed from aggregate sums cannot say which fandom was short.
func TestPlacementsRecordWhereEachHeldOutWorkLanded(t *testing.T) {
	split := Split{
		Users:     1,
		Bookmarks: 4,
		Train:     map[int64][]int64{1: {100}},
		Test:      map[int64][]int64{1: {7, 8, 9}},
	}
	// 7 is found at position 1, 8 at position 3, 9 is absent.
	rec := &fakeRecommender{order: []int64{7, 55, 8}}
	rep, err := Run(context.Background(), rec, split, 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := map[int64]int{7: 1, 8: 3, 9: 0}
	if len(rep.Placements) != 3 {
		t.Fatalf("got %d placements, want 3: %+v", len(rep.Placements), rep.Placements)
	}
	for _, p := range rep.Placements {
		if p.Rank != want[p.Work] {
			t.Errorf("work %d ranked %d, want %d", p.Work, p.Rank, want[p.Work])
		}
	}
	// The aggregate must still be the aggregate: two of three found.
	if rep.Hits != 2 {
		t.Errorf("Hits = %d, want 2", rep.Hits)
	}
}

// TestByGroupCountsAWorkInEveryGroupAndDropsSmallOnes is the slice rule:
// a work belongs to each of its fandoms, and a group below minN is not
// reported at all rather than reported with a number nobody can read.
func TestByGroupCountsAWorkInEveryGroupAndDropsSmallOnes(t *testing.T) {
	placements := []Placement{
		{Work: 1, Rank: 1},
		{Work: 2, Rank: 0},
		{Work: 3, Rank: 2},
		{Work: 4, Rank: 0},
	}
	groups := map[int64][]string{
		1: {"alpha", "crossover"},
		2: {"alpha"},
		3: {"beta"},
		4: {"beta"},
	}
	slices := ByGroup(placements, "fandom", 1, func(w int64) []string { return groups[w] })

	by := map[string]Slice{}
	for _, s := range slices {
		by[s.Name] = s
	}
	if g := by["alpha"]; g.N != 2 || g.Hits != 1 || g.HitRate != 0.5 {
		t.Errorf("alpha = %+v, want n=2 hits=1 rate=0.5", g)
	}
	if g := by["beta"]; g.N != 2 || g.Hits != 1 {
		t.Errorf("beta = %+v, want n=2 hits=1", g)
	}
	// A work in two fandoms counts in both, and the totals may therefore
	// exceed the placements: that is the point, not a bug.
	if g := by["crossover"]; g.N != 1 || g.Hits != 1 {
		t.Errorf("crossover = %+v, want n=1 hits=1", g)
	}

	// minN drops the small group.
	big := ByGroup(placements, "fandom", 2, func(w int64) []string { return groups[w] })
	for _, s := range big {
		if s.Name == "crossover" {
			t.Errorf("minN=2 kept crossover (%+v)", s)
		}
	}
	// Ordering is deterministic: N desc, then name.
	for i := 1; i < len(big); i++ {
		if big[i-1].N < big[i].N {
			t.Errorf("slices are not ordered by N desc: %+v", big)
		}
		if big[i-1].N == big[i].N && big[i-1].Name > big[i].Name {
			t.Errorf("equal-N slices are not ordered by name: %+v", big)
		}
	}
}
