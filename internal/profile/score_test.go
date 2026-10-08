package profile

import (
	"math"
	"testing"
)

func TestSpearmanIsOneForAMonotoneRelationship(t *testing.T) {
	// Spearman is a rank statistic, so any strictly increasing relationship
	// must score exactly 1 no matter how the values are scaled. If this fails,
	// the implementation is correlating the raw values and the whole
	// validation is measuring something else.
	a := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	b := []float64{2, 4, 6, 8, 10, 12, 14, 16}
	if got := Spearman(a, b); math.Abs(got-1) > 1e-12 {
		t.Errorf("Spearman = %v, want 1", got)
	}
}

func TestSpearmanDoesNotCareAboutTheValueScale(t *testing.T) {
	// This is the property that makes Spearman the right statistic for
	// ratings: a model that predicts 8.0 vs 8.1 correctly but compresses
	// everything into [0,1] must score the same as one predicting on the
	// rating scale directly.
	a := []float64{9, 4, 7, 1, 8, 3}
	b := []float64{0.91, 0.42, 0.70, 0.11, 0.83, 0.30}
	if got := Spearman(a, b); math.Abs(got-1) > 1e-12 {
		t.Errorf("Spearman = %v, want 1 for a rescaled copy", got)
	}
}

// A constant predictor must score NaN, not 0 and not a spurious positive.
//
// This is the case that catches the classic implementation bug: assigning
// ordinal ranks 1..n to tied values invents a perfect ordering out of nothing
// and reports a large positive correlation. NaN is the honest answer -- the
// model has no information -- and the verdict line keys off it.
func TestSpearmanRejectsAFlatPredictor(t *testing.T) {
	predicted := make([]float64, 20)
	actual := make([]float64, 20)
	for i := range actual {
		actual[i] = float64(i%10) + 1
	}
	got := Spearman(predicted, actual)
	if !math.IsNaN(got) {
		t.Errorf("Spearman against a flat predictor = %v, want NaN", got)
	}
}

// Ties get averaged ranks. Two values tied at ordinal ranks 2 and 3 both take
// 2.5, which keeps the rank sum at n(n+1)/2 -- if they took 2 and 3 instead,
// every downstream correlation would be subtly off.
//
// The second half checks that a tie does not by itself change any pair's order:
// a=[1,2,2,3] and c=[1,2,2,4] have IDENTICAL rank vectors, so a perfect
// correlation is the correct answer, not a bug. A model that ranks ties by
// position rather than by value would score these below 1 and invent an
// ordering that is not in the data.
func TestSpearmanAveragesTiedRanks(t *testing.T) {
	a := []float64{1, 2, 2, 3}
	if got := Spearman(a, a); math.Abs(got-1) > 1e-12 {
		t.Errorf("Spearman on tied series = %v, want 1", got)
	}
	c := []float64{1, 2, 2, 4}
	if got := Spearman(a, c); math.Abs(got-1) > 1e-12 {
		t.Errorf("Spearman across a tie = %v, want 1: the rank vectors are identical", got)
	}
	// A genuine order disagreement must still be penalised.
	d := []float64{1, 3, 2, 2}
	if got := Spearman(a, d); got >= 0.99 {
		t.Errorf("Spearman with an inverted pair = %v, want well below 1", got)
	}
	// And the averaged ranks must sum to n(n+1)/2, which is the invariant the
	// averaging exists to preserve.
	for _, series := range [][]float64{a, c, d, {5, 5, 5, 5, 5, 5, 5, 5, 5}} {
		sum := 0.0
		for _, r := range rankWithTies(series) {
			sum += r
		}
		n := float64(len(series))
		if want := n * (n + 1) / 2; math.Abs(sum-want) > 1e-12 {
			t.Errorf("ranks of %v sum to %v, want %v", series, sum, want)
		}
	}
}

func TestSpearmanIsNegativeWhenInverted(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{5, 4, 3, 2, 1}
	if got := Spearman(a, b); math.Abs(got+1) > 1e-12 {
		t.Errorf("Spearman on an inverted pair = %v, want -1", got)
	}
}

func TestSpearmanRejectsMismatchedLengths(t *testing.T) {
	if got := Spearman([]float64{1, 2, 3}, []float64{1, 2}); !math.IsNaN(got) {
		t.Errorf("Spearman on mismatched lengths = %v, want NaN", got)
	}
	if got := Spearman([]float64{1}, []float64{1}); !math.IsNaN(got) {
		t.Errorf("Spearman on one point = %v, want NaN", got)
	}
}

// Tau is the concordance correlation: it counts only pair ORDER. Spearman is a
// linear function of ranks, so on a monotone pair the two agree exactly -- a
// model that gets every pair right but puts the 10-rated fic at 100 instead of
// 10 has still ranked everything correctly. The divergence shows up when one
// work is mis-ranked, and Tau is then the stabler of the two.
func TestKendallTauMeasuresPairOrderOnly(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{1, 2, 3, 4, 5}
	if got := KendallTau(a, b); math.Abs(got-1) > 1e-12 {
		t.Errorf("KendallTau on an identical pair = %v, want 1", got)
	}
	// One work badly misplaced among eight: values 4 and 8 trade places, which
	// leaves five of the seven other elements in order. That is 7 discordant
	// pairs of 28, so tau = 0.5 exactly.
	//
	// Tau exceeds rho here (0.438) and that is the point of printing both: tau
	// charges a fixed 2/n^2 for each misordered pair and so barely notices a
	// single work thrown far out of place, while rho is linear in ranks and
	// notices it a lot. One badly-wrong work should not cost the whole
	// profile its rating, and tau is the number that says so.
	c := []float64{1, 2, 3, 8, 5, 6, 7, 4}
	tau, rho := KendallTau(a, c), Spearman(a, c)
	if math.Abs(tau-0.5) > 1e-12 {
		t.Errorf("tau = %v, want 0.5 (7 discordant of 28 pairs)", tau)
	}
	if rho >= tau {
		t.Errorf("rho (%v) should sit below tau (%v): one misplaced work drags rho, not tau", rho, tau)
	}
}

