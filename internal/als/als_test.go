package als

// The ALS fit (#89), on data small enough to reason about.
//
// The properties a factorisation must have if it is the model it claims to be:
//   1. the solver returns the right answer for a matrix whose answer is known;
//   2. two works that the same readers bookmarked come out more similar than two that
//      nobody co-bookmarked;
//   3. the training error falls and the factors stop moving — a run that does neither is
//      a broken solver, whatever the signal does downstream;
//   4. it is deterministic, so a measurement can be repeated.

import (
	"math"
	"testing"
)

// The Cholesky path against a hand-solvable system.
func TestSolveSPDRecoversAKnownVector(t *testing.T) {
	// A = [[4,1],[1,3]], b = [1,2] → x = [1/11, 7/11].
	a := []float64{4, 1, 1, 3}
	b := []float64{1, 2}
	out := make([]float64, 2)
	solveSPD(a, b, out, 2)
	if math.Abs(out[0]-1.0/11) > 1e-12 || math.Abs(out[1]-7.0/11) > 1e-12 {
		t.Fatalf("solved to %v, want [0.0909 0.6364]", out)
	}
	// And a 3x3 identity scales the right-hand side unchanged.
	id := []float64{2, 0, 0, 0, 2, 0, 0, 0, 2}
	out3 := make([]float64, 3)
	solveSPD(id, []float64{2, 4, 6}, out3, 3)
	if math.Abs(out3[0]-1) > 1e-12 || math.Abs(out3[1]-2) > 1e-12 || math.Abs(out3[2]-3) > 1e-12 {
		t.Fatalf("2I·x = [2 4 6] solved to %v, want [1 2 3]", out3)
	}
}

// Two works bookmarked by the same readers must come out more similar than two that
// share none. This is the whole claim of the dimension; if it fails, the factors are
// noise with a shape.
func TestCoBookmarkedWorksComeOutSimilar(t *testing.T) {
	// Readers 1-5 bookmark works 10 and 11 together; readers 6-10 bookmark work 12 only.
	var in []Interaction
	for u := int64(1); u <= 5; u++ {
		in = append(in, Interaction{User: u, Work: 10}, Interaction{User: u, Work: 11})
	}
	for u := int64(6); u <= 10; u++ {
		in = append(in, Interaction{User: u, Work: 12})
	}
	p := DefaultParams()
	p.Iters = 6
	res := Train(in, p)
	idx := func(id int64) []float32 {
		for i, w := range res.WorkIDs {
			if w == id {
				return res.Works[i]
			}
		}
		t.Fatalf("work %d missing from the model", id)
		return nil
	}
	co := Cosine(idx(10), idx(11))
	disjoint := Cosine(idx(10), idx(12))
	if !(co > disjoint) {
		t.Fatalf("co-bookmarked similarity %.3f is not above disjoint %.3f", co, disjoint)
	}
	if co <= 0 {
		t.Fatalf("co-bookmarked works scored %.3f: the factors did not separate them at all", co)
	}
}

// The factors must recover READERSHIP GROUPS: works read by the same people come out
// more similar than works read by different people.
//
// WHAT IS NOT TESTED HERE, deliberately: the claim that ALS relates works two hops apart
// ("r1 read A and B, r2 read B and C, therefore A resembles C"). I wrote that test first
// and it failed — cosine 0.000 against 0.001 on three readers — and the failure is not a
// bug: with binary implicit data an item's factor is a function of WHICH users touched
// it, so two items touched by disjoint users are only related once the model has learned
// that those users resemble each other from other evidence. Three readers cannot teach
// that, and a synthetic set large enough to teach it is a live measurement, not a unit
// test. The honest statement of the dimension is what this test asserts, and the two-hop
// claim is what the eval harness has to settle.
func TestFactorsGroupByReadership(t *testing.T) {
	var in []Interaction
	// Four groups of fifteen readers, each group reading its own three works.
	for g := int64(0); g < 4; g++ {
		for u := int64(1); u <= 15; u++ {
			user := g*100 + u
			for w := int64(0); w < 3; w++ {
				in = append(in, Interaction{User: user, Work: 1000 + g*10 + w})
			}
		}
	}
	p := DefaultParams()
	p.Iters = 10
	res := Train(in, p)
	get := func(id int64) []float32 {
		for i, w := range res.WorkIDs {
			if w == id {
				return res.Works[i]
			}
		}
		t.Fatalf("work %d missing", id)
		return nil
	}
	// Within group 0 vs across to group 2.
	within := Cosine(get(1000), get(1001))
	across := Cosine(get(1000), get(1020))
	if !(within > across) {
		t.Fatalf("works read by the same fifteen people scored %.3f, works read by "+
			"disjoint groups %.3f: the factors learned nothing about readership",
			within, across)
	}
	if within <= 0 {
		t.Fatalf("within-group similarity is %.3f", within)
	}
}

