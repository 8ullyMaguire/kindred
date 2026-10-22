package store

import (
	"context"
	"testing"
)

// An evaluation that is not stored cannot be compared with the next one, which
// is the only thing an offline arm is for. This pins the round trip and the two
// columns that make runs comparable: k and mode.
func TestEvalRunRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)

	id, err := s.RecordEvalRun(ctx, EvalRun{
		K: 10, Readers: 214, Bookmarks: 5100, Evaluated: 199, Failures: 1,
		HeldOut: 1020, Hits: 143, Recall: 0.1402, NDCG: 0.0931,
		Coverage: 0.6120, Mode: "full",
	})
	if err != nil {
		t.Fatalf("RecordEvalRun: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id = %d, want a real rowid", id)
	}

	runs, err := s.EvalRuns(ctx, 10)
	if err != nil {
		t.Fatalf("EvalRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.ID != id || r.K != 10 || r.Mode != "full" {
		t.Fatalf("identity fields wrong: %+v", r)
	}
	if r.Recall != 0.1402 || r.NDCG != 0.0931 || r.Coverage != 0.6120 {
		t.Fatalf("metrics did not survive the round trip: %+v", r)
	}
	if r.Evaluated != 199 || r.Failures != 1 || r.Hits != 143 || r.HeldOut != 1020 {
		t.Fatalf("counts did not survive the round trip: %+v", r)
	}
	if r.RanAt == "" {
		t.Fatal("ran_at is empty; a run with no timestamp cannot be compared over time")
	}
}

// Newest first, and the limit is real. A dashboard that silently shows the
// OLDEST runs would look like the recommender never changes.
func TestEvalRunsNewestFirstAndLimited(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)
	for i := 1; i <= 3; i++ {
		if _, err := s.RecordEvalRun(ctx, EvalRun{
			K: 10, Mode: "full", Recall: float64(i) / 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.EvalRuns(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("%d runs with limit 2, want 2", len(runs))
	}
	if runs[0].Recall <= runs[1].Recall {
		t.Fatalf("runs are not newest-first: %+v", runs)
	}
}
