package arena

import (
	"math"
	"testing"
)

// The weights: one voter's influence on the ratings.
//
// This file exists because adding a weight to a published formula is the
// easiest way to change every existing number without noticing. The
// regression contract is that a caller who never heard of weights keeps
// producing exactly what it produced before, and the only thing that
// guarantees that is `weightOf`: the struct's zero means UNSET, so every
// pre-existing call site reads as influence 1.
//
// The other half is the part that cannot be checked by "the old tests still
// pass": a weight has to make a comparison matter LESS without making it
// matter backwards, in both places it appears. It appears twice -- once in
// the residual sum (the direction of the move) and once in the variance
// (how far the move goes) -- and getting the second one backwards moves a
// low-influence comparison FURTHER than a full-influence one. That inversion
// is invisible to any test that only asserts "weighted results differ from
// unweighted", so it is checked directly.

// unweightedVariance and unweightedResidualSum are the pre-weighting
// formulas, copied here verbatim. A test that recomputed them from the
// production functions would agree with any mutation to them; these are the
// reference.
func unweightedVariance(mu float64, ops []Opponent) float64 {
	sum := 0.0
	for _, o := range ops {
		e := Expected(mu, o.Mu, o.Phi)
		sum += g(reducePhi(o.Phi)) * g(reducePhi(o.Phi)) * e * (1.0 - e)
	}
	if sum == 0 {
		return math.Inf(1)
	}
	return 1.0 / sum
}

func unweightedResidualSum(mu float64, ops []Opponent) float64 {
	sum := 0.0
	for _, o := range ops {
		sum += g(reducePhi(o.Phi)) * (o.Score - Expected(mu, o.Mu, o.Phi))
	}
	return sum
}

// mixedOps is a small period with opponents of different ratings and
// different outcomes, so the weighting has something to disagree about.
func mixedOps(weight float64) []Opponent {
	return []Opponent{
		{Mu: 1600, Phi: 200, Score: Win, Weight: weight},
		{Mu: 1450, Phi: 260, Score: Loss, Weight: weight},
		{Mu: 1500, Phi: 350, Score: Draw, Weight: weight},
		{Mu: 1700, Phi: 120, Score: Win, Weight: weight},
	}
}

func sameRating(a, b Rating) bool {
	return a.Mu == b.Mu && a.Phi == b.Phi && a.Sigma == b.Sigma
}

// TestWeightedVarianceAndResidualReduceToStandard pins the regression
// contract at the level of the formulas: with every weight set to 1, the
// weighted functions equal the pre-weighting ones EXACTLY, not
// approximately. An epsilon here would let a mis-folded weight pass.
func TestWeightedVarianceAndResidualReduceToStandard(t *testing.T) {
	mu := 1500.0
	for _, ops := range [][]Opponent{mixedOps(1), mixedOps(0)} {
		if got, want := variance(mu, ops), unweightedVariance(mu, ops); got != want {
			t.Errorf("variance with weight %.0f = %v, want the unweighted %v",
				ops[0].Weight, got, want)
		}
		if got, want := residualSum(mu, ops), unweightedResidualSum(mu, ops); got != want {
			t.Errorf("residualSum with weight %.0f = %v, want the unweighted %v",
				ops[0].Weight, got, want)
		}
		if got, want := Delta(mu, ops), unweightedVariance(mu, ops)*unweightedResidualSum(mu, ops); got != want {
			t.Errorf("Delta with weight %.0f = %v, want the unweighted %v",
				ops[0].Weight, got, want)
		}
	}
}

