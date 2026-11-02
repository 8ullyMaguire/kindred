package graph

import (
	"math"
	"testing"
)

// N=10 works. a on 5, b on 4, together on 3.
//
//	p_ab     = 0.3
//	p_a*p_b  = 0.2
//	ln(1.5)  = 0.4054651
//	-ln(0.3) = 1.2039728
//	0.4054651 / 1.2039728 = 0.336825
func TestNPMIHandComputed(t *testing.T) {
	got := NPMI(5, 4, 3, 10)
	if math.Abs(got-0.336825) > 1e-4 {
		t.Fatalf("NPMI(5,4,3,10) = %v, want 0.336825", got)
	}
	if back := NPMI(4, 5, 3, 10); math.Abs(back-got) > 1e-12 {
		t.Fatalf("NPMI is not symmetric: %v vs %v", got, back)
	}
}

// A tag with itself always co-occurs, so its nPMI is 1 at any frequency: p_ab is
// df/N and the two marginals multiply to (df/N)^2, so the ratio is N/df and the
// normaliser is exactly -ln(df/N). This is the property that lets nPMI carry the
// direct-match term without a special case.
func TestNPMIOfATagWithItselfIsOne(t *testing.T) {
	for _, df := range []float64{1, 5, 100, 50000} {
		if got := NPMI(df, df, df, 112935); math.Abs(got-1) > 1e-9 {
			t.Fatalf("NPMI(%v,%v,%v) = %v, want 1", df, df, df, got)
		}
	}
}

// Exactly independent: co/N == (freqA/N)*(freqB/N), so the log is 0.
func TestNPMIOfIndependentTagsIsZero(t *testing.T) {
	if got := NPMI(5, 2, 1, 10); math.Abs(got) > 1e-9 {
		t.Fatalf("NPMI(5,2,1,10) = %v, want 0 (exactly independent)", got)
	}
}

func TestNPMIIsBoundedToMinusOneToOne(t *testing.T) {
	cases := [][4]float64{
		{1, 1, 1, 112935},
		{2, 2, 1, 112935},
		{50000, 50000, 50000, 112935},
		{112935, 112935, 112935, 112935},
		{10, 90000, 1, 112935},
	}
	for _, c := range cases {
		got := NPMI(c[0], c[1], c[2], c[3])
		if math.IsNaN(got) {
			t.Fatalf("NPMI%v = NaN", c)
		}
		if got < -1-1e-9 || got > 1+1e-9 {
			t.Fatalf("NPMI%v = %v, outside [-1,1]", c, got)
		}
	}
}

func TestNPMIGuardsDegenerateInputs(t *testing.T) {
	for _, c := range [][4]float64{
		{0, 5, 3, 10},
		{5, 0, 3, 10},
		{5, 5, 0, 10},
		{5, 5, 3, 0},
	} {
		if got := NPMI(c[0], c[1], c[2], c[3]); got != 0 {
			t.Fatalf("NPMI%v = %v, want 0", c, got)
		}
	}
}

// TestNPMICollapsesTheRarePairMagnitudeGapThatPMIDoesNot is the test that justifies
// nPMI over PMI, and it asserts the MAGNITUDE gap rather than the ordering, because
// the ordering is not the defect.
//
// Two pairs on a 112,935-work corpus:
//
//	chance : both tags on 2 works, co-occurring once (a coin flip on almost no data)
//	real   : both tags on 800 works, co-occurring 500 times
//
// Raw PMI prefers the coincidence by more than 2x. Neighbourhood SUMS votes, so one
// sloppy pair outvotes a real association. nPMI compresses the two into the same
// statement ("strong, but not certain"): the gap falls to about 6%.
//
// nPMI does not cure the rare-pair bias -- 0.88 is still high -- which is why the
// co-occurrence floor (signal.TagNPMI.MinCount) remains necessary and is documented
// as the thing doing that work, not the normalisation.
func TestNPMICollapsesTheRarePairMagnitudeGapThatPMIDoesNot(t *testing.T) {
	const n = 112935.0
	pmiChance := PMI(2, 2, 1, n)
	pmiReal := PMI(800, 800, 500, n)
	npmiChance := NPMI(2, 2, 1, n)
	npmiReal := NPMI(800, 800, 500, n)

	if pmiChance <= 0 || pmiReal <= 0 {
		t.Fatalf("premise: PMI must be positive for both pairs, got %v and %v", pmiChance, pmiReal)
	}
	if pmiChance/pmiReal < 2 {
		t.Fatalf("premise: PMI ratio %.3f, expected >2 — the fixture no longer shows the defect",
			pmiChance/pmiReal)
	}
	if npmiChance/npmiReal > 1.1 {
		t.Fatalf("nPMI(2,2,1)=%.4f vs nPMI(800,800,500)=%.4f: ratio %.3f, want <=1.1",
			npmiChance, npmiReal, npmiChance/npmiReal)
	}
	t.Logf("PMI chance/real = %.2f, nPMI chance/real = %.3f",
		pmiChance/pmiReal, npmiChance/npmiReal)
}
