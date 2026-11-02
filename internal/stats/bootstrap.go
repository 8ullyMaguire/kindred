package stats

import (
	"math"
	"math/rand"
	"sort"
)

// BootstrapDiff is the paired percentile bootstrap of mean(b) - mean(a) over readers.
//
// Paired, because the same readers are scored by both arms: reader difficulty cancels
// instead of being counted twice. Percentile, because per-reader recall is quantised
// (four held-out works means the values are 0, .25, .5, .75, 1) and a normal
// approximation on that is a lie.
//
// It returns n = 0 for unpaired inputs rather than pairing by index: two arms
// evaluated on different reader sets are not comparable, and a silently truncated
// pairing is how a comparison becomes fiction.
//
// The seed is explicit so a report is reproducible: the same seed and the same inputs
// produce the same interval, and two runs cannot disagree about the same data.
func BootstrapDiff(b, a []float64, iters int, seed uint64) (mean, lo, hi float64, n int) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, 0, 0, 0
	}
	n = len(a)
	d := make([]float64, n)
	sum := 0.0
	for i := range a {
		d[i] = b[i] - a[i]
		sum += d[i]
	}
	mean = sum / float64(n)
	if iters <= 0 {
		iters = 2000
	}
	rng := rand.New(rand.NewSource(int64(seed)))
	buf := make([]float64, iters)
	for it := 0; it < iters; it++ {
		s := 0.0
		for j := 0; j < n; j++ {
			s += d[rng.Intn(n)]
		}
		buf[it] = s / float64(n)
	}
	sort.Float64s(buf)
	lo = buf[int(math.Floor(0.025*float64(iters)))]
	hi = buf[int(math.Ceil(0.975*float64(iters)))-1]
	return mean, lo, hi, n
}
