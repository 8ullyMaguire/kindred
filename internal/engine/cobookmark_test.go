package engine

import (
	"context"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/collab"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// coBookmarkFixture builds an engine whose corpus has real co-bookmark rows.
//
// The shared fixture has no interactions at all, and its corpus is attached
// READ-ONLY (as production's is), so the rows have to be written before the
// store opens. That is why this rebuilds the fixture instead of calling
// newFixtureEngine and inserting afterwards -- and it is also why the SQL
// fallback needs a test at all: with an empty table both the index and the
// fallback return nothing, and "nothing" is also the answer a broken query
// gives.
func coBookmarkFixture(t *testing.T) *Engine {
	t.Helper()
	c := testcorpus.New(60)
	c.Users = []testcorpus.User{
		{ID: 1, Username: "one", BookmarkCount: 3},
		{ID: 2, Username: "two", BookmarkCount: 2},
		{ID: 3, Username: "three", BookmarkCount: 2},
		{ID: 4, Username: "four", BookmarkCount: 2},
	}
	rows := []struct {
		user, work int64
	}{
		// work 1 is the seed. Two readers of it also kept work 2 and two
		// also kept work 3; one kept work 4. A duplicate row for (1, 2)
		// reproduces the live table's shape: COUNT(*) would call that pair
		// three readers when it is two.
		{1, 1}, {2, 1}, {3, 1}, {4, 1},
		{1, 2}, {2, 2}, {1, 2},
		{3, 3}, {4, 3},
		{1, 4},
	}
	for _, r := range rows {
		c.Interactions = append(c.Interactions,
			testcorpus.Interaction{UserID: r.user, WorkID: r.work})
	}
	return newFixtureEngineFrom(t, c)
}

func TestCoBookmarkedCountsDistinctReadersAndOrdersByThem(t *testing.T) {
	e := coBookmarkFixture(t)
	got, err := e.CoBookmarked(context.Background(), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d works, want 3: %v", len(got), idsOfCandidates(got))
	}
	if got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("order = %v, want [2 3 4]: works 2 and 3 have two shared readers, work 4 has one",
			idsOfCandidates(got))
	}
	if got[2].ID != 4 {
		t.Fatalf("last = %d, want 4", got[2].ID)
	}
	for _, c := range got {
		if c.ID == 1 {
			t.Fatal("the seed itself was returned as a co-bookmark")
		}
		if len(c.Evidence) == 0 || c.Evidence[0].Signal != "co_bookmark" {
			t.Errorf("work %d carries no co_bookmark evidence: %+v", c.ID, c.Evidence)
		}
	}
	// The duplicate (user 1, work 2) must not make work 2 look like it has
	// three readers.
	if got[0].Evidence[0].Value != 2 {
		t.Errorf("work 2 evidence = %v, want 2 distinct readers", got[0].Evidence[0].Value)
	}
}

func TestCoBookmarkedIndexAndSQLAgree(t *testing.T) {
	e := coBookmarkFixture(t)
	fromSQL, err := e.CoBookmarked(context.Background(), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	idx, _, err := (&collab.Builder{Corpus: e.Corpus.DB}).Build(context.Background())
	if err != nil {
		t.Fatalf("build collab index: %v", err)
	}
	e.Collab = idx
	fromIndex, err := e.CoBookmarked(context.Background(), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromIndex) != len(fromSQL) {
		t.Fatalf("index returned %v, SQL returned %v", idsOfCandidates(fromIndex), idsOfCandidates(fromSQL))
	}
	for i := range fromSQL {
		if fromIndex[i].ID != fromSQL[i].ID {
			t.Fatalf("index order %v, SQL order %v", idsOfCandidates(fromIndex), idsOfCandidates(fromSQL))
		}
	}
}

func TestCoBookmarkedWithNoSharedReadersIsAnEmptyList(t *testing.T) {
	e := newFixtureEngine(t)
	got, err := e.CoBookmarked(context.Background(), 1, 5)
	if err != nil {
		// A corpus fixture without the interactions table is a real
		// deployment state, but it is not this test's state: newFixtureEngine
		// creates the table through testcorpus. If that changes, the test
		// should say so rather than silently pass.
		t.Fatalf("co-bookmarked on a corpus without interactions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want an empty list", idsOfCandidates(got))
	}
}

func idsOfCandidates(c []rank.Candidate) []int64 {
	out := make([]int64, len(c))
	for i := range c {
		out[i] = c[i].ID
	}
	return out
}
