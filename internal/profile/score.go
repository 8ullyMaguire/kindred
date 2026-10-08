package profile

import "math"

// Scoring and correlation for the rated profile.
//
// Kept apart from rated.go because the scorer is the thing the ranker calls on
// every candidate, and it must stay cheap enough to call once per work per
// request. The correlation helpers are test-only weight and would otherwise
// sit in the file every request path imports.

// Score estimates a work's enjoyment from a profile's weights.
//
// It returns the signed sum of the profile's weights for the tags it holds,
// divided by sqrt(tag count) so a fic carrying 34 tags cannot out-score one
// carrying 4 purely by having more chances to hit a positive weight.
//
// That normalisation is not neutral. It is a length prior borrowed from TF-IDF
// and it encodes an assumption -- that a fic's extra tags carry correspondingly
// less information per tag -- which is true for common tags and false for the
// ones that matter. "Alpha/Beta/Omega" on a 34-tag fic gets 1/5.8 of the pull
// it gets on a 4-tag fic. The alternative, no normalisation, lets tag-stuffed
// works dominate every list, which is worse and less legible; the sqrt divisor
// is the least-wrong of the cheap options and the divisor is exposed as a
// constant rather than inlined so the assumption is one edit away.
func Score(weights map[int32]float64, tags []int32) (float64, int) {
	if len(weights) == 0 || len(tags) == 0 {
		return 0, 0
	}
	var sum float64
	hits := 0
	seen := make(map[int32]bool, len(tags))
	for _, t := range tags {
		if seen[t] {
			continue
		}
		seen[t] = true
		if w, ok := weights[t]; ok {
			sum += w
			hits++
		}
	}
	if hits == 0 {
		return 0, 0
	}
	return sum / sqrt(len(tags)), hits
}

// sqrt is math.Sqrt, wrapped so the scorer does not import math for one call
// and so the length divisor can be swapped in one place if the assumption
// turns out wrong.
func sqrt(n int) float64 {
	if n <= 0 {
		return 1
	}
	return math.Sqrt(float64(n))
}

// Spearman is the Pearson correlation of two series' RANKS.
//
// Ties get average ranks, which matters here: ratings are integers, so a model
// that predicts the same score for four works produces four ties and the naive
// ordinal rank would silently order them 1,2,3,4 and invent a correlation that
// is not in the data. Averaging is what makes a flat predictor score 0 rather
// than a spurious positive.
//
// NaN is returned rather than 0 for a constant input, because "no information"
// and "perfectly reliable inverse" are different answers and the caller prints
// this number. A NaN in the verdict line is a prompt to look, not a number to
// average in.
func Spearman(a, b []float64) float64 {
	if len(a) != len(b) || len(a) < 2 {
		return math.NaN()
	}
	ra := rankWithTies(a)
	rb := rankWithTies(b)
	return pearson(ra, rb)
}

// KendallTau is the concordance correlation: the fraction of ordered pairs
// where a ranks the same way as b.
//
// Printed next to Spearman because the two disagree informatively. Spearman is
// sensitive to how far apart the extremes are -- one badly mis-ranked 10-rated
// fic drags it -- while Tau only cares about order. A model with rho 0.45 and
// tau 0.31 is getting most pairs right and occasionally getting a big one
// badly wrong, which is a different profile from rho 0.45 and tau 0.44 and
// points at a different fix.
func KendallTau(a, b []float64) float64 {
	if len(a) != len(b) || len(a) < 2 {
		return math.NaN()
	}
	ra := rankWithTies(a)
	rb := rankWithTies(b)
	var con, dis int
	for i := 0; i < len(ra); i++ {
		for j := i + 1; j < len(ra); j++ {
			switch {
			case ra[i] < ra[j] && rb[i] < rb[j],
				ra[i] > ra[j] && rb[i] > rb[j]:
				con++
			case ra[i] < ra[j] && rb[i] > rb[j],
				ra[i] > ra[j] && rb[i] < rb[j]:
				dis++
			}
		}
	}
	total := con + dis
	if total == 0 {
		return math.NaN()
	}
	return float64(con-dis) / float64(total)
}

// rankWithTies returns 1-based ranks, averaging ties.
func rankWithTies(v []float64) []float64 {
	n := len(v)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	// Insertion sort by value: n is a held-out library, not a corpus, and
	// sort.Slice's reflection cost dominates at this size.
	for i := 1; i < n; i++ {
		v2 := v[idx[i]]
		j := i - 1
		for j >= 0 && v[idx[j]] > v2 {
			idx[j+1] = idx[j]
			j--
		}
		idx[j+1] = i
	}
	out := make([]float64, n)
	i := 0
	for i < n {
		j := i
		for j+1 < n && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		avg := float64(i+j+2) / 2 // ranks i+1 .. j+1, averaged
		for k := i; k <= j; k++ {
			out[idx[k]] = avg
		}
		i = j + 1
	}
	return out
}

// pearson is the standard correlation of two equal-length series.
func pearson(a, b []float64) float64 {
	n := len(a)
	var sa, sb float64
	for i := 0; i < n; i++ {
		sa += a[i]
		sb += b[i]
	}
	ma, mb := sa/float64(n), sb/float64(n)
	var num, da, db float64
	for i := 0; i < n; i++ {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return math.NaN()
	}
	return num / math.Sqrt(da*db)
}
