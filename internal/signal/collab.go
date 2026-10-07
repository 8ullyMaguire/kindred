package signal

import (
	"fmt"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/collab"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// Collab is the collaborative-filtering signal: how much this candidate was
// read by the same people who read the seeds.
//
// It is the one dimension here that is evidence of what a reader CHOSE rather
// than a restatement of the work's own metadata. tag_overlap,
// neighbourhood and embedding all derive from the works' tags, so they agree
// with each other about what a fic IS and none of them can notice that every
// person who bookmarked the seed also bookmarked the sequel. PeerRating is the
// other human signal, but it reads the arena, which needs comparisons a reader
// has actually made — this one reads the mirror, which already has 180,677
// bookmark rows from 6,261 real readers.
//
// ## The pool problem this signal had to solve to be worth anything
//
// The candidate pool is built from TAG sharing: a work enters it by sharing a
// tag with a seed. A work that shares no tag with the seed but is
// overwhelmingly co-bookmarked with it would therefore never be ranked, and
// the signal would be inert on exactly the cases that justify it. Engine
// therefore unions the collab neighbours into the pool before scoring (see
// Engine.poolFor), which is the only reason this signal can be anything but
// a restatement of tag_overlap.
//
// ## It is registered unconditionally, like peer_rating
//
// With no index — a lite run, a mirror without user_work_interactions, an
// ingest predating this feature — it returns rank.ErrSkip for every candidate
// and names itself in meta.degraded[]. That is SPEC §7.1, and it is the
// specific failure kindling made: a signal that is inert in production and
// silent about it is indistinguishable from one that does not exist. A signal
// added only when it has data is a signal absent without saying so.
type Collab struct {
	// Votes maps candidate work id to its accumulated co-bookmark score
	// from the seeds. It is precomputed once per request by the caller,
	// because the vote set depends on the SEEDS and recomputing it per
	// candidate is the per-candidate-recomputation cost class this project
	// has measured and removed three times.
	Votes map[int64]float64

	// MinVote is the smallest score a candidate must reach to be scored at
	// all. Zero disables the gate.
	//
	// This is the signal's own evidence gate, distinct from collab's
	// per-pair gate. That one says "do not trust this pair of works"; this
	// one says "do not report a score for a candidate the seeds say nothing
	// about". Without it, every candidate outside the vote set scores an
	// exact zero and the whole ranking carries a dimension that is
	// constant-zero rather than absent — which reads as "we measured it and
	// it is mediocre" rather than "we have no idea".
	MinVote float64

	// Scale normalises the accumulated vote into roughly [0,1]. Zero means
	// the package default. Votes accumulate over up to MaxSeeds
	// neighbours each, so the raw sum is bounded by seeds*limit*maxSim.
	Scale float64
}

func (Collab) Name() string { return "collab" }

// defaultCollabScale bounds the raw vote.
//
// Measured reasoning rather than a guess: NeighboursFor returns at most
// perSeed neighbours per seed, each scoring at most 1.0 (cosine is bounded by
// 1 for binary vectors and confidence only discounts). With the engine's
// default 5 seeds and 40 neighbours per seed, the raw sum is bounded by 200.
// Dividing by that puts a candidate every seed voted for at the top of the
// range and the rest proportionally below, rather than saturating past 1.0
// where every well-supported candidate ties at the ceiling and the signal
// stops discriminating exactly where it should be strongest.
const defaultCollabScale = 200.0

func (s Collab) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	if len(s.Votes) == 0 {
		// The index is absent or empty. Skip, and be named in
		// meta.degraded[] — the observable difference between "no bookmark
		// evidence here" and "the signal does not work".
		return 0, "", rank.ErrSkip
	}
	v, ok := s.Votes[c.ID]
	if !ok {
		// The index has opinions, just not about this work. Like
		// peer_rating's unrated candidate, that is an absence of evidence
		// rather than a missing dimension, so the candidate scores 0 and
		// the signal stays un-degraded. Skipping here would report the
		// whole signal as degraded the first time an un-collaborated work
		// entered the pool, which is noise about a working system.
		return 0, "", nil
	}
	if s.MinVote > 0 && v < s.MinVote {
		return 0, "", rank.ErrSkip
	}

	scale := s.Scale
	if scale <= 0 {
		scale = defaultCollabScale
	}
	score := v / scale
	if score > 1 {
		// Clamped rather than allowed to exceed the range the other signals
		// live in. An unbounded value here would let one heavily-voted work
		// outrank a slightly better work on every other dimension, which is
		// what a weight is supposed to prevent, not cause.
		score = 1
	}
	return score, fmt.Sprintf("co-bookmarked by readers of the seeds (vote %.3f of max %.0f)",
		v, scale), nil
}

// CollabNormaliser converts a raw accumulated vote into [0,1] using the
// ACTUAL bound of this request's vote set rather than a constant.
//
// It exists because the constant bound above is wrong for small requests: a
// single seed with four neighbours would score every candidate under 0.02 and
// the signal would be effectively absent, reported as "measured and
// mediocre". Normalising against the observed maximum makes the top
// candidate 1.0 whatever the seed count, and leaves the ranking shape intact.
//
// It must NOT normalise against the pool maximum when the pool includes
// candidates with no votes: those are 0 and would be included in the
// denominator range for no reason.
type CollabNormaliser struct{}

func (CollabNormaliser) Normalise(votes map[int64]float64) float64 {
	max := 0.0
	for _, v := range votes {
		if v > max {
			max = v
		}
	}
	return max
}

// collabVoteCache memoises the per-request vote set.
//
// The cache key is the seed set plus the limit, because the same seeds at a
// different neighbour limit produce a different vote set. Memoising it is
// worth it because /work/N renders a recommendation block and /recommend
// re-requests the same seeds; the vote set is the expensive part and it does
// not depend on the candidate pool.
var collabVoteCache = newLRU[int64](8)

// CollabVotes returns the vote set for a seed list, memoised.
func CollabVotes(idx *collab.Index, seeds []int64, limit int) map[int64]float64 {
	if idx == nil || len(seeds) == 0 {
		return nil
	}
	key := collabVoteKey(seeds, limit)
	if v, ok := collabVoteCache.get(key); ok {
		return v
	}
	votes := idx.NeighboursFor(seeds, limit, collab.Options{})
	collabVoteCache.put(key, votes)
	return votes
}

// ResetCollabVoteCache clears the memo. Exported for tests: a shared cache
// across tests lets one test's memo answer another's query, which is the same
// reason resetVoteCache exists.
func ResetCollabVoteCache() { collabVoteCache = newLRU[int64](8) }

func collabVoteKey(seeds []int64, limit int) string {
	cp := append([]int64(nil), seeds...)
	sort.Slice(cp, func(a, b int) bool { return cp[a] < cp[b] })
	k := fmt.Sprintf("c%d:%d:", limit, len(cp))
	for _, s := range cp {
		k += fmt.Sprintf("%d,", s)
	}
	return k
}
