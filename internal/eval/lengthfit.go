package eval

// Length fit: did the list come back at the length the reader actually reads?
//
// The length_fit SIGNAL prefers works near the seeds' median length, and recall
// cannot say whether it worked: recall asks "did the list contain a work the reader
// bookmarked", which is silent about the SHAPE of the list. A signal that does
// nothing and a signal that works perfectly both leave recall where it was, so
// without this number the ablation in the handoff would be measuring nothing.
//
// The measurement is deliberately not the signal's own input. The signal sees the
// TRAIN bookmarks' median (that is all a live request has); this compares the
// returned list against the held-out works' median, which the recommender never
// saw. Otherwise the test would be "the list matches the thing that shaped it",
// which is true by construction and worth nothing.
//
//	fit = min(listMedian, heldOutMedian) / max(listMedian, heldOutMedian)
//
// — the same shape the signal uses, so 1.0 means "the same length as the works this
// reader went on to bookmark" and 0.5 means "half or double". Written as a ratio
// rather than as exp(-|ln|) because the number a report prints should be checkable
// by hand from the two medians printed beside it.
//
// A reader enters the average when both sides have a measurable length: at least
// one returned work with a word count, and at least one held-out work with one.
// Readers who cannot be measured are counted in LengthSkipped, never averaged in as
// a zero — "no length was measured" and "the lengths were sixteen times apart" must
// not produce the same number.

import (
	"context"
	"sort"
)

// WordCountSource is the optional interface a recommender implements to let the
// evaluation measure length. Like TagSource, it exists so this package does not
// import the corpus, and so the measurement is possible only when the recommender
// can actually answer.
type WordCountSource interface {
	WordCounts(ctx context.Context, ids []int64) (map[int64]int, error)
}

// LengthFit is one reader's list against what they went on to read.
//
// Returned and HeldOut are the medians, in words, so a report can print both
// rather than only the ratio: "900 vs 40,000" is actionable and "0.06" is not.
type LengthFit struct {
	Returned float64 `json:"returned"`
	HeldOut  float64 `json:"held_out"`
	Items    int     `json:"items"`
	Value    float64 `json:"value"`
}

// MeasureLengthFit compares one returned list with one reader's held-out works.
//
// items counts the RETURNED works that carried a length: that is what the ratio's
// left side was computed from, and a list of ten where two had lengths is a weaker
// claim than one where ten did.
func MeasureLengthFit(returned, heldOut []int64, words map[int64]int) (LengthFit, bool) {
	rl := lengthsOf(returned, words)
	hl := lengthsOf(heldOut, words)
	if len(rl) == 0 || len(hl) == 0 {
		return LengthFit{}, false
	}
	rMed := median(rl)
	hMed := median(hl)
	if rMed <= 0 || hMed <= 0 {
		return LengthFit{}, false
	}
	small, large := rMed, hMed
	if small > large {
		small, large = large, small
	}
	return LengthFit{Returned: rMed, HeldOut: hMed, Items: len(rl), Value: small / large}, true
}

func lengthsOf(ids []int64, words map[int64]int) []float64 {
	out := make([]float64, 0, len(ids))
	for _, id := range ids {
		if w := words[id]; w > 0 {
			out = append(out, float64(w))
		}
	}
	return out
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}