func TestKendallTauOnAnInvertedPair(t *testing.T) {
	a := []float64{1, 2, 3, 4}
	b := []float64{4, 3, 2, 1}
	if got := KendallTau(a, b); math.Abs(got+1) > 1e-12 {
		t.Errorf("KendallTau on an inverted pair = %v, want -1", got)
	}
}

// A tie counts as neither concordant nor discordant, which is what keeps tau
// from reporting a spurious disagreement on data with genuine ties.
func TestKendallTauIgnoresTiedPairs(t *testing.T) {
	a := []float64{1, 2, 2, 3}
	b := []float64{1, 2, 2, 3}
	if got := KendallTau(a, b); math.Abs(got-1) > 1e-12 {
		t.Errorf("KendallTau on tied series = %v, want 1", got)
	}
}

// The length divisor is the load-bearing part of Score: without it a fic with
// 40 tags sums far more positive weights than a fic with 4 and wins on volume
// alone. The divisor is a DAMPING, not a preference -- 4 tags / sqrt(4) = 2
// still beats 2 tags / sqrt(2) = 1.41 -- so what is tested is that the gap
// shrinks with length, by exactly the sqrt factor.
func TestScoreDividesByTagCount(t *testing.T) {
	weights := map[int32]float64{1: 1.0, 2: 1.0, 3: 1.0, 4: 1.0}

	sTwo, _ := Score(weights, []int32{1, 2})        // 2/sqrt(2) = 1.414
	sFour, _ := Score(weights, []int32{1, 2, 3, 4}) // 4/sqrt(4) = 2
	if math.Abs(sTwo-2/math.Sqrt2) > 1e-12 || math.Abs(sFour-2) > 1e-12 {
		t.Fatalf("scores = %v and %v, want 2/sqrt(2) and 4/sqrt(4)", sTwo, sFour)
	}
	// The ratio between the two lengths is 4/sqrt(4) : 2/sqrt(2) = sqrt(2).
	if ratio := sFour / sTwo; math.Abs(ratio-math.Sqrt2) > 1e-9 {
		t.Errorf("ratio = %v, want sqrt(2) = %v", ratio, math.Sqrt2)
	}
	// The point of the divisor: the raw sums would say the 4-tag work is TWICE
	// as good (4 vs 2), so the ratio would be exactly 2.0. Normalisation brings
	// it to sqrt(2) = 1.41 -- it damps the length advantage, it does not
	// invert it, and a longer work with more positive tags still wins.
	if ratio := sFour / sTwo; ratio >= 2.0 {
		t.Errorf("ratio %v leaves the length advantage undamped; want sqrt(2) = 1.414", ratio)
	}
	if sFour <= sTwo {
		t.Error("a longer work with more positive tags should still score above a shorter one")
	}
}

func TestScoreIgnoresUnknownAndDuplicateTags(t *testing.T) {
	weights := map[int32]float64{1: 0.5}
	// 99 is not in the profile and 1 is repeated: neither may move the score.
	got, hits := Score(weights, []int32{1, 99, 1})
	if hits != 1 {
		t.Errorf("hits = %d, want 1 (a repeated tag is not a second hit)", hits)
	}
	want := 0.5 / math.Sqrt(3)
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("Score = %v, want %v", got, want)
	}
}

func TestScoreOnNoOverlapIsZeroNotNegative(t *testing.T) {
	weights := map[int32]float64{1: -0.9}
	got, hits := Score(weights, []int32{7, 8})
	if got != 0 || hits != 0 {
		t.Errorf("Score = %v with %d hits, want 0 and 0", got, hits)
	}
}

// Negative weights must pull the score down. A scorer that summed absolute
// values, or clipped at zero, would rank a disliked fic as neutral and lose
// the entire reason for signed weights.
func TestScoreIsSigned(t *testing.T) {
	disliked := map[int32]float64{1: -0.9, 2: -0.8}
	liked := map[int32]float64{1: 0.9, 2: 0.8}
	tags := []int32{1, 2}
	sDisliked, _ := Score(disliked, tags)
	sLiked, _ := Score(liked, tags)
	if sDisliked >= 0 {
		t.Errorf("a disliked work scored %v, want negative", sDisliked)
	}
	if sLiked <= sDisliked {
		t.Errorf("liked %v is not above disliked %v", sLiked, sDisliked)
	}
}

func TestScoreOnEmptyInput(t *testing.T) {
	if got, hits := Score(map[int32]float64{1: 1}, nil); got != 0 || hits != 0 {
		t.Errorf("Score(nil tags) = %v/%d, want 0/0", got, hits)
	}
	if got, hits := Score(nil, []int32{1}); got != 0 || hits != 0 {
		t.Errorf("Score(nil weights) = %v/%d, want 0/0", got, hits)
	}
}
