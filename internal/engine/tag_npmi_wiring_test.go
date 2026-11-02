package engine

import (
	"context"
	"testing"
)

// TestTagNPMIHasATuneKey asserts the signal can be WEIGHTED at all.
//
// `kindred tune --set` validates every key against DefaultTune().Weights
// (cmd/kindred/tune.go), so a registered signal that is absent from the default
// vector cannot be given a weight by any eval arm: the arm fails with "unknown
// signal" before it measures anything. The weight itself starts at 0, because a
// candidate dimension must not change the shipped ranking until a run earns it.
func TestTagNPMIHasATuneKey(t *testing.T) {
	w, ok := DefaultTune().Weights["tag_npmi"]
	if !ok {
		t.Fatal("DefaultTune has no tag_npmi key; `kindred tune --set tag_npmi=0.12` will be rejected")
	}
	if w != 0 {
		t.Fatalf("tag_npmi starts at weight %v, want 0 until an eval earns it", w)
	}
}

// TestTagNPMISignalIsRegistered asserts the signal reaches the ranker at all.
//
// A signal constructed but never appended to sigs passes every test of its own
// arithmetic and changes nothing in the product — this repo has shipped that shape
// five times, so the wiring gets its own test.
func TestTagNPMISignalIsRegistered(t *testing.T) {
	e := newFixtureEngine(t)
	sigs, _ := e.signals(context.Background(), nil, nil, 0, nil, "default", true, signalOptions{})
	found := false
	for _, s := range sigs {
		if s.Name() == "tag_npmi" {
			found = true
		}
	}
	if !found {
		t.Fatal("the tag_npmi signal is not registered in signals()")
	}
}