// TestApplyWeightsMatchPlainWhenUnset is the plan's first test: the same
// period built twice, once with the field left at its zero value and once
// with every weight explicitly 1, must produce identical ratings.
//
// It is the one that protects every existing caller, and the two runs are
// deliberately built differently -- `mk(0)` never mentions weights, `mk(1)`
// states them -- so a future edit that treats "unset" as anything other than
// 1 shows up here rather than in production.
func TestApplyWeightsMatchPlainWhenUnset(t *testing.T) {
	mk := func(weight float64) []PeriodOutcome {
		// Work 1 beats 2, 3 beats 1, 2 draws 4: works 1 and 2 each play
		// twice, which is what makes the pre-period snapshot matter.
		return []PeriodOutcome{
			{Work: 1, Score: Win, OppWork: 2, Weight: weight},
			{Work: 2, Score: Loss, OppWork: 1, Weight: weight},
			{Work: 3, Score: Win, OppWork: 1, Weight: weight},
			{Work: 1, Score: Loss, OppWork: 3, Weight: weight},
			{Work: 2, Score: Draw, OppWork: 4, Weight: weight},
			{Work: 4, Score: Draw, OppWork: 2, Weight: weight},
		}
	}
	plain := Apply(nil, mk(0))
	stated := Apply(nil, mk(1))

	if len(plain) != len(stated) {
		t.Fatalf("unset run rated %d works, explicit-1 run rated %d", len(plain), len(stated))
	}
	for id, p := range plain {
		s, ok := stated[id]
		if !ok {
			t.Fatalf("work %d rated in the unset run and not in the explicit-1 run", id)
		}
		if !sameRating(p, s) {
			t.Errorf("work %d: unset weight gave %+v, explicit weight 1 gave %+v -- "+
				"zero must mean UNSET, or every caller that predates weighting changed "+
				"its numbers", id, p, s)
		}
	}
}

// TestWeightScalesVarianceAndResidualExactly pins the weight in BOTH places
// it belongs, independently, by their exact scaling laws.
//
// With a uniform weight w, both formulas are homogeneous of degree one in
// the weights, so they have to scale by exactly w (the residual) and 1/w
// (the variance, which is a reciprocal):
//
//	residualSum(w) / residualSum(1) = w
//	variance(w)    / variance(1)    = 1/w
//
// Two assertions rather than one because the interesting mutation -- leaving
// the weight on one side of the maths and not the other -- is invisible to
// any test that only checks "the result differs", and each of these goes to
// 1 (or flakes to 1/w) under exactly one of the two omissions. The
// tolerance is not slack in the claim: the weight is folded into a sum term
// by term, so the identity holds to rounding rather than bit for bit. The
// mutations are orders of magnitude away from it.
func TestWeightScalesVarianceAndResidualExactly(t *testing.T) {
	const mu = 1500.0
	ones := mixedOps(1)
	baseVar, baseRes := variance(mu, ones), residualSum(mu, ones)

	for _, w := range []float64{0.5, 0.3, 0.25} {
		weighted := mixedOps(w)
		if got, want := variance(mu, weighted)/baseVar, 1/w; math.Abs(got-want) > 1e-9 {
			t.Errorf("weight %.2f: variance scaled by %v, want 1/w = %v -- the "+
				"weight is missing from the variance's sum, and v is what sets "+
				"phi' in step 7", w, got, want)
		}
		if got, want := residualSum(mu, weighted)/baseRes, w; math.Abs(got-want) > 1e-9 {
			t.Errorf("weight %.2f: residual sum scaled by %v, want w = %v -- the "+
				"weight is missing from the evidence", w, got, want)
		}
	}
}

// TestLowWeightMovesLessThanFullWeight is the property behind "opposite
// taste -> minimal change", stated as a measurement rather than as a
// formula: the same result against the same opponent, at a lower weight,
// moves the rating less.
//
// The measurement is not a restatement of the scaling above, because the
// step-7 update is mu' = mu + phi'^2 * residualSum: the weight enters twice
// with opposite effects (v grows, so phi' shrinks, while the evidence
// shrinks), and which way the product lands is not something to reason out
// -- it is measured here (a single opponent: 78.8 at weight 1, 23.0 at
// weight 0.25).
//
// Worth knowing while reading it: Delta is INVARIANT under a uniform weight
// (the w cancels between v and the sum), so a test written against Delta
// would show no effect at all and pass for the wrong reason.
func TestLowWeightMovesLessThanFullWeight(t *testing.T) {
	// A single opponent this player beat, at the plan's influence values:
	// 1.0 is full agreement, 0.25 is agreement 0.5.
	move := func(w float64) float64 {
		before := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}
		after := before.Update([]Opponent{{Mu: 1500, Phi: 200, Score: Win, Weight: w}})
		return after.Mu - before.Mu
	}

	full := move(1.0)
	quarter := move(0.25)
	if !(full > 0) {
		t.Fatalf("a win at full weight moved the rating by %v, want a positive move", full)
	}
	if !(quarter > 0) {
		t.Fatalf("a win at weight 1/4 moved the rating by %v, want a positive move", quarter)
	}
	if !(quarter < full) {
		t.Errorf("weight 1/4 moved the rating by %v and full weight by %v: a "+
			"lower-influence comparison must move it LESS", quarter, full)
	}
}

