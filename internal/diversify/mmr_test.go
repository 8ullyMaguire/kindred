package diversify

import (
	"fmt"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

func c(id int64, score float64, tags ...int32) rank.Candidate {
	return rank.Candidate{ID: id, Kind: "test", Score: score, TagIDs: tags}
}

func TestApplyReturnsRequestedCountWhenPossible(t *testing.T) {
	in := []rank.Candidate{c(1, 0.9, 10), c(2, 0.8, 20), c(3, 0.7, 30)}
	res := Apply(in, Options{Requested: 3, Lambda: 1})
	if len(res.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(res.Items))
	}
	if res.Shortfall != nil {
		t.Fatalf("a full result reported a shortfall: %+v", res.Shortfall)
	}
}

func TestApplyPreservesScoreOrderAtLambdaOne(t *testing.T) {
	// At lambda=1 there is no diversity term, so this is plain
	// score-ordered selection.
	in := []rank.Candidate{c(3, 0.7, 30), c(1, 0.9, 10), c(2, 0.8, 20)}
	SortByScore(in)
	res := Apply(in, Options{Requested: 3, Lambda: 1})
	for i, want := range []int64{1, 2, 3} {
		if res.Items[i].ID != want {
			t.Fatalf("position %d = %d, want %d", i, res.Items[i].ID, want)
		}
	}
}

// TestShortfallIsReportedNotPadded is the behaviour the previous
// deployment's spec explicitly corrected: asking for 100 with a cap that
// admits 90 must return 90 rows and say why, never 100 with the cap
// quietly ignored.
func TestShortfallIsReportedNotPadded(t *testing.T) {
	// Five candidates, all in the same group, cap of 2.
	in := []rank.Candidate{
		c(1, 0.9, 1, 2), c(2, 0.85, 1, 3), c(3, 0.8, 1, 4), c(4, 0.75, 1, 5), c(5, 0.7, 1, 6),
	}
	group := func(rank.Candidate) string { return "same" }
	res := Apply(in, Options{Requested: 5, Lambda: 1, MaxPerGroup: 2, GroupOf: group})

	if len(res.Items) != 2 {
		t.Fatalf("got %d items, want 2: the cap admits no more", len(res.Items))
	}
	if res.Shortfall == nil {
		t.Fatal("a capped result reported no shortfall; a short list with no explanation is a silent lie")
	}
	if res.Shortfall.Requested != 5 || res.Shortfall.Returned != 2 {
		t.Fatalf("shortfall = %+v, want requested=5 returned=2", res.Shortfall)
	}
	if res.Shortfall.Reason == "" {
		t.Fatal("the shortfall does not say why")
	}
}

func TestCapIsRespectedAcrossGroups(t *testing.T) {
	in := []rank.Candidate{
		c(1, 0.9, 1), c(2, 0.85, 1), c(3, 0.8, 1),
		c(4, 0.7, 2), c(5, 0.65, 2), c(6, 0.6, 2),
	}
	group := func(x rank.Candidate) string {
		if len(x.TagIDs) == 0 {
			return ""
		}
		return fmt.Sprint(x.TagIDs[0])
	}
	res := Apply(in, Options{Requested: 6, Lambda: 1, MaxPerGroup: 2, GroupOf: group})
	if len(res.Items) != 4 {
		t.Fatalf("got %d items, want 4 (2 per group over 2 groups)", len(res.Items))
	}
}

func TestGroupKeyFromTagNames(t *testing.T) {
	a := rank.Candidate{ID: 1, TagNames: []string{"enemies to lovers", "slow burn"}}
	b := rank.Candidate{ID: 2, TagNames: []string{"hurt no comfort"}}
	if GroupByTag(a) != "enemies to lovers" {
		t.Fatalf("GroupByTag = %q", GroupByTag(a))
	}
	if GroupByTag(b) != "hurt no comfort" {
		t.Fatalf("GroupByTag = %q", GroupByTag(b))
	}
	if GroupByTag(rank.Candidate{ID: 3}) != "" {
		t.Fatal("a candidate with no tags must have an empty group key, not a panic")
	}
}

