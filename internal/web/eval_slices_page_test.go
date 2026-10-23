package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// fakeEvalStore serves canned runs and slices.
type fakeEvalStore struct {
	runs   []store.EvalRun
	slices []store.EvalSlice
	asked  int64
}

func (f *fakeEvalStore) EvalRuns(context.Context, int) ([]store.EvalRun, error) {
	return f.runs, nil
}

func (f *fakeEvalStore) EvalSlices(_ context.Context, runID int64) ([]store.EvalSlice, error) {
	f.asked = runID
	return f.slices, nil
}

// TestEvalPagePrintsTheNewestRunsWeakestGroups is idea 46/71 at the surface:
// the dashboard has to name the groups where the run failed, weakest first,
// and it has to ask for the NEWEST run's slices rather than any run's.
func TestEvalPagePrintsTheNewestRunsWeakestGroups(t *testing.T) {
	f := &fakeEvalStore{
		runs: []store.EvalRun{
			{ID: 9, RanAt: "2026-10-23 10:00:00", K: 10, Mode: "full", Readers: 40, Evaluated: 40},
			{ID: 8, RanAt: "2026-10-22 10:00:00", K: 10, Mode: "full", Readers: 40, Evaluated: 40},
		},
		// Delivered best-first, as the store returns them.
		slices: []store.EvalSlice{
			{Axis: "fandom", Name: "big fandom", N: 300, Hits: 90, HitRate: 0.30},
			{Axis: "fandom", Name: "quiet fandom", N: 25, Hits: 0, HitRate: 0},
			{Axis: "tag", Name: "selkie au", N: 30, Hits: 1, HitRate: 0.0333},
		},
	}
	d := Deps{Version: "test", Eval: f}

	rr := httptest.NewRecorder()
	d.renderEval(rr, httptest.NewRequest(http.MethodGet, "/eval", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if f.asked != 9 {
		t.Errorf("asked for run %d's slices, want the newest (9)", f.asked)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-testid="eval-slices-fandom"`) {
		t.Fatalf("no fandom slice table:\n%s", headOf(body, 800))
	}
	if !strings.Contains(body, "quiet fandom") {
		t.Errorf("a fandom with zero hits is missing from the table")
	}
	// Weakest first: the zero-hit fandom must come before the 30% one.
	if strings.Index(body, "quiet fandom") > strings.Index(body, "big fandom") {
		t.Errorf("slices are not weakest-first: quiet fandom appears after big fandom")
	}
	// And the page names whose run these are, so the section cannot be read as
	// a summary of the whole table above it.
	if !strings.Contains(body, "Where run #9 was weak") {
		t.Errorf("the section does not name the run it describes")
	}
}

// TestEvalPageWithoutSlicesOmitsTheSection pins the older-run case: a run
// recorded before slices existed has none, and the page must not print an
// empty table with an apology.
func TestEvalPageWithoutSlicesOmitsTheSection(t *testing.T) {
	f := &fakeEvalStore{runs: []store.EvalRun{{ID: 3, K: 10, Mode: "full"}}}
	d := Deps{Version: "test", Eval: f}
	rr := httptest.NewRecorder()
	d.renderEval(rr, httptest.NewRequest(http.MethodGet, "/eval", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "was weak") {
		t.Errorf("an empty slice set rendered the section heading")
	}
}
