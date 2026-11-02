package eval

import (
	"context"
	"testing"
)

// TestUserSinkRecordsEveryEvaluatedReader pins the contract `kindred evalcmp` depends
// on: one UserResult per reader that was actually scored, carrying that reader's own
// recall and NDCG.
//
// Without it, two arms can only be compared by their aggregate means, and at the
// cohort sizes this evaluator runs (37 readers, four held-out works each) an aggregate
// difference is indistinguishable from noise.
func TestUserSinkRecordsEveryEvaluatedReader(t *testing.T) {
	split := Split{
		Train: map[int64][]int64{1: {10, 11}, 2: {20, 21}},
		Test:  map[int64][]int64{1: {12}, 2: {22}},
		Users: 2,
	}
	// Reader 1's held-out work is at rank 1; reader 2's is at rank 3.
	rec := &fakeRecommender{order: []int64{12, 99, 98, 22}}

	var got []UserResult
	rep, err := Run(context.Background(), rec, split, 10, WithUserSink(func(u UserResult) {
		got = append(got, u)
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != rep.Evaluated {
		t.Fatalf("sink saw %d readers, report evaluated %d", len(got), rep.Evaluated)
	}
	if len(got) != 2 {
		t.Fatalf("sink saw %d readers, want 2", len(got))
	}
	byUser := map[int64]UserResult{}
	for _, u := range got {
		byUser[u.User] = u
	}
	first, ok := byUser[1]
	if !ok {
		t.Fatalf("reader 1 missing from the sink: %+v", got)
	}
	if first.HeldOut != 1 || first.Hits != 1 || first.Recall != 1 {
		t.Fatalf("reader 1 = %+v, want held_out 1, hits 1, recall 1", first)
	}
	if first.Coverage != 1 {
		t.Fatalf("reader 1 coverage = %v, want 1", first.Coverage)
	}
	second, ok := byUser[2]
	if !ok {
		t.Fatalf("reader 2 missing from the sink: %+v", got)
	}
	if second.Hits != 1 || second.Recall != 1 {
		t.Fatalf("reader 2 = %+v, want hits 1, recall 1 (rank 3 is inside k=10)", second)
	}
	if second.NDCG >= first.NDCG {
		t.Fatalf("reader 2 ndcg %.4f should be below reader 1's %.4f: rank 3 against rank 1",
			second.NDCG, first.NDCG)
	}
}

// A reader whose recommendation FAILED is not evaluated, so the sink must not see
// them: a broken request and a bad ranking are different facts, and folding the
// failure into a per-reader zero would let an outage look like a regression.
func TestUserSinkSkipsFailedReaders(t *testing.T) {
	split := Split{
		Train: map[int64][]int64{1: {10, 11}},
		Test:  map[int64][]int64{1: {12}},
		Users: 1,
	}
	rec := &fakeRecommender{err: context.DeadlineExceeded}

	var got []UserResult
	_, err := Run(context.Background(), rec, split, 10, WithUserSink(func(u UserResult) {
		got = append(got, u)
	}))
	// Run refuses a report when EVERY recommendation failed: an outage must not be
	// reported as a score of zero. The sink must agree with that and stay empty.
	if err == nil {
		t.Fatal("Run returned a report for a run in which every reader failed")
	}
	if len(got) != 0 {
		t.Fatalf("sink saw %d results for a failed run, want 0", len(got))
	}
}
