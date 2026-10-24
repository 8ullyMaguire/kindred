package arena

// The BT fit (#53), tested by properties rather than by re-implementing the formula.
//
// A test that recomputes `num/den` asserts that the code does what the code does. These
// assert the properties a Bradley-Terry fit must have if it is the model anyone claims
// it is: symmetry, transitivity, finite at a perfect record, invariant to how many
// times the same comparison is repeated, and a rank correlation that behaves.

import (
	"math"
	"testing"
)

func duel(w, l int64) Duel { return Duel{Winner: w, Loser: l, Weight: 1} }

// Symmetry: two items that each won once did not differ, and no tie-break may invent a
// difference. This is the property a floating-point fixed point most often loses.
func TestSymmetricDuelsGiveEqualStrength(t *testing.T) {
	p, _, ok := FitBT([]Duel{duel(1, 2), duel(2, 1)})
	if !ok {
		t.Fatal("did not converge")
	}
	if math.Abs(p[1]-p[2]) > 1e-9 {
		t.Fatalf("1 = %v, 2 = %v: equal records gave different strengths", p[1], p[2])
	}
}

// Transitivity: A beat B, B beat C, A beat C. Order must follow.
func TestAChainOrdersItself(t *testing.T) {
	p, _, ok := FitBT([]Duel{duel(1, 2), duel(2, 3), duel(1, 3)})
	if !ok {
		t.Fatal("did not converge")
	}
	if !(p[1] > p[2] && p[2] > p[3]) {
		t.Fatalf("chain gave %v > %v > %v, want 1 > 2 > 3", p[1], p[2], p[3])
	}
}

// The one that breaks the MLE: an item that never lost. Without the Jeffreys term the
// fixed point runs to +Inf and the table has one row nobody can read.
func TestAPerfectRecordStaysFinite(t *testing.T) {
	p, _, ok := FitBT([]Duel{duel(1, 2), duel(1, 2), duel(1, 2), duel(1, 2), duel(1, 2)})
	if !ok {
		t.Fatal("did not converge")
	}
	for id, v := range p {
		if math.IsInf(v, 0) || math.IsNaN(v) || v > 1e6 {
			t.Fatalf("item %d has strength %v: a perfect record ran away", id, v)
		}
	}
	if p[1] <= p[2] {
		t.Fatalf("the undefeated item (%v) is not above the one it beat (%v)", p[1], p[2])
	}
}

// The pure MLE is scale-invariant: duplicating every comparison cannot change it,
// because it is a statement about ratios. This pins the MM fixed point itself, with no
// prior in the way.
func TestThePlainMLEIsInvariantToDuplication(t *testing.T) {
	// Every item wins at least once. Without a prior an item that never won has a
	// maximum-likelihood strength of exactly 0 (and one that never lost, +Inf), so the
	// prior-free model is only defined on a table where both degeneracies are absent --
	// which is why the shipped fit uses the prior and this test names its own data.
	base := []Duel{duel(1, 2), duel(2, 3), duel(3, 1), duel(1, 3)}
	twice := make([]Duel, 0, 2*len(base))
	for _, d := range base {
		twice = append(twice, d, d)
	}
	a, _, ok := FitBTWithPrior(base, 0)
	if !ok {
		t.Fatal("did not converge")
	}
	b, _, _ := FitBTWithPrior(twice, 0)
	for id := range a {
		if rel := math.Abs(a[id]-b[id]) / a[id]; rel > 1e-9 {
			t.Fatalf("item %d: %v became %v when every duel was doubled (rel %.2g)",
				id, a[id], b[id], rel)
		}
	}
}

// With the prior, duplication is NOT invariance -- it is more evidence, and the fit must
// move AWAY from the prior and toward the item's own record. An implementation whose
// prior does not shrink with the data would pass the invariance test and be wrong: it
// would let one comparison outweigh a thousand.
func TestWithThePriorMoreEvidenceSharpens(t *testing.T) {
	base := []Duel{duel(1, 2), duel(2, 1), duel(1, 2), duel(1, 3), duel(3, 2)}
	twice := make([]Duel, 0, 2*len(base))
	for _, d := range base {
		twice = append(twice, d, d)
	}
	a, _, _ := FitBT(base)
	b, _, _ := FitBT(twice)
	if !(b[1] > a[1]) {
		t.Fatalf("the winner's strength went %v -> %v when its record doubled: the "+
			"prior is not giving way to the data", a[1], b[1])
	}
	if !(b[2] < a[2]) {
		t.Fatalf("the loser's strength went %v -> %v when the loss was doubled", a[2], b[2])
	}
}

