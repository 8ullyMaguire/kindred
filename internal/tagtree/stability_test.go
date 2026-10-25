package tagtree

// Stability is the one measurement available WITHOUT ground truth, so it gets its own tests:
// one where two partitions are identical, one where they are unrelated, and one where they are
// close but not equal. An AdjustedRand that returns 1 for everything passes the first.

import "testing"

func TestAdjustedRandOnKnownPartitions(t *testing.T) {
	base := map[int64]int{1: 0, 2: 0, 3: 0, 4: 1, 5: 1, 6: 1}
	if got := AdjustedRand(base, base); got != 1 {
		t.Fatalf("identical partitions scored %v, want 1", got)
	}

	// The same two clusters with the labels swapped: a rename, not a change.
	swapped := map[int64]int{1: 1, 2: 1, 3: 1, 4: 0, 5: 0, 6: 0}
	if got := AdjustedRand(base, swapped); got != 1 {
		t.Fatalf("relabelled partitions scored %v, want 1", got)
	}

	// A partition that splits every point: it agrees with nothing beyond chance.
	split := map[int64]int{1: 0, 2: 1, 3: 2, 4: 3, 5: 4, 6: 5}
	if got := AdjustedRand(base, split); got > 0.2 {
		t.Fatalf("a one-point-per-cluster partition scored %v, want near 0", got)
	}

	// One point moves across the boundary: high but not 1.
	moved := map[int64]int{1: 0, 2: 0, 3: 1, 4: 1, 5: 1, 6: 1}
	got := AdjustedRand(base, moved)
	if got >= 1 || got < 0.3 {
		t.Fatalf("one point moved scored %v, want between 0.3 and 1", got)
	}
}

// The stability number has to be able to come out LOW on data with no structure: vectors drawn
// from one isotropic cloud have no families, and two seeds will divide them differently.
func TestStabilityIsLowOnNoise(t *testing.T) {
	vecs := planted(1, 60, 8, 1.0, 42)
	tree, err := BuildAndMeasure(vecs, Options{K: 4, SubK: 0, Seed: 3, Iters: 30})
	if err != nil {
		t.Fatal(err)
	}
	if tree.Stability > 0.75 {
		t.Fatalf("stability %.2f on an isotropic cloud: two seeds agreeing this well means the "+
			"measure is not measuring", tree.Stability)
	}
	// ... and HIGH on data with four clear groups, which is the half that proves it is not a
	// constant near zero.
	sep := planted(4, 15, 8, 0.1, 43)
	tree2, err := BuildAndMeasure(sep, Options{K: 4, SubK: 0, Seed: 3, Iters: 40})
	if err != nil {
		t.Fatal(err)
	}
	if tree2.Stability < 0.9 {
		t.Fatalf("stability %.2f on four well-separated groups, want >= 0.9", tree2.Stability)
	}
}