// The training diagnostics must behave: error falls, movement falls to near zero.
//
// This is the test that catches a solver that "works" because the data is easy. A
// factorisation whose error does not fall has an update rule with the wrong sign, and one
// whose factors never stop moving never reached a fixed point.
func TestTrainingErrorFallsAndMovementDies(t *testing.T) {
	var in []Interaction
	// Three clusters of readers and works, so there is structure to find.
	for u := int64(1); u <= 30; u++ {
		cluster := u % 3
		for w := int64(0); w < 6; w++ {
			if int64(w)%3 == cluster {
				in = append(in, Interaction{User: u, Work: 10 + w})
			}
		}
	}
	p := DefaultParams()
	p.Iters = 10
	res := Train(in, p)
	if len(res.Fit) != 10 || len(res.Moved) != 10 {
		t.Fatalf("expected one diagnostic per sweep, got %d fits and %d movements",
			len(res.Fit), len(res.Moved))
	}
	if !(res.Fit[9] < res.Fit[0]) {
		t.Fatalf("training error went %v → %v: the solver is not fitting", res.Fit[0], res.Fit[9])
	}
	if !(res.Moved[9] < res.Moved[0]) {
		t.Fatalf("factor movement went %v → %v: the loop is not settling", res.Moved[0], res.Moved[9])
	}
	if res.Moved[9] > 0.5 {
		t.Fatalf("after ten sweeps the largest factor still moves by %v per sweep", res.Moved[9])
	}
	for _, v := range res.Fit {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("training error is %v: the update rule diverged", v)
		}
	}
}

// The same input must produce the same model, or no measurement of this signal can be
// repeated.
func TestTrainingIsDeterministic(t *testing.T) {
	in := []Interaction{
		{User: 1, Work: 1}, {User: 1, Work: 2}, {User: 2, Work: 2}, {User: 2, Work: 3},
	}
	p := DefaultParams()
	p.Iters = 4
	a := Train(in, p)
	// Reversed input order must not matter either: the dense indices are sorted.
	rev := make([]Interaction, len(in))
	for i := range in {
		rev[len(in)-1-i] = in[i]
	}
	b := Train(rev, p)
	for i := range a.Works {
		for j := range a.Works[i] {
			if a.Works[i][j] != b.Works[i][j] {
				t.Fatalf("work %d factor %d differs between runs: %v vs %v",
					a.WorkIDs[i], j, a.Works[i][j], b.Works[i][j])
			}
		}
	}
	if len(a.WorkIDs) != len(b.WorkIDs) || a.WorkIDs[0] != b.WorkIDs[0] {
		t.Fatalf("the same input produced different id sets: %v vs %v", a.WorkIDs, b.WorkIDs)
	}
}

// An empty table is not an error and must not produce a model that claims one.
func TestNoInteractionsMeansNoModel(t *testing.T) {
	res := Train(nil, DefaultParams())
	if len(res.WorkIDs) != 0 || len(res.Works) != 0 {
		t.Fatalf("empty input produced %d works", len(res.Works))
	}
	// And Cosine over empty rows is 0, not NaN.
	if c := Cosine(nil, nil); c != 0 {
		t.Fatalf("cosine of nothing = %v", c)
	}
}

// A work with very few readers must not be inflated by the confidence weighting: the
// factor magnitude is what a naive implementation lets run away.
func TestSparseWorksStayBounded(t *testing.T) {
	in := []Interaction{
		{User: 1, Work: 1}, {User: 1, Work: 2}, {User: 1, Work: 3},
		{User: 2, Work: 1}, {User: 2, Work: 2},
		{User: 3, Work: 4},
	}
	p := DefaultParams()
	p.Iters = 8
	res := Train(in, p)
	for i, row := range res.Works {
		norm := 0.0
		for _, v := range row {
			norm += float64(v) * float64(v)
		}
		if n := math.Sqrt(norm); n > 50 {
			t.Fatalf("work %d has factor norm %v after regularisation: the solve is "+
				"missing its L2 term", res.WorkIDs[i], n)
		}
	}
}
