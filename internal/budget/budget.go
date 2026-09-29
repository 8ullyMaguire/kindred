// Package budget samples this process's own memory and decides pass/fail
// against a cap. It is a package rather than a shell script because the
// numbers it asserts (VmHWM, Go runtime stats) are only meaningful in the
// process that allocated them.
package budget

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// ErrNoProc is returned on a platform without /proc. Callers that only
// want heap numbers should ignore it; the budget target must not.
var ErrNoProc = errors.New("budget: /proc/self/status unavailable")

// PeakRSSKiB returns VmHWM — the kernel's high-water mark, in KiB.
//
// Sampling current RSS instead would let a spike between samples go
// unnoticed, and a memory regression is exactly a spike. VmHWM cannot
// miss one.
func PeakRSSKiB() (int, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNoProc, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kiB, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, fmt.Errorf("budget: VmHWM unparseable: %q", line)
		}
		return kiB, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%w: VmHWM not present", ErrNoProc)
}

// Report is one sampled point of a budget run.
type Report struct {
	Phase        string  `json:"phase"`
	PeakRSSKiB   int     `json:"peak_rss_kib"`
	HeapAllocMiB float64 `json:"heap_alloc_mib"`
	SysMiB       float64 `json:"sys_mib"`
	Goroutines   int     `json:"goroutines"`
}

// Snapshot samples the current process. A VmHWM read failure is folded
// into the report rather than returned, so a non-Linux target still gets
// heap numbers; Check below is the gate and it does return the error.
func Snapshot(phase string) Report {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	peak, _ := PeakRSSKiB()
	return Report{
		Phase:        phase,
		PeakRSSKiB:   peak,
		HeapAllocMiB: float64(ms.HeapAlloc) / (1 << 20),
		SysMiB:       float64(ms.Sys) / (1 << 20),
		Goroutines:   runtime.NumGoroutine(),
	}
}

// Check compares a snapshot against a cap in KiB.
func Check(r Report, capKiB int) error {
	kiB, err := PeakRSSKiB()
	if err != nil {
		return err
	}
	if kiB > capKiB {
		return fmt.Errorf("over budget: peak RSS %d KiB exceeds cap %d KiB by %d KiB (phase %q)",
			kiB, capKiB, kiB-capKiB, r.Phase)
	}
	return nil
}

// CapKiB is the steady-state cap for a mode, in KiB, from SPEC §6.
// lite targets a 512 MB Pi; full targets a 16 GB host running twenty
// other services resident.
func CapKiB(mode string) int {
	if mode == "lite" {
		return 60 * 1024
	}
	return 220 * 1024
}
