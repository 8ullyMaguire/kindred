package eval

// Length fit claims.
//
//   The comparison is against the HELD-OUT works, not the seeds the signal saw.
//   Otherwise the measurement is "the list matches the thing that shaped it",
//   which is true by construction.
//
//   An unmeasurable side is not a zero. A reader whose list carries no lengths, or
//   whose held-out works carry none, contributes no observation and is counted out
//   loud — never averaged in as "the lengths were infinitely far apart".
//
//   The medians are medians: one 400k-word epic in a reader's test set must not set
//   the target for the whole comparison.

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestAListAtTheReadersOwnLengthScoresOne(t *testing.T) {
	words := map[int64]int{1: 10000, 2: 12000, 3: 11000}
	fit, ok := MeasureLengthFit([]int64{1, 2}, []int64{3}, words)
	if !ok {
		t.Fatal("both sides had lengths; the measurement refused")
	}
	if math.Abs(fit.Value-1) > 0.01 {
		t.Fatalf("fit = %.4f, want ~1 (returned %.0f, held out %.0f)", fit.Value, fit.Returned, fit.HeldOut)
	}
}

func TestHalfOrDoubleTheLengthScoresHalf(t *testing.T) {
	words := map[int64]int{1: 5000, 2: 10000}
	fit, ok := MeasureLengthFit([]int64{1}, []int64{2}, words)
	if !ok {
		t.Fatal("refused a measurable pair")
	}
	if math.Abs(fit.Value-0.5) > 0.01 {
		t.Fatalf("2x ratio scored %.4f, want 0.5", fit.Value)
	}
	double, _ := MeasureLengthFit([]int64{2}, []int64{1}, words)
	if math.Abs(double.Value-fit.Value) > 1e-9 {
		t.Fatalf("halving scored %.4f and doubling %.4f; the distance is symmetric", double.Value, fit.Value)
	}
}

func TestUnmeasurableSidesAreNotZeros(t *testing.T) {
	words := map[int64]int{2: 40000}
	if _, ok := MeasureLengthFit([]int64{1}, []int64{2}, words); ok {
		t.Fatal("returned list had no length, but the measurement produced a number")
	}
	if _, ok := MeasureLengthFit([]int64{2}, []int64{1}, words); ok {
		t.Fatal("held-out side had no length, but the measurement produced a number")
	}
	if _, ok := MeasureLengthFit(nil, []int64{2}, words); ok {
		t.Fatal("an empty list measured as a fit")
	}
}

func TestZeroAndNegativeCountsAreAbsentNotShort(t *testing.T) {
	words := map[int64]int{1: 0, 2: -5, 3: 10000}
	fit, ok := MeasureLengthFit([]int64{1, 2, 3}, []int64{3}, words)
	if !ok {
		t.Fatal("one work carried a real length; the measurement refused")
	}
	if fit.Items != 1 {
		t.Fatalf("items = %d, want 1: a zero or negative count is not a length", fit.Items)
	}
}

func TestTheMediansAreMedians(t *testing.T) {
	// Returned: 5k, 5k, 5k, 400k -> median 5k. Held out: 5k -> 5k. A mean on the
	// left would be 103,750 and the fit would be ~0.06.
	words := map[int64]int{1: 5000, 2: 5000, 3: 5000, 4: 400000, 5: 5000}
	fit, ok := MeasureLengthFit([]int64{1, 2, 3, 4}, []int64{5}, words)
	if !ok {
		t.Fatal("refused")
	}
	if fit.Value < 0.99 {
		t.Fatalf("fit = %.4f with a median of 5k on both sides: the mean is being used", fit.Value)
	}
}

// The end-to-end wiring: a recommender that can answer for words gets measured,
// and one that cannot says so instead of reporting 0.
type wordyRec struct {
	lists map[int64][]int64
	words map[int64]int
	noSrc bool
}

func (w wordyRec) Recommend(_ context.Context, train []int64, k int) ([]int64, error) {
	if len(train) == 0 {
		return nil, nil
	}
	ids := w.lists[train[0]]
	if len(ids) > k {
		ids = ids[:k]
	}
	out := make([]int64, len(ids))
	copy(out, ids)
	return out, nil
}

type plainRec struct{ lists map[int64][]int64 }

func (p plainRec) Recommend(_ context.Context, train []int64, k int) ([]int64, error) {
	if len(train) == 0 {
		return nil, nil
	}
	ids := p.lists[train[0]]
	if len(ids) > k {
		ids = ids[:k]
	}
	out := make([]int64, len(ids))
	copy(out, ids)
	return out, nil
}

type wordSource map[int64]int

func (w wordSource) WordCounts(_ context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	for _, id := range ids {
		if v, ok := w[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// withWords wraps a recommender so it satisfies WordCountSource without changing
// its ranking: the two interfaces are separate on purpose.
type withWords struct {
	rec   Recommender
	words map[int64]int
}

func (w withWords) Recommend(ctx context.Context, train []int64, k int) ([]int64, error) {
	return w.rec.Recommend(ctx, train, k)
}

func (w withWords) WordCounts(ctx context.Context, ids []int64) (map[int64]int, error) {
	return wordSource(w.words).WordCounts(ctx, ids)
}

func TestRunMeasuresLengthFitForARecommenderThatCanAnswer(t *testing.T) {
	split := Split{
		Train: map[int64][]int64{1: {10}, 2: {20}},
		Test:  map[int64][]int64{1: {100}, 2: {200}},
	}
	rec := withWords{
		rec: plainRec{lists: map[int64][]int64{10: {900, 901}, 20: {902, 903}}},
		words: map[int64]int{
			900: 10000, 901: 12000, // list 1: median 11k
			100: 10500,           // held out 1: 10.5k -> fit ~1
			902: 5000, 903: 5000, // list 2: 5k
			200: 40000, // held out 2: 40k -> fit 0.5
		},
	}
	rep, err := Run(context.Background(), rec, split, 4)
	if err != nil {
		t.Fatal(err)
	}
	if rep.LengthRuns != 2 {
		t.Fatalf("length runs = %d, want 2", rep.LengthRuns)
	}
	// 11000 vs 10500, and 5000 vs 40000: the ratio of the smaller to the larger,
	// averaged over the two readers.
	want := (10500.0/11000.0 + 5000.0/40000.0) / 2
	if math.Abs(rep.LengthFit-want) > 0.02 {
		t.Fatalf("length fit = %.4f, want ~%.4f", rep.LengthFit, want)
	}
}

func TestARecommenderWithNoWordCountsReportsWhy(t *testing.T) {
	split := Split{
		Train: map[int64][]int64{1: {10}},
		Test:  map[int64][]int64{1: {100}},
	}
	rep, err := Run(context.Background(), plainRec{lists: map[int64][]int64{10: {100}}}, split, 4)
	if err != nil {
		t.Fatal(err)
	}
	if rep.LengthRuns != 0 || rep.LengthFit != 0 {
		t.Fatalf("length fit = %.3f over %d runs with no word source", rep.LengthFit, rep.LengthRuns)
	}
	if !strings.Contains(rep.LengthSkipped, "word counts") {
		t.Fatalf("no reason recorded: %q", rep.LengthSkipped)
	}
}
