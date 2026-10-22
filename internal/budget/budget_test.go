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

func TestErrNoProcIsDistinguishable(t *testing.T) {
	// Callers that only want heap numbers ignore the error; a caller that
	// needs the number must be able to tell "no /proc" from "read failed".
	_, err := PeakRSSKiB()
	if err != nil && !errors.Is(err, ErrNoProc) {
		t.Fatalf("unexpected error kind: %v", err)
	}
}