// TestUpdateHandlesMixedWeights: symmetric periods at two weights, and one
// deliberately asymmetric period, in the same code path.
//
// The asymmetry is the interesting case: a full-weight win against a
// light-weight loss must move the rating UP, because the win is the
// comparison the owner's taste endorses. A weighting applied to the period
// as a whole rather than per comparison cannot produce that.
func TestUpdateHandlesMixedWeights(t *testing.T) {
	before := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}
	pair := func(w float64) []Opponent {
		return []Opponent{
			{Mu: 1600, Phi: 200, Score: Win, Weight: w},
			{Mu: 1400, Phi: 200, Score: Loss, Weight: w},
		}
	}
	heavy := before.Update(pair(1.0))
	light := before.Update(pair(0.3))
	mixed := before.Update([]Opponent{
		{Mu: 1600, Phi: 200, Score: Win, Weight: 1.0},
		{Mu: 1400, Phi: 200, Score: Loss, Weight: 0.3},
	})

	for name, r := range map[string]Rating{"heavy": heavy, "light": light, "mixed": mixed} {
		if math.IsNaN(r.Mu) || math.IsNaN(r.Phi) || math.IsInf(r.Phi, 0) {
			t.Errorf("%s run produced a non-finite rating: %+v", name, r)
		}
		if r.Phi <= 0 || r.Phi >= before.Phi {
			t.Errorf("%s run: phi = %v, want a shrink from %v that stays positive",
				name, r.Phi, before.Phi)
		}
	}

	// Symmetric pairs: equal and opposite, so mu must not move -- at ANY
	// weight. A weight that reached only one side would break this.
	if math.Abs(heavy.Mu-before.Mu) > 1e-9 {
		t.Errorf("symmetric pair at full weight moved mu from %v to %v", before.Mu, heavy.Mu)
	}
	if math.Abs(light.Mu-before.Mu) > 1e-9 {
		t.Errorf("symmetric pair at weight 0.3 moved mu from %v to %v; the weight "+
			"belongs to the comparison, not the winner", before.Mu, light.Mu)
	}
	// Less evidence, less shrink.
	if !(light.Phi > heavy.Phi) {
		t.Errorf("light period left phi at %v and the heavy one at %v: a "+
			"lower-influence period is less evidence and must shrink the "+
			"deviation less", light.Phi, heavy.Phi)
	}
	// And the asymmetric pair moves toward the endorsed side -- but not as
	// far as the endorsed comparison alone, because the light-weight loss
	// still pulls. Both halves matter: the first says the weight did not
	// silence the endorsed result, the second says it did not amplify it
	// either.
	winOnly := before.Update([]Opponent{{Mu: 1600, Phi: 200, Score: Win, Weight: 1.0}})
	if !(mixed.Mu > before.Mu) {
		t.Errorf("a full-weight win against a light-weight loss moved mu from %v to "+
			"%v, want an upward move: the endorsed comparison is the one that "+
			"should count", before.Mu, mixed.Mu)
	}
	if !(mixed.Mu < winOnly.Mu) {
		t.Errorf("the asymmetric period moved mu to %v, at least as far as the "+
			"endorsed win alone (%v): the light-weight loss must still pull, or "+
			"a weight below the floor is doing nothing at all",
			mixed.Mu, winOnly.Mu)
	}
}

// TestOpponentsForSharesTheWeightBetweenBothSides: the weight belongs to
// the comparison, so both works have to see it. If only one side carried
// it, a pair would move by two different amounts and the loser of a
// low-influence comparison would be updated as though the comparison were
// fully influential.
func TestOpponentsForSharesTheWeightBetweenBothSides(t *testing.T) {
	ops := OpponentsFor([]PeriodOutcome{{Work: 1, Score: Win, OppWork: 2, Weight: 0.3}}, nil)

	if len(ops[1]) != 1 || len(ops[2]) != 1 {
		t.Fatalf("both works must get one opponent, got %d and %d", len(ops[1]), len(ops[2]))
	}
	if ops[1][0].Weight != 0.3 || ops[2][0].Weight != 0.3 {
		t.Errorf("weights reached the two sides as %v and %v, want 0.3 on both: the "+
			"weight is a property of the comparison, not of the winner",
			ops[1][0].Weight, ops[2][0].Weight)
	}
	if ops[2][0].Score != Loss {
		t.Errorf("the loser's score against the winner = %v, want %v",
			ops[2][0].Score, Loss)
	}
}
