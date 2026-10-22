package web

import (
	"context"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// evalDeps wires /eval: a fixture corpus, a store with one recorded run, one
// algorithm vote and one interleaved impression. The page is a read-only view,
// so the interesting failure is a page that renders while showing nothing that
// was actually measured.
func evalDeps(t *testing.T) Deps {
	t.Helper()
	d, st := algoDeps(t)
	ctx := context.Background()
	if _, err := st.RecordEvalRun(ctx, store.EvalRun{
		K: 10, Readers: 214, Bookmarks: 5100, Evaluated: 199,
		HeldOut: 1020, Hits: 143, Recall: 0.1402, NDCG: 0.0931,
		Coverage: 0.6120, Mode: "full",
	}); err != nil {
		t.Fatalf("RecordEvalRun: %v", err)
	}
	if err := st.RecordAlgoComparison(ctx, "owner", store.AlgoComparison{
		SeedKind: "ao3_work", SeedID: 1, AlgoA: "default", AlgoB: "quality", Winner: "a",
	}); err != nil {
		t.Fatalf("RecordAlgoComparison: %v", err)
	}
	if err := st.RecordInterleave(ctx, "owner", "ao3_work", 1, "default", "quality",
		[]store.InterleaveSlot{{WorkID: 2, Side: "a", Slot: 0}, {WorkID: 3, Side: "b", Slot: 1}}); err != nil {
		t.Fatalf("RecordInterleave: %v", err)
	}
	d.Eval = st
	d.Interleave = st
	return d
}

func TestTheEvalPageShowsWhatWasMeasured(t *testing.T) {
	d := evalDeps(t)
	code, body := arenaBody(t, d, "/eval")
	if code != 200 {
		t.Fatalf("GET /eval = %d", code)
	}
	// The offline number, unrounded to four places: the page must show the
	// measurement rather than a rounded impression of it.
	for _, want := range []string{"0.1402", "0.0931", "0.6120"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the stored run's %s is not on the page", want)
		}
	}
	// Both online measurements are present, and each names its source.
	if !strings.Contains(body, "algorithm votes") {
		t.Fatal("the explicit online measurement is missing")
	}
	if !strings.Contains(body, "interleaved credits") {
		t.Fatal("the implicit online measurement is missing")
	}
	// The corpus under the numbers, so a metric is not a number from nowhere.
	if !strings.Contains(body, "Co-occurrence edges") {
		t.Fatal("the page does not say what the measurements were taken across")
	}
}

// No stored runs must read as "nothing measured yet", not as a table of zeros
// that a reader would take for a terrible recommender.
func TestTheEvalPageSaysWhenNothingIsMeasured(t *testing.T) {
	d, _ := algoDeps(t) // a store with no eval runs recorded
	d.Eval = nil
	code, body := arenaBody(t, d, "/eval")
	if code != 200 {
		t.Fatalf("GET /eval without a store = %d", code)
	}
	if !strings.Contains(body, "No offline runs recorded yet") {
		t.Fatal("the page does not say there are no stored runs")
	}
	if strings.Contains(body, "0.0000") {
		t.Fatal("the page printed zeros as if they were measurements")
	}
}
