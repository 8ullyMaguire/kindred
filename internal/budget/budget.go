// Package budget samples this process's own memory. It MEASURES; it does
// not decide pass/fail.
//
// The pass/fail cap (SPEC §6: 60 MiB lite, 220 MiB full) was removed on
// 2026-10-22. /healthz used to turn a peak above that line into a 503, which
// meant a service answering every request correctly reported itself unwell
// because of a number a spec line predicted. A health check that fails for a
// number trains its readers to ignore it, and the deploy gate then cannot
// tell "the product is broken" from "the product is bigger than the note".
//
// The measurement stays because it is how the number was ever known: VmHWM
// cannot miss a spike the way sampling current RSS can, and /healthz still
// reports peak_rss_kib and heap_mib. What changed is that nothing fails on it.
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
// want heap numbers should ignore it; a memory report must not.
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

// Report is one sampled point of a memory reading.
type Report struct {
	Phase        string  `json:"phase"`
	PeakRSSKiB   int     `json:"peak_rss_kib"`
	HeapAllocMiB float64 `json:"heap_alloc_mib"`
	SysMiB       float64 `json:"sys_mib"`
	Goroutines   int     `json:"goroutines"`
}

// Snapshot samples the current process. A VmHWM read failure is folded
// into the report rather than returned, so a non-Linux target still gets
// heap numbers.
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
