// Package signal implements the scoring dimensions.
//
// Each signal is a pure function over a candidate, the seeds, and a read
// -only store handle. A signal that cannot be computed returns
// rank.ErrSkip rather than a zero, because a zero is a claim and a skip
// is a gap — and the previous deployment shipped a bug where two signals
// were inert in production and silent about it, so the scores looked
// plausible while two dimensions contributed nothing.
package signal

import (
	"context"
	"fmt"
	"math"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// Graph is the co-occurrence index a neighbourhood signal reads.
type Graph interface {
	Neighbours(id int32) (ids []int32, weights []float32)
	Frequency(id int32) float64
	NeighbourScore(query int32, limit int, totalWorks float64) []graph.ScoredNeighbour
	Name(id int32) string
}

// TagOverlap scores direct tag agreement between a candidate and the
// seeds, as a Jaccard-style ratio.
type TagOverlap struct {
	// MaxTags bounds the work per candidate. A work with thousands of
	// freeform tags is a bot or a fandom dump, and intersecting it against
	// every seed is quadratic for no signal.
	MaxTags int
}

func (TagOverlap) Name() string { return "tag_overlap" }

func (s TagOverlap) Score(c rank.Candidate, seeds []rank.Candidate, _ rank.Store) (float64, string, error) {
	if len(c.TagIDs) == 0 || len(seeds) == 0 {
		return 0, "", rank.ErrSkip
	}
	own := make(corpus.TagSet, len(c.TagIDs))
	ids := c.TagIDs
	if s.MaxTags > 0 && len(ids) > s.MaxTags {
		ids = ids[:s.MaxTags]
	}
	for _, id := range ids {
		own[id] = 1
	}
	if len(own) == 0 {
		return 0, "", rank.ErrSkip
	}

	var best float64
	var bestSeed int64 = -1
	var bestShared int
	for _, seed := range seeds {
		sset := make(corpus.TagSet, len(seed.TagIDs))
		for _, id := range seed.TagIDs {
			sset[id] = 1
		}
		shared, weight := corpus.Intersect(own, sset)
		if shared == 0 {
			continue
		}
		// Normalise by the union, not the seed: a candidate that shares
		// everything with a narrow seed is a better match than one that
		// shares everything with a wide one.
		union := len(own) + len(sset) - shared
		j := float64(shared) / float64(union)
		_ = weight
		if j > best {
			best = j
			bestSeed = seed.ID
			bestShared = shared
		}
	}
	if bestSeed < 0 {
		return 0, "", rank.ErrSkip
	}
	return best, fmt.Sprintf("shares %d tags with seed %d (jaccard %.3f)", bestShared, bestSeed, best), nil
}

// Neighbourhood scores a candidate by how strongly its tags are connected
// to the seeds' tags in the co-occurrence graph, weighted by PMI.
//
// This is the dimension that finds works similar in a way tag overlap
// cannot: two works sharing no tag at all can still sit next to each
// other if their tags co-occur strongly everywhere.
type Neighbourhood struct {
	G              Graph
	SeedTags       []int32
	TotalWorks     float64
	NeighbourLimit int
}

func (Neighbourhood) Name() string { return "neighbourhood" }

func (s Neighbourhood) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	if s.G == nil || len(s.SeedTags) == 0 || len(c.TagIDs) == 0 {
		return 0, "", rank.ErrSkip
	}
	limit := s.NeighbourLimit
	if limit <= 0 {
		limit = 24
	}

	// The seed tags vote for their strongest neighbours; a candidate is
	// scored by the total vote its tags attract. Voting is a set
	// intersection against a map, so this is O(tags x 1) per candidate
	// rather than a scan of the neighbourhood per candidate — the
	// per-candidate-recomputation cost class that dominates a naive
	// implementation.
	votes := make(map[int32]float64, 64)
	for _, st := range s.SeedTags {
		for _, nb := range s.G.NeighbourScore(st, limit, s.TotalWorks) {
			votes[nb.TagID] += nb.PMI
		}
	}
	if len(votes) == 0 {
		return 0, "", rank.ErrSkip
	}

	var total, maxV float64
	var hits int
	var bestTag int32 = -1
	for _, id := range c.TagIDs {
		if v, ok := votes[id]; ok {
			total += v
			hits++
			if v > maxV {
				maxV = v
				bestTag = id
			}
		}
	}
	if hits == 0 {
		return 0, "", rank.ErrSkip
	}
	// Normalise by the candidate's own tag count so a work with 200 tags
	// does not out-score a focused one purely by having more chances.
	score := total / math.Sqrt(float64(len(c.TagIDs)))
	return score, fmt.Sprintf("%d tags reach the seed neighbourhood; strongest is %q (pmi-weighted vote %.2f)",
		hits, s.G.Name(bestTag), maxV), nil
}

