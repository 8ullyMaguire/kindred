package signal

import (
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// LengthFit scores how close a work's length is to the lengths of the seeds.
//
// # Why this is a signal and not a filter
//
// A reader who spends an evening on 3,000-word fics and one who wants a 300,000
// word epic are not the same reader, and the tag vocabulary barely separates them:
// both may carry the same fandom, pairing and freeform tags. Length is one of the
// few preferences that is (a) recorded for every work, (b) stable per reader and
// (c) invisible to every other signal here.
//
// The existing `min_words`/`max_words` filters let a reader declare a range, but a
// filter answers a different question: it deletes everything outside the range and
// leaves the inside unordered, so a 1,200-word drabble ranks beside a 90,000-word
// epic when both are inside a 1,000-100,000 window. This scores the distance, so a
// reader's list can prefer the shape of what they asked from.
//
// # The shape of the score
//
// Distance is measured in LOG space, because length is multiplicative: one step
// from 5k to 10k is the same experience as 50k to 100k, and a linear difference
// would make every long work look equally far from every short one.
//
//	value = min(words, seedMedian) / max(words, seedMedian)
//
// which is exp(-|ln ratio|) and has a plainer statement: the score is the ratio of
// the smaller length to the larger. 1.00 at the seeds' own length, 0.50 at twice or
// half, 0.25 at four times, 0.06 at sixteen times. The falloff is a chosen shape,
// not a measured one, and it is stated here rather than hidden: a work twice the
// length is still worth showing, a work sixteen times longer is usually not what
// the reader meant.
//
// # What it refuses to guess
//
// A work with no word count (0 or absent in Stats) scores nothing and says so for
// that candidate. A REQUEST whose seeds carry no word count at all SKIPS, naming
// itself in meta.degraded[] — an unmeasured length preference is not "every length
// is equally good", and a signal that reported 0.0 for both would make a corrupt
// corpus look like a considered opinion.
//
// It also skips the whole request when every seed has the same length and every
// candidate does too — but that is not detectable per candidate, so the per-seed
// median is the only guard here, and a corpus with one length is a corpus where
// this signal is constant and harmless.
type LengthFit struct {
	// MaxSeeds bounds how many seeds are consulted. The median of a few hundred
	// bookmarks is not a sharper number than the median of twenty: it is the same
	// reader's taste with more of their outliers included.
	MaxSeeds int
}

// Name is the signal's name and the key a tune's weight vector uses.
func (LengthFit) Name() string { return "length_fit" }

// Score returns the log-distance fit to the seeds' median length.
func (l LengthFit) Score(c rank.Candidate, seeds []rank.Candidate, _ rank.Store) (float64, string, error) {
	// Each seed contributes one length. A seed without one is not a zero-length
	// work, it is silence, and including it would drag the median toward short.
	lengths := make([]float64, 0, len(seeds))
	limit := l.MaxSeeds
	if limit <= 0 || limit > len(seeds) {
		limit = len(seeds)
	}
	for _, s := range seeds[:limit] {
		if w := s.Stats["word_count"]; w > 0 {
			lengths = append(lengths, w)
		}
	}
	if len(lengths) == 0 {
		// Every seed is silent about length. That is the request-level skip: the
		// reader's length preference is unmeasured, not neutral.
		return 0, "", rank.ErrSkip
	}
	median := medianOf(lengths)

	w := c.Stats["word_count"]
	if w <= 0 {
		// Silence about THIS work. Zero would rank an unknown-length work as
		// maximally distant, which is a claim; skipping contributes no term.
		return 0, "", rank.ErrSkip
	}
	// The ratio of the smaller to the larger. Equivalent to exp(-|ln ratio|),
	// written as a ratio so the number on the page is the number a reader can
	// check: two lengths, divided.
	if w < median {
		return w / median, "", nil
	}
	return median / w, "", nil
}

// medianOf returns the middle value, averaging the two middles for an even count.
// It sorts a copy: the caller's slice order is the reader's bookmark order and is
// not this function's to destroy.
func medianOf(xs []float64) float64 {
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}
