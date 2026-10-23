package web

// The calibration block's three honesty claims, as tests over the real store.
//
//  1. A deployment with no evaluation says so, and says what would fix it. It must
//     not render an empty table or a 0%.
//  2. A band nothing reached is a dash with its counts, not 0%.
//  3. A run smaller than MinCalibrationReaders prints NO percentage — three readers
//     produce rates of 0, 0.33, 0.67 and 1, and "1 in 3" for that is a coin flip
//     with a decimal point.
//
// The view is built from the store rather than from a hand-made struct here,
// because the wiring between the two is where a wrong column would hide.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func calibrationDeps(t *testing.T) (Deps, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	// Open reads the corpus, so it has to be a real (if small) mirror.
	corpusPath, err := testcorpus.New(12).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("test corpus: %v", err)
	}
	s, err := store.Open(ctx, filepath.Join(dir, "state.db"), corpusPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return Deps{Engine: &engine.Engine{Store: s}}, s
}

func TestWithNoRunTheBlockSaysWhatWouldFixIt(t *testing.T) {
	d, _ := calibrationDeps(t)
	v := d.calibrationView(context.Background())
	if v.Measured {
		t.Fatal("a store with no runs reports itself as calibrated")
	}
	if strings.Contains(v.Sentence, "%") {
		t.Fatalf("no run, but a percentage appeared: %q", v.Sentence)
	}
	if !strings.Contains(v.Sentence, "kindred eval") {
		t.Fatalf("the sentence does not say what would produce a calibration: %q", v.Sentence)
	}
	for _, b := range v.Bands {
		if b.Percent != "" || b.OneIn != "" {
			t.Fatalf("band %q carries a rate with no run behind it: %+v", b.Label, b)
		}
	}
}

func TestABandNothingReachedRendersAsADashNotZeroPercent(t *testing.T) {
	ctx := context.Background()
	d, s := calibrationDeps(t)
	runID, err := s.RecordEvalRun(ctx, store.EvalRun{K: 10, Readers: 40, Evaluated: 40, Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	// Positions 1..3 only: the tail bands (4-7, 8+) were never reached.
	if err := s.RecordEvalPositions(ctx, runID, []store.EvalPosition{
		{Position: 1, Hits: 10, Observed: 40},
		{Position: 2, Hits: 4, Observed: 40},
		{Position: 3, Hits: 2, Observed: 40},
	}); err != nil {
		t.Fatal(err)
	}
	v := d.calibrationView(ctx)
	if !v.Measured || !v.Enough {
		t.Fatalf("view = %+v, want measured and enough", v)
	}
	if len(v.Bands) != 3 {
		t.Fatalf("got %d bands, want 3", len(v.Bands))
	}
	if v.Bands[0].Percent != "13%" {
		t.Fatalf("first band = %q, want 13%% (16 of 120)", v.Bands[0].Percent)
	}
	if v.Bands[1].Percent != "" || v.Bands[2].Percent != "" {
		t.Fatalf("an unreached band printed a rate: %+v / %+v", v.Bands[1], v.Bands[2])
	}
	if v.Bands[1].Observed != 0 {
		t.Fatalf("unreached band has observations: %+v", v.Bands[1])
	}
}

func TestAThinRunPrintsNoPercentageButKeepsTheCounts(t *testing.T) {
	ctx := context.Background()
	d, s := calibrationDeps(t)
	runID, err := s.RecordEvalRun(ctx, store.EvalRun{K: 10, Readers: 3, Evaluated: 3, Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvalPositions(ctx, runID, []store.EvalPosition{
		{Position: 1, Hits: 2, Observed: 3},
	}); err != nil {
		t.Fatal(err)
	}
	v := d.calibrationView(ctx)
	if v.Enough {
		t.Fatal("three readers is not enough to quote a rate")
	}
	if v.Bands[0].Percent != "" {
		t.Fatalf("a 3-reader run printed %q", v.Bands[0].Percent)
	}
	if v.Bands[0].Hits != 2 || v.Bands[0].Observed != 3 {
		t.Fatalf("the counts were dropped along with the rate: %+v", v.Bands[0])
	}
	if !strings.Contains(v.Sentence, "too few") {
		t.Fatalf("sentence does not name the sample size problem: %q", v.Sentence)
	}
}

func TestTheBlockQuotesTheNewestRun(t *testing.T) {
	ctx := context.Background()
	d, s := calibrationDeps(t)
	first, err := s.RecordEvalRun(ctx, store.EvalRun{K: 10, Readers: 40, Evaluated: 40})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvalPositions(ctx, first, []store.EvalPosition{
		{Position: 1, Hits: 4, Observed: 40},
	}); err != nil {
		t.Fatal(err)
	}
	second, err := s.RecordEvalRun(ctx, store.EvalRun{K: 10, Readers: 60, Evaluated: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvalPositions(ctx, second, []store.EvalPosition{
		{Position: 1, Hits: 30, Observed: 60},
	}); err != nil {
		t.Fatal(err)
	}
	v := d.calibrationView(ctx)
	if v.RunID != second {
		t.Fatalf("block quotes run %d, want the newest (%d)", v.RunID, second)
	}
	if v.Readers != 60 {
		t.Fatalf("readers = %d, want 60 from the newest run", v.Readers)
	}
	if v.Bands[0].Percent != "50%" {
		t.Fatalf("rate = %q, want 50%% (30 of 60)", v.Bands[0].Percent)
	}
}