func TestMMRBoundsKAndSaysSo(t *testing.T) {
	// MMR is O(k x n). Bounding k must be visible, not silent.
	in := make([]rank.Candidate, 100)
	for i := range in {
		in[i] = c(int64(i+1), float64(100-i), int32(i))
	}
	res := Apply(in, Options{Requested: 100, Lambda: 0.7, MMRK: 20})
	if len(res.Items) != 20 {
		t.Fatalf("got %d items, want 20 with MMRK=20", len(res.Items))
	}
	if res.Shortfall == nil {
		t.Fatal("bounding k did not produce a reported shortfall")
	}
	if res.Shortfall.Returned != 20 || res.Shortfall.Requested != 100 {
		t.Fatalf("shortfall = %+v", res.Shortfall)
	}
}

func TestMMRPrefersDiversityOverRawScore(t *testing.T) {
	// Candidate 2 scores highest but shares everything with 1. With a
	// diversity weight, 3 should be chosen second.
	in := []rank.Candidate{
		c(1, 0.90, 1, 2, 3),
		c(2, 0.89, 1, 2, 3), // near-duplicate of 1
		c(3, 0.70, 7, 8, 9), // distinct
	}
	res := Apply(in, Options{Requested: 2, Lambda: 0.5})
	if len(res.Items) != 2 {
		t.Fatalf("got %d", len(res.Items))
	}
	if res.Items[0].ID != 1 {
		t.Fatalf("first pick = %d, want the top scorer 1", res.Items[0].ID)
	}
	if res.Items[1].ID != 3 {
		t.Fatalf("second pick = %d, want the distinct 3, not the near-duplicate 2", res.Items[1].ID)
	}
}

func TestApplyOnEmptyAndSingle(t *testing.T) {
	if got := Apply(nil, Options{Requested: 10}); len(got.Items) != 0 || got.Shortfall == nil {
		t.Fatalf("empty input produced %d items, shortfall %+v", len(got.Items), got.Shortfall)
	}
	one := Apply([]rank.Candidate{c(1, 1)}, Options{Requested: 5})
	if len(one.Items) != 1 || one.Shortfall == nil {
		t.Fatalf("single input produced %d items, shortfall %+v", len(one.Items), one.Shortfall)
	}
}

func TestRequestedLargerThanAvailable(t *testing.T) {
	// Asking for 10 from a corpus of 2 must report 2 of 10, not quietly
	// treat the request as 2 and report a full result. The shortfall is
	// the honest account, and it is measured against what was asked.
	in := []rank.Candidate{c(1, 0.5), c(2, 0.4)}
	res := Apply(in, Options{Requested: 10})
	if len(res.Items) != 2 {
		t.Fatalf("got %d, want 2", len(res.Items))
	}
	if res.Shortfall == nil {
		t.Fatal("a request for 10 answered with 2 reported no shortfall")
	}
	if res.Shortfall.Requested != 10 || res.Shortfall.Returned != 2 {
		t.Fatalf("shortfall = %+v, want requested=10 returned=2", res.Shortfall)
	}
}

func TestZeroGroupKeyIsNotCapped(t *testing.T) {
	// An entity with no group must not be treated as being in a group
	// called "", or every ungrouped entity would cap against each other.
	in := []rank.Candidate{c(1, 0.9), c(2, 0.8), c(3, 0.7)}
	group := func(rank.Candidate) string { return "" }
	res := Apply(in, Options{Requested: 3, Lambda: 1, MaxPerGroup: 1, GroupOf: group})
	if len(res.Items) != 3 {
		t.Fatalf("got %d items, want 3: an empty group key must not consume the cap", len(res.Items))
	}
}

func TestSortByScoreIsStableAndTotal(t *testing.T) {
	in := []rank.Candidate{c(3, 0.5), c(1, 0.5), c(2, 0.9)}
	SortByScore(in)
	if in[0].ID != 2 || in[1].ID != 1 || in[2].ID != 3 {
		t.Fatalf("sorted to %d,%d,%d; want 2,1,3 (score desc, then id)", in[0].ID, in[1].ID, in[2].ID)
	}
}

func TestJaccard(t *testing.T) {
	a := map[int32]struct{}{1: {}, 2: {}, 3: {}}
	b := map[int32]struct{}{2: {}, 3: {}, 4: {}}
	if got := jaccard(a, b); got < 0.49 || got > 0.51 {
		t.Fatalf("jaccard = %v, want ~0.5", got)
	}
	if jaccard(a, nil) != 0 || jaccard(nil, b) != 0 {
		t.Fatal("a nil side must score 0, not panic")
	}
	if jaccard(nil, nil) != 0 {
		t.Fatal("two nil sets must score 0")
	}
}

var _ = fmt.Sprint
