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
