package engine

import (
	"errors"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/signal"
)

// owner_taste is the deployment's global prior: the owner's taste applied to
// every reader. These tests pin the three properties that make it safe to
// register unconditionally, which is the house convention for signals and the
// one this project has broken five times.

// ownerTasteFrom finds the owner_taste signal in a signal set.
func ownerTasteFrom(t *testing.T, sigs []rank.Signal) signal.OwnerTaste {
	t.Helper()
	for _, s := range sigs {
		if s.Name() == "owner_taste" {
			ot, ok := s.(signal.OwnerTaste)
			if !ok {
				t.Fatalf("the signal named owner_taste is a %T", s)
			}
			return ot
		}
	}
	t.Fatal("owner_taste is absent from the signal set; a deployment without an owner must still register it")
	return signal.OwnerTaste{}
}

func TestOwnerTasteIsRegisteredAndWeightedWithNoOwner(t *testing.T) {
	e := &Engine{}
	sigs, tune := e.signals(nil, nil, 0, nil, "default", true)

	ot := ownerTasteFrom(t, sigs)
	// No owner vector: the signal must SKIP for every candidate rather than
	// score a zero, so the response names it in meta.degraded[]. A constant
	// zero would read as "we measured the owner's taste and it is mediocre".
	if _, _, err := ot.Score(rank.Candidate{ID: 1, TagIDs: []int32{10}}, nil, nil); !errors.Is(err, rank.ErrSkip) {
		t.Fatalf("owner_taste with no owner scored instead of skipping: %v", err)
	}

	// And it has a weight. A registered signal with no weight in the tune is
	// a signal that has been switched off silently, which is the same defect
	// wearing different clothes.
	if w := tune.Weights["owner_taste"]; w <= 0 {
		t.Fatalf("DefaultTune weight for owner_taste = %v; a non-positive weight switches the signal off", w)
	}

	// The graph is nil here, and the signal must hold a nil interface rather
	// than a typed nil. Assigning a nil *graph.CSR into the Graph interface
	// field would make the field non-nil and the first Name(id) call would
	// dereference it, so the guard is asserted rather than assumed.
	if ot.Graph != nil {
		t.Fatal("owner_taste holds a non-nil Graph built from a nil engine graph; a typed nil would panic on Name()")
	}
}

func TestNoOwnerTasteWithholdsTheVectorWithoutRemovingTheSignal(t *testing.T) {
	e := &Engine{}
	e.SetOwnerTaste(map[int32]float64{10: 1})

	// personal=true: the prior is applied.
	sigs, _ := e.signals(nil, nil, 0, nil, "default", true)
	ot := ownerTasteFrom(t, sigs)
	got, _, err := ot.Score(rank.Candidate{ID: 1, TagIDs: []int32{10}}, nil, nil)
	if err != nil {
		t.Fatalf("owner_taste skipped with an owner configured: %v", err)
	}
	if got != 1.0 {
		t.Fatalf("score for a fully owner-liked candidate = %v; want 1.0", got)
	}

	// personal=false (?personal=0): the vector is withheld, but the signal is
	// still in the set and still says why it skipped. Dropping the signal
	// instead would make a deliberate request indistinguishable from a broken
	// installation in the response.
	sigs, _ = e.signals(nil, nil, 0, nil, "default", false)
	ot = ownerTasteFrom(t, sigs)
	if _, _, err := ot.Score(rank.Candidate{ID: 1, TagIDs: []int32{10}}, nil, nil); !errors.Is(err, rank.ErrSkip) {
		t.Fatalf("?personal=0 returned %v; want rank.ErrSkip so meta.degraded names it", err)
	}
}

// The vector is swapped by the refresh loop while requests read it, so the
// accessor must be the only way in. This is a smoke test of that path rather
// than a race test: `go test -race` is what covers the interleaving, and the
// lock exists because an unsynchronised map swap is a fatal runtime error
// rather than a detector warning.
func TestSetOwnerTasteIsReadBack(t *testing.T) {
	e := &Engine{}
	if e.OwnerTasteSignal() != nil {
		t.Fatal("a fresh engine reports an owner vector")
	}
	e.SetOwnerTaste(map[int32]float64{1: 0.5})
	if got := e.OwnerTasteSignal(); len(got) != 1 || got[1] != 0.5 {
		t.Fatalf("OwnerTasteSignal = %v; want map[1:0.5]", got)
	}
	e.SetOwnerTaste(nil)
	if e.OwnerTasteSignal() != nil {
		t.Fatal("the vector was not cleared")
	}
}