// Quality scores kudos per 1k words, the "praise per effort" measure.
//
// kudos is used rather than bookmarks because bookmarks is NULL for
// 112,890 of 112,935 works in the real corpus: ordering by it sorts by
// NULL. A NULL is not a zero, and a zero is not a quality.
type Quality struct{}

func (Quality) Name() string { return "quality" }

func (Quality) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	kudos := c.Stats["kudos"]
	words := c.Stats["word_count"]
	if kudos <= 0 {
		return 0, "", rank.ErrSkip
	}
	if words < 1000 {
		// Too short for a per-1k rate to mean anything; the ratio would
		// be dominated by the denominator.
		return 0, "", rank.ErrSkip
	}
	rate := kudos / (words / 1000)
	// Squash: one very popular work should not outrank everything by a
	// hundredfold, because a linear rate makes the top of any list a
	// single outlier.
	score := math.Log1p(rate) / math.Log1p(500)
	if score > 1 {
		score = 1
	}
	return score, fmt.Sprintf("%.0f kudos per 1k words over %d words", rate, int(words)), nil
}

// Recency scores how recently a work was updated, on a 365-day half-life.
type Recency struct {
	Now float64 // epoch days
}

func (Recency) Name() string { return "recency" }

func (s Recency) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	updated := c.Stats["update_date"]
	if updated == 0 {
		// 0 means "unparseable or absent", not "epoch": treating it as
		// ancient would rank every undated work last.
		return 0, "", rank.ErrSkip
	}
	now := s.Now
	if now == 0 {
		return 0, "", rank.ErrSkip
	}
	age := now - updated
	if age < 0 {
		age = 0
	}
	score := math.Exp2(-age / 365)
	return score, fmt.Sprintf("updated %.0f days ago (halflife 365d)", age), nil
}

// Popularity scores log-scaled hits and kudos together.
type Popularity struct{}

func (Popularity) Name() string { return "popularity" }

func (Popularity) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	hits := c.Stats["hits"]
	kudos := c.Stats["kudos"]
	if hits <= 0 && kudos <= 0 {
		return 0, "", rank.ErrSkip
	}
	// Log because hits span five orders of magnitude in the corpus; a
	// linear score would make the whole list one work and a tie-break.
	score := math.Log1p(hits)/math.Log1p(1e6)*0.6 + math.Log1p(kudos)/math.Log1p(1e5)*0.4
	if score > 1 {
		score = 1
	}
	return score, fmt.Sprintf("%d hits, %d kudos", int(hits), int(kudos)), nil
}

// Embedding scores cosine similarity in the tag-embedding space.
type Embedding struct {
	Store Store
	// SeedVec is the mean of the seeds' embeddings.
	SeedVec []float32
}

func (Embedding) Name() string { return "embedding" }

func (s Embedding) Score(c rank.Candidate, _ []rank.Candidate, st rank.Store) (float64, string, error) {
	store := s.Store
	if store == nil {
		store = st
	}
	if store == nil {
		return 0, "", rank.ErrSkip
	}
	vec, ok, err := store.Embedding(context.Background(), c.Kind, c.ID)
	if err != nil {
		return 0, "", err
	}
	if !ok || len(vec) == 0 || len(s.SeedVec) != len(vec) {
		return 0, "", rank.ErrSkip
	}
	sim := Cosine(vec, s.SeedVec)
	if sim <= 0 {
		return 0, "", rank.ErrSkip
	}
	return sim, fmt.Sprintf("cosine similarity %.3f in %d-dim tag space", sim, len(vec)), nil
}

// Store is the embedding read the Embedding signal needs.
type Store interface {
	Embedding(ctx context.Context, kind string, id int64) ([]float32, bool, error)
}

// Cosine returns the cosine similarity of two vectors, 0 if either has no
// magnitude.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// MeanVec averages vectors, skipping absent ones.
func MeanVec(vecs [][]float32) []float32 {
	if len(vecs) == 0 {
		return nil
	}
	dim := 0
	for _, v := range vecs {
		if len(v) > dim {
			dim = len(v)
		}
	}
	if dim == 0 {
		return nil
	}
	out := make([]float32, dim)
	n := 0
	for _, v := range vecs {
		if len(v) != dim {
			continue
		}
		for i := range v {
			out[i] += v[i]
		}
		n++
	}
	if n == 0 {
		return nil
	}
	for i := range out {
		out[i] /= float32(n)
	}
	return out
}

// SortEvidence orders evidence strongest-first, so a response reads
// top-down and the reason for the score is visible immediately.
func SortEvidence(e []rank.Evidence) {
	sort.SliceStable(e, func(i, j int) bool {
		ai := e[i].Value * e[i].Weight
		aj := e[j].Value * e[j].Weight
		return ai > aj
	})
}
