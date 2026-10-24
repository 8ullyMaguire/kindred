package signal

// latent scores a candidate by its position in the ALS factor space relative to the
// seeds (#89).
//
// This is the first dimension here learned from what readers CHOSE rather than derived
// from what the works say about themselves. collab counts co-bookmark pairs, which is
// also reader evidence but stops at pairs; these factors place every work in a
// 32-dimensional space where similarity survives works that share no reader directly.
//
// WHAT IT SCORES, and what it deliberately does not:
//
//	score = max over seeds of cosine(factor(candidate), factor(seed)), clamped to [0,1]
//
// Max, not mean: a reader seeding two works from different fandoms is asking for
// something like either, and averaging would reward the candidate that sits between them
// — which is usually the work that resembles nothing. The zero-clamp lives in
// als.Cosine, because a negative dot product means "different readers", not "the opposite
// book".
//
// A seed with no factors (a work outside the interaction graph) contributes nothing; a
// candidate with none is recorded as unscored. With no model at all the signal skips for
// every candidate, so "this mirror was never trained" appears in meta.degraded[] rather
// than looking like a dimension that scored zero.

import (
	"strconv"

	"git.polarisocial.xyz/kindred/kindred/internal/als"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// Latent scores by ALS factor similarity. Load returns the trained model keyed by work
// id; nil or empty means the deployment has none.
//
// A LOADER rather than a map, because the model is 112,935 x 32 float32s (14 MB) and the
// default tune gives this signal weight 0. A map field would be filled when the signal
// list is built, i.e. on every request that does not use it; a loader is called from
// Score, which rank only calls for a signal whose weight is non-zero.
type Latent struct {
	Load func() map[int64][]float32
	// MaxSeeds bounds how many seeds are compared: the cost is one cosine per
	// (candidate, seed) pair and a reader can pass a hundred seeds.
	MaxSeeds int
}

func (Latent) Name() string { return "latent" }

// Score returns the best factor cosine to any seed.
func (l Latent) Score(c rank.Candidate, seeds []rank.Candidate, _ rank.Store) (float64, string, error) {
	if l.Load == nil {
		return 0, "", rank.ErrSkip
	}
	factors := l.Load()
	if len(factors) == 0 {
		return 0, "", rank.ErrSkip
	}
	cf, ok := factors[int64(c.ID)]
	if !ok {
		return 0, "", rank.ErrSkip
	}
	limit := l.MaxSeeds
	if limit <= 0 || limit > len(seeds) {
		limit = len(seeds)
	}
	best := 0.0
	bestSeed := int64(0)
	compared := false
	for _, s := range seeds[:limit] {
		sf, ok := factors[int64(s.ID)]
		if !ok {
			continue
		}
		compared = true
		if v := als.Cosine(cf, sf); v > best {
			best, bestSeed = v, s.ID
		}
		if bestSeed == 0 {
			// A cosine of exactly 0 is a real answer ("these have nothing in common"),
			// and the seed still has to be named for the evidence line. The first seed
			// that produced ANY comparable answer is the fallback.
			bestSeed = s.ID
		}
	}
	if !compared {
		// No seed had a factor row: nothing was compared, which is a gap and not a zero.
		return 0, "", rank.ErrSkip
	}
	// The reason names the seed: 0.4 against seed A and 0.4 against seed B are the same
	// number and different explanations, and the evidence line is what a reader sees.
	return best, "the same readers as " + strconv.FormatInt(bestSeed, 10) + ", by learned factors", nil
}
