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

// TestEvalSlicesRoundTripAndReplace pins the two things the dashboard relies
// on: the order (best hit rate first within an axis) and the fact that a
// second record for the same run REPLACES the first rather than doubling it.
// A run is one measurement; two rows for one group would make the page's
// weakest-first head depend on insertion order.
func TestEvalSlicesRoundTripAndReplace(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)

	runID, err := s.RecordEvalRun(ctx, EvalRun{
		K: 10, Readers: 40, Bookmarks: 400, Evaluated: 40,
		HeldOut: 120, Hits: 12, Recall: 0.1, NDCG: 0.05, Coverage: 0.5, Mode: "full",
	})
	if err != nil {
		t.Fatalf("RecordEvalRun: %v", err)
	}

	if err := s.RecordEvalSlices(ctx, runID, []EvalSlice{
		{Axis: "fandom", Name: "quiet", N: 25, Hits: 0},
		{Axis: "fandom", Name: "loud", N: 300, Hits: 90},
		{Axis: "tag", Name: "selkie au", N: 30, Hits: 1},
	}); err != nil {
		t.Fatalf("RecordEvalSlices: %v", err)
	}

	got, err := s.EvalSlices(ctx, runID)
	if err != nil {
		t.Fatalf("EvalSlices: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d slices, want 3: %+v", len(got), got)
	}
	// Ordered by axis, then hit rate descending: "loud" (0.30) before "quiet".
	var fandom []string
	for _, sl := range got {
		if sl.Axis == "fandom" {
			fandom = append(fandom, sl.Name)
		}
		if sl.Axis == "fandom" && sl.Name == "loud" && sl.HitRate != 0.3 {
			t.Errorf("loud hit rate = %v, want 0.3", sl.HitRate)
		}
		if sl.Axis == "fandom" && sl.Name == "quiet" && sl.HitRate != 0 {
			t.Errorf("quiet hit rate = %v, want 0", sl.HitRate)
		}
	}
	if len(fandom) != 2 || fandom[0] != "loud" {
		t.Errorf("fandom slices = %v, want loud first (hit rate desc)", fandom)
	}

	// Re-recording replaces: running eval twice on one run id is a
	// correction, and a corrections-not-duplicates rule is the one that keeps
	// the table readable.
	if err := s.RecordEvalSlices(ctx, runID, []EvalSlice{
		{Axis: "fandom", Name: "loud", N: 300, Hits: 60},
	}); err != nil {
		t.Fatalf("second RecordEvalSlices: %v", err)
	}
	again, err := s.EvalSlices(ctx, runID)
	if err != nil {
		t.Fatalf("EvalSlices: %v", err)
	}
	if len(again) != 1 || again[0].Name != "loud" || again[0].Hits != 60 {
		t.Errorf("after the correction: %+v, want one row for loud with 60 hits", again)
	}

	// A run id that is not a record is refused rather than written as 0.
	if err := s.RecordEvalSlices(ctx, 0, nil); err == nil {
		t.Errorf("RecordEvalSlices(0) did not refuse")
	}
}
