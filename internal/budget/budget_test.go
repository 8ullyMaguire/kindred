package budget

import (
	"errors"
	"testing"
)

func TestPeakRSSKiBReadsARealNumber(t *testing.T) {
	kiB, err := PeakRSSKiB()
	if err != nil {
		t.Fatalf("PeakRSSKiB: %v", err)
	}
	// A live Go test binary is comfortably over 1 MiB. Asserting a real
	// lower bound means this fails if the parse silently returns zero,
	// which is the failure mode it exists to catch.
	if kiB <= 1024 {
		t.Fatalf("VmHWM = %d KiB, want > 1024 for a live process", kiB)
	}
}

func TestSnapshotReportsHeapAndGoroutines(t *testing.T) {
	r := Snapshot("test")
	if r.HeapAllocMiB <= 0 {
		t.Fatalf("heap alloc = %f MiB, want > 0", r.HeapAllocMiB)
	}
	if r.SysMiB < r.HeapAllocMiB {
		t.Fatalf("Sys %f < HeapAlloc %f; runtime.MemStats is misread", r.SysMiB, r.HeapAllocMiB)
	}
	if r.Goroutines <= 0 {
		t.Fatalf("goroutines = %d, want > 0", r.Goroutines)
	}
	if r.Phase != "test" {
		t.Fatalf("Phase = %q", r.Phase)
	}
}

func TestCheckFailsOverCapAndPassesUnder(t *testing.T) {
	r := Snapshot("gate")
	if err := Check(r, CapKiB("lite")); err != nil {
		t.Fatalf("a bare test binary should fit the lite cap: %v", err)
	}
	// 1 KiB is below anything a live process reports, so this must fail.
	// A gate never seen failing is not a gate.
	err := Check(r, 1)
	if err == nil {
		t.Fatal("Check passed against a 1 KiB cap; the gate does not gate")
	}
	if !contains(err.Error(), "over budget") {
		t.Fatalf("error %q does not say over budget", err)
	}
}

func TestCapKiBMatchesSpec(t *testing.T) {
	// SPEC §6: lite 60 MB, full 220 MB steady.
	if got := CapKiB("lite"); got != 61440 {
		t.Fatalf("CapKiB(lite) = %d, want 61440", got)
	}
	if got := CapKiB("full"); got != 225280 {
		t.Fatalf("CapKiB(full) = %d, want 225280", got)
	}
}

func TestErrNoProcIsDistinguishable(t *testing.T) {
	// Callers that only want heap numbers ignore the error; the budget
	// target does not. They must be able to tell those cases apart.
	_, err := PeakRSSKiB()
	if err != nil && !errors.Is(err, ErrNoProc) {
		t.Fatalf("unexpected error kind: %v", err)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (hay == needle || indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