// A draw is half a win and half a loss, and a table of draws must land at the prior --
// exactly 1 after normalisation -- rather than at some arbitrary value the half-weight
// happens to produce.
func TestDrawsAloneSitAtThePrior(t *testing.T) {
	// One draw between 1 and 2, expressed the way DuelsFromOutcomes expresses it.
	duels := DuelsFromOutcomes([]PeriodOutcome{
		{Work: 1, OppWork: 2, Score: Draw, Weight: 1},
		{Work: 2, OppWork: 1, Score: Draw, Weight: 1},
	})
	// The caller states each item's own outcome; stating both sides of one draw means
	// one duel each way, at half weight, which is what this has.
	p, _, ok := FitBT(duels)
	if !ok {
		t.Fatal("did not converge")
	}
	for id, v := range p {
		if math.Abs(v-1) > 1e-6 {
			t.Fatalf("item %d = %v after a draw, want the prior 1", id, v)
		}
	}
}

// DuelsFromOutcomes must state each outcome from the item's own side, not the winner's.
func TestOutcomesBecomeDuelsFromEachSide(t *testing.T) {
	out := DuelsFromOutcomes([]PeriodOutcome{
		{Work: 7, OppWork: 9, Score: Win, Weight: 1},
		{Work: 7, OppWork: 9, Score: Loss, Weight: 1},
	})
	if len(out) != 2 {
		t.Fatalf("two outcomes became %d duels", len(out))
	}
	if out[0].Winner != 7 || out[0].Loser != 9 {
		t.Fatalf("a win for 7 became %+v", out[0])
	}
	// A loss for 7 is a win for 9 -- the direction is what makes the fit possible.
	if out[1].Winner != 9 || out[1].Loser != 7 {
		t.Fatalf("a loss for 7 became %+v", out[1])
	}
	// Self-comparisons and negative weights are dropped, not inverted.
	if got := DuelsFromOutcomes([]PeriodOutcome{
		{Work: 4, OppWork: 4, Score: Win, Weight: 1},
		{Work: 4, OppWork: 5, Score: Win, Weight: -1},
	}); len(got) != 0 {
		t.Fatalf("degenerate outcomes produced %+v", got)
	}
}

// Spearman: the ends are exact, ties average, and n < 2 is reported as n rather than as
// a correlation of zero.
func TestSpearmanEndsAndTies(t *testing.T) {
	a := map[int64]float64{1: 1, 2: 2, 3: 3, 4: 4}
	same := map[int64]float64{1: 10, 2: 20, 3: 30, 4: 40}
	if rho, n := Spearman(a, same); math.Abs(rho-1) > 1e-12 || n != 4 {
		t.Fatalf("identical order: rho %v n %d", rho, n)
	}
	rev := map[int64]float64{1: 40, 2: 30, 3: 20, 4: 10}
	if rho, _ := Spearman(a, rev); math.Abs(rho+1) > 1e-12 {
		t.Fatalf("reversed order: rho %v", rho)
	}
	// Every value tied on one side: there is no ordering to correlate with.
	flat := map[int64]float64{1: 5, 2: 5, 3: 5, 4: 5}
	if rho, n := Spearman(a, flat); rho != 0 || n != 4 {
		t.Fatalf("constant side: rho %v n %d, want 0 and 4", rho, n)
	}
	if rho, n := Spearman(map[int64]float64{1: 1}, map[int64]float64{1: 2}); rho != 0 || n != 1 {
		t.Fatalf("single item: rho %v n %d", rho, n)
	}
	// Disjoint keys: nothing shared, so nothing to correlate.
	if rho, n := Spearman(map[int64]float64{1: 1}, map[int64]float64{2: 2}); rho != 0 || n != 0 {
		t.Fatalf("disjoint: rho %v n %d", rho, n)
	}
	// Ties average: with ranks 1.5/1.5 for the tied pair the correlation with a
	// distinct ordering is finite and below 1 rather than undefined.
	tied := map[int64]float64{1: 5, 2: 5, 3: 30, 4: 40}
	if rho, _ := Spearman(a, tied); !(rho > 0.8 && rho < 1) {
		t.Fatalf("a tied ordering correlated at %v with the clean one", rho)
	}
}
