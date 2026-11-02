package stats

import "testing"

// A difference present in EVERY reader must produce an interval that excludes zero,
// even on a small sample. Without this the whole comparison is a decimal that looks
// like a verdict.
func TestBootstrapDiffDetectsAConsistentImprovement(t *testing.T) {
	a := []float64{0.10, 0.20, 0.30, 0.40, 0.50, 0.15, 0.25, 0.35}
	b := make([]float64, len(a))
	for i, v := range a {
		b[i] = v + 0.05
	}
	mean, lo, hi, n := BootstrapDiff(b, a, 2000, 7)
	if n != len(a) {
		t.Fatalf("paired n = %d, want %d", n, len(a))
	}
	if lo <= 0 {
		t.Fatalf("CI [%.4f, %.4f] includes 0 for a uniform +0.05 shift", lo, hi)
	}
	if mean < 0.049 || mean > 0.051 {
		t.Fatalf("mean diff %.4f, want 0.05", mean)
	}
}

// Noise must not become a verdict: alternating signs average to nothing and the
// interval must straddle zero.
func TestBootstrapDiffRefusesNoise(t *testing.T) {
	a := []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6}
	b := []float64{0.2, 0.1, 0.4, 0.3, 0.6, 0.5}
	_, lo, hi, _ := BootstrapDiff(b, a, 2000, 11)
	if lo > 0 || hi < 0 {
		t.Fatalf("CI [%.4f, %.4f] excludes 0 on pure noise", lo, hi)
	}
}

// Readers cannot be paired by index across cohorts. Refusing beats silently
// truncating, which is how a comparison becomes fiction.
func TestBootstrapDiffRefusesUnpairedLengths(t *testing.T) {
	if _, _, _, n := BootstrapDiff([]float64{1, 2, 3}, []float64{1, 2}, 100, 1); n != 0 {
		t.Fatalf("paired a 3-sample with a 2-sample (n=%d)", n)
	}
	if _, _, _, n := BootstrapDiff(nil, nil, 100, 1); n != 0 {
		t.Fatalf("paired two empty samples (n=%d)", n)
	}
}

// The same seed must give the same interval, or two runs of the report disagree
// about the same data.
func TestBootstrapDiffIsDeterministic(t *testing.T) {
	a := []float64{0.1, 0.2, 0.3, 0.4}
	b := []float64{0.11, 0.22, 0.29, 0.44}
	_, lo1, hi1, _ := BootstrapDiff(b, a, 500, 42)
	_, lo2, hi2, _ := BootstrapDiff(b, a, 500, 42)
	if lo1 != lo2 || hi1 != hi2 {
		t.Fatalf("not deterministic: [%v,%v] vs [%v,%v]", lo1, hi1, lo2, hi2)
	}
	_, lo3, hi3, _ := BootstrapDiff(b, a, 500, 43)
	if lo3 == lo1 && hi3 == hi1 {
		t.Log("different seeds gave the same interval; fine for this sample, not a failure")
	}
}

// The interval must bracket the point estimate, or the report would print a mean
// that its own CI contradicts.
func TestBootstrapDiffBracketsItsMean(t *testing.T) {
	a := []float64{0.0, 0.25, 0.5, 0.75, 1.0, 0.25, 0.0, 0.5, 0.75, 0.25}
	b := []float64{0.25, 0.25, 0.5, 0.75, 1.0, 0.5, 0.25, 0.5, 0.75, 0.5}
	mean, lo, hi, _ := BootstrapDiff(b, a, 2000, 99)
	if lo > mean || hi < mean {
		t.Fatalf("mean %.4f outside its own CI [%.4f, %.4f]", mean, lo, hi)
	}
}
