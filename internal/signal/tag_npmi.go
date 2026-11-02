package signal

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// TagNPMI scores tag agreement between a candidate and the seeds using both halves
// of the evidence a pair of works can carry:
//
//	direct(t)  how RARE the shared tag is                        (what IDF is for)
//	assoc(t)   how much MORE THAN CHANCE t accompanies the seed's tags   (what nPMI is for)
//
// and takes the max of the two per tag, then the mean over the candidate's tags.
//
// It is a separate signal rather than a change to IDFWeightedOverlap, because the tune
// exists to choose between them: an eval can weight one, the other, or both, and the
// number that decides is a measurement rather than an argument.
//
// Every term is in [0,1], so the score needs no saturation constant -- the property
// Neighbourhood had to buy with NeighbourSaturation.
//
// The two halves are not interchangeable. IDF answers "is this tag surprising"; nPMI
// answers "do these two tags belong together". A shared ubiquitous tag must score
// near zero (IDF's job: sharing "General Audiences" is not evidence), while a tag the
// seeds do not carry at all can still be evidence if it accompanies them more often
// than chance (nPMI's job). Neither alone is a similarity; together they are.
type TagNPMI struct {
	// G supplies the co-occurrence counts and per-tag frequencies. No SQL: the
	// layering rule puts the query in the engine and the arithmetic here.
	G Graph
	// DocFreq is the per-tag work count, for the direct-match term. The same map
	// IDFWeightedOverlap takes.
	DocFreq map[int32]float64
	// Total is the number of works in the corpus; both normalisations divide by it.
	Total float64
	// SeedTags is the union of the seeds' tags, resolved once per request.
	SeedTags []int32
	// NeighbourLimit bounds how many associated tags each seed tag contributes.
	NeighbourLimit int
	// MinCount is the co-occurrence floor: a pair seen fewer than this many times is
	// not evidence. nPMI is biased toward rare pairs (co=1 on two df=5 tags reads
	// about 0.72), and the floor -- not the normalisation -- is what stops a single
	// coincidence from becoming an association. 1 disables it, for tests.
	MinCount float64
	// MaxTags bounds the work per candidate, as TagOverlap does: a work with
	// thousands of freeform tags is a bot or a fandom dump.
	MaxTags int
}

func (TagNPMI) Name() string { return "tag_npmi" }

// idfNorm is log(N/df) rescaled to [0,1] by log(N): 1 for a tag on one work, 0 for a
// tag on every work. A tag absent from the table is maximally rare, matching idfOf's
// treatment of a table-completeness gap.
func idfNorm(df, total float64) float64 {
	if total <= 1 {
		return 0
	}
	denom := math.Log(total)
	if denom <= 0 {
		return 0
	}
	if df <= 0 {
		return 1
	}
	if df >= total {
		return 0
	}
	v := math.Log(total/df) / denom
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// npmiCache is its own LRU, not the neighbourhood's: two signals sharing one cache
// would hand each other their numbers, and the neighbourhood stores sums while this
// one stores best-match values.
var npmiCache = newLRU[int32](8)

// assocFor memoises the seed-tag association map, which is identical for every
// candidate in a request.
//
// The key carries the seed set, the limit, the floor, the corpus size and the size of
// the frequency table. That is everything the map is a function of within a process --
// the corpus does not change under a running server, so a stale entry is not reachable
// without a restart, and a test that wants a different graph uses different seeds.
func (s TagNPMI) assocFor() (map[int32]float64, bool) {
	limit := s.NeighbourLimit
	if limit <= 0 {
		limit = 24
	}
	minCount := s.MinCount
	if minCount <= 0 {
		minCount = 1
	}
	key := "npmi:" + voteKey(s.SeedTags, limit) +
		":" + strconv.FormatFloat(minCount, 'f', 1, 64) +
		":" + strconv.FormatFloat(s.Total, 'f', 0, 64) +
		":" + strconv.Itoa(len(s.DocFreq))
	if v, ok := npmiCache.get(key); ok {
		return v, len(v) > 0
	}
	out := make(map[int32]float64, 64)
	for _, st := range s.SeedTags {
		fq := s.DocFreq[st]
		for _, nb := range s.G.NeighbourScore(st, limit, s.Total) {
			if nb.Count < minCount {
				continue
			}
			// Best association per tag: a tag reached from six seed tags is not six
			// times as relevant, and summing would rebuild the scale problem.
			if v := graph.NPMI(fq, s.DocFreq[nb.TagID], nb.Count, s.Total); v > out[nb.TagID] {
				if v > 0 {
					out[nb.TagID] = v
				}
			}
		}
	}
	npmiCache.put(key, out)
	return out, len(out) > 0
}

func (s TagNPMI) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	if s.G == nil || len(s.DocFreq) == 0 || s.Total <= 1 || len(s.SeedTags) == 0 || len(c.TagIDs) == 0 {
		return 0, "", rank.ErrSkip
	}
	ids := c.TagIDs
	if s.MaxTags > 0 && len(ids) > s.MaxTags {
		ids = ids[:s.MaxTags]
	}
	seeds := make(map[int32]struct{}, len(s.SeedTags))
	for _, t := range s.SeedTags {
		seeds[t] = struct{}{}
	}
	assoc, hasAssoc := s.assocFor()

	var total, best float64
	var bestTag int32 = -1
	hits := 0
	seen := make(map[int32]struct{}, len(ids))
	var matched []int32
	for _, t := range ids {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		term := 0.0
		if _, shared := seeds[t]; shared {
			term = idfNorm(s.DocFreq[t], s.Total)
			if len(matched) < 8 {
				matched = append(matched, t)
			}
		}
		if hasAssoc {
			if v := assoc[t]; v > term {
				term = v
			}
		}
		if term <= 0 {
			continue
		}
		hits++
		total += term
		if term > best {
			best = term
			bestTag = t
		}
	}
	if hits == 0 || total <= 0 {
		// Nothing shared and nothing associated: this candidate carries no evidence
		// for these seeds. Zero is a real answer, not a skipped dimension -- skipping
		// would let it inherit the other signals' ranking without being penalised.
		return 0, "no shared or associated tags", nil
	}
	// The denominator is every DISTINCT tag the candidate carries, not the hits: a
	// work with two hundred tags must not out-score a focused one by having two
	// hundred chances to match.
	score := total / float64(len(seen))
	if score > 1 {
		score = 1
	}
	sort.Slice(matched, func(i, j int) bool {
		return idfNorm(s.DocFreq[matched[i]], s.Total) > idfNorm(s.DocFreq[matched[j]], s.Total)
	})
	rarest := "none"
	if len(matched) > 0 {
		rarest = s.G.Name(matched[0])
	}
	reason := fmt.Sprintf("%d of %d tags score; strongest %q (%.3f), rarest shared %s",
		hits, len(seen), s.G.Name(bestTag), best, rarest)
	return score, reason, nil
}

// Ensure the signal satisfies the interface.
var _ rank.Signal = TagNPMI{}
