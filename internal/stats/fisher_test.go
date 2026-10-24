package stats

// The p-value against known tables, and the three refusals.
//
// The reference values are R's `fisher.test` at its default (two-sided), because a
// hand-written exact test is a place where being off by a factor is invisible and
// consequential: this number decides whether a page names a winning algorithm.

import (
	"math"
	"strings"
	"testing"
)

func TestFisherMatchesKnownTables(t *testing.T) {
	cases := []struct {
		name       string
		a, b, c, d int
		want       float64
		tol        float64
	}{
		// fisher.test(matrix(c(3,1,1,3),2)) -> 0.4857
		{"tea tasting 3/4 vs 1/4", 3, 1, 1, 3, 0.4857, 0.001},
		// fisher.test(matrix(c(10,0,0,10),2)) -> 1.08e-05
		{"perfect split, ten each", 10, 0, 0, 10, 1.0825e-05, 1e-7},
		// fisher.test(matrix(c(1,9,9,1),2)) -> 0.0011
		{"1/10 against 9/10", 1, 9, 9, 1, 0.0011, 0.0002},
		// An exact tie is no evidence at all.
		{"identical columns", 5, 5, 5, 5, 1.0, 1e-9},
	}
	for _, c := range cases {
		got := FisherExact(c.a, c.b, c.c, c.d)
		if math.Abs(got-c.want) > c.tol {
			t.Fatalf("%s: p = %.6f, want %.6f", c.name, got, c.want)
		}
	}
}

// A table with no variation has no evidence of a difference, and p must say that
// rather than 0 -- which would be the strongest possible claim about nothing.
func TestADegenerateTableReturnsOneNotZero(t *testing.T) {
	for _, tc := range [][4]int{
		{0, 0, 0, 0},
		{5, 0, 0, 0},
		{0, 5, 0, 5}, // one row empty
		{5, 5, 0, 0}, // one column empty
	} {
		if p := FisherExact(tc[0], tc[1], tc[2], tc[3]); p != 1 {
			t.Fatalf("table %v gave p = %v, want 1", tc, p)
		}
	}
}

// The floor comes before the test. Two slots an arm is a coin landing, whichever
// way it landed, and the verdict must name the slot counts rather than the p.
func TestTooFewSlotsIsRefusedBeforeAnyPValue(t *testing.T) {
	v := Compare("a", 2, 2, "b", 0, 2)
	if v.Leading != "" {
		t.Fatalf("declared %q the winner on 2 slots each", v.Leading)
	}
	if !v.Underpowered {
		t.Fatal("did not report the sample as the reason")
	}
	if !strings.Contains(v.Reason, "50") || !strings.Contains(v.Reason, "slot") {
		t.Fatalf("reason does not say what is missing: %q", v.Reason)
	}
}

// With enough slots, a real difference is called -- and a small one is not.
func TestALargeDifferenceOverEnoughSlotsIsCalled(t *testing.T) {
	v := Compare("a", 60, 400, "b", 20, 400)
	if v.Leading != "a" {
		t.Fatalf("leading = %q, want a (p = %.5f)", v.Leading, v.P)
	}
	if v.P > Alpha {
		t.Fatalf("p = %.5f but a lead was declared", v.P)
	}
}

func TestASmallDifferenceOverEnoughSlotsIsNot(t *testing.T) {
	// 210/400 vs 200/400: a 2.5% relative difference on 800 slots is noise.
	v := Compare("a", 210, 400, "b", 200, 400)
	if v.Leading != "" {
		t.Fatalf("declared %q the winner on a difference of 10 slots in 800", v.Leading)
	}
	if v.P > Alpha {
		// The reason should quote the p.
		if !strings.Contains(v.Reason, "p") {
			t.Fatalf("reason does not quote the p-value: %q", v.Reason)
		}
	}
}

// The direction of the lead must follow the RATES, not the raw counts: an arm with
// more slots can have more reads and still be behind.
func TestTheLeadFollowsTheRateNotTheCount(t *testing.T) {
	// A: 60 of 400 (15%). B: 40 of 150 (26.7%).
	v := Compare("a", 60, 400, "b", 40, 150)
	if v.Leading != "b" {
		t.Fatalf("leading = %q, want b: 40/150 beats 60/400 on rate", v.Leading)
	}
}

// An exact tie reports no lead even at large n, and its reason says so rather than
// quoting a p-value of 1 as if it were a finding.
func TestAnExactTieIsNotALead(t *testing.T) {
	v := Compare("a", 100, 400, "b", 100, 400)
	if v.Leading != "" {
		t.Fatalf("declared %q on an exact tie", v.Leading)
	}
	if !strings.Contains(v.Reason, "no evidence") {
		t.Fatalf("reason = %q", v.Reason)
	}
}
