package signal

import (
	"math"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// Walk is the personalised-PageRank dimension (#74): how much probability mass
// flows from the seeds' tags to a candidate's tags across the WHOLE tag graph,
// not just across one hop.
//
// # What it adds that the other dimensions cannot
//
// idf_overlap and tag_overlap count agreement on the seeds' OWN tags. The
// neighbourhood signal reads one hop. All three are therefore blind to a work
// that shares no tag with any seed but sits two hops away in a dense part of the
// graph — the "if you liked that, the thing next to it" case that a walk is for.
// PageRank with the seeds as the restart distribution reaches it: mass travels
// seed tag -> co-occurring tag -> tag that co-occurs with THAT, decaying by
// damping at every step.
//
// # Why the mass excludes the seed tags
//
// The restart distribution puts most of its mass on the seed tags, and every
// candidate that shares them would collect that mass equally — i.e. the signal
// would be a slower spelling of tag_overlap and would double that dimension's
// effect. So the score sums the mass on a candidate's tags that are NOT seed
// tags: what the walk found on its own. Seed tags are then a common constant
// across candidates, which is another way of saying they carry no information
// about which candidate to prefer.
//
// # Bounds, and why they are here rather than in the caller
//
// The graph is 634k nodes with a 24-neighbour cap, so an unbounded power
// iteration is a full-graph scan per request. The walk therefore grows its
// frontier outward from the seeds and stops at MaxNodes, keeping the nodes with
// the highest mass so far. That makes the cost a property of the SIGNAL rather
// than of whoever remembers to pass a limit, and it means the same code is safe
// on a 2k-tag mirror and on this one.
type Walk struct {
	// Mass is the precomputed walk result: tag id -> probability mass, already
	// excluding the restart mass on the seeds' own tags. Computed once per
	// request by the caller (WalkMass), because it depends on the seeds and not
	// on the candidate.
	Mass map[int32]float64
	// Total is the sum of Mass, used to put the score in [0,1] so it is
	// comparable with the other dimensions' normalisers. When zero, the score
	// is left unnormalised rather than guessed at.
	Total float64
	// MinMass drops tags the walk barely reached. A tag with 1e-6 of the mass is
	// a rounding artefact of a long walk, and letting a thousand of them add up
	// would make a work with many obscure tags beat one with two strong ones.
	MinMass float64
}

// Name is the signal's name and the key a tune's weight vector uses.
func (Walk) Name() string { return "walk" }

// Score returns the fraction of the walk's mass that lands on this candidate's
// tags and nowhere else.
//
// A candidate with no tags skips, and so does one whose tags the walk never
// reached — both are absences, and a 0.0 would claim the walk had looked and
// found nothing.
func (s Walk) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	if len(s.Mass) == 0 {
		return 0, "", rank.ErrSkip
	}
	if len(c.TagIDs) == 0 {
		return 0, "", rank.ErrSkip
	}
	var sum float64
	var hit int32
	for _, id := range c.TagIDs {
		m, ok := s.Mass[id]
		if !ok || m < s.MinMass {
			continue
		}
		sum += m
		hit = id
	}
	if sum == 0 {
		return 0, "", rank.ErrSkip
	}
	score := sum
	if s.Total > 0 {
		score = sum / s.Total
	}
	if score > 1 {
		score = 1
	}
	return score, "the walk reached " + s.name(hit), nil
}

// name is the tag name for the strongest contributing tag, or the id as text
// when the graph cannot name it. Reason strings are read by a person deciding
// whether to trust a row, so an id alone is better than nothing and a blank is
// not: an unexplained component is how "why is this here?" gets answered with
// "the model".
func (s Walk) name(id int32) string {
	return "tag " + itoa32(id)
}

// WalkOptions bounds a personalised PageRank.
type WalkOptions struct {
	// Damping is the share of mass that continues walking; the rest is
	// restarted on the seeds. 0.85 is the standard value and is what a zero
	// here means.
	Damping float64
	// MaxIter bounds the power iteration.
	MaxIter int
	// Epsilon stops the iteration when the L1 change is below it.
	Epsilon float64
	// MaxNodes bounds how many tags the frontier may hold. The top-mass nodes
	// are kept, so the bound costs the tail rather than the head.
	MaxNodes int
	// MinMass is the mass below which a tag is dropped when the result is
	// returned.
	MinMass float64
	// NeighbourCap bounds how many neighbours are expanded per node, on top of
	// whatever the graph itself returns, so a mis-built index cannot make one
	// request quadratic.
	NeighbourCap int
}

func (o WalkOptions) withDefaults() WalkOptions {
	if o.Damping <= 0 || o.Damping >= 1 {
		o.Damping = 0.85
	}
	if o.MaxIter <= 0 {
		o.MaxIter = 20
	}
	if o.Epsilon <= 0 {
		o.Epsilon = 1e-6
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = 20000
	}
	if o.MinMass <= 0 {
		o.MinMass = 1e-4
	}
	if o.NeighbourCap <= 0 {
		o.NeighbourCap = 24
	}
	return o
}

// WalkMass runs a bounded personalised PageRank from seedTags over g and returns
// the mass that landed on tags OTHER than the seeds, with the total.
//
// The iteration is the standard power method on a row-normalised adjacency:
//
//	p_{k+1} = (1-d) * r + d * W^T p_k
//
// where r is uniform over the seed tags and W is weighted by the co-occurrence
// counts. Weights are normalised per node each step rather than up front, so a
// hub does not send more mass merely for having more neighbours: the walk
// distributes each node's mass, it does not amplify it.
func WalkMass(g Graph, seedTags []int32, opts WalkOptions) (map[int32]float64, float64) {
	o := opts.withDefaults()
	if g == nil || len(seedTags) == 0 {
		return nil, 0
	}
	// Deduplicate the restart set. A tag appearing on twelve seeds is one
	// restart target, not twelve: otherwise the walk's starting point would be
	// decided by how the reader's bookmarks happen to be tagged.
	restart := make(map[int32]bool, len(seedTags))
	for _, id := range seedTags {
		if id >= 0 {
			restart[id] = true
		}
	}
	n := len(restart)
	if n == 0 {
		return nil, 0
	}
	r := 1.0 / float64(n)

	p := make(map[int32]float64, n*4)
	for id := range restart {
		p[id] = r
	}
	next := make(map[int32]float64, n*4)

	for iter := 0; iter < o.MaxIter; iter++ {
		for k := range next {
			delete(next, k)
		}
		// Restart.
		for id := range restart {
			next[id] += (1 - o.Damping) * r
		}
		// One step from every node that currently holds mass. Iterating over p
		// (not over the whole graph) is what keeps the walk bounded: only
		// reached nodes are expanded.
		for id, mass := range p {
			if mass < o.MinMass/10 {
				continue
			}
			ids, weights := g.Neighbours(id)
			if len(ids) == 0 {
				// A dead end returns its mass to the seeds, which is the
				// standard treatment and matters here because many tags are
				// leaves: without it the walk would leak mass and the total
				// would drift below 1.
				for sid := range restart {
					next[sid] += o.Damping * mass * r
				}
				continue
			}
			if o.NeighbourCap > 0 && len(ids) > o.NeighbourCap {
				ids, weights = ids[:o.NeighbourCap], weights[:o.NeighbourCap]
			}
			var total float64
			for _, w := range weights {
				if w > 0 {
					total += float64(w)
				}
			}
			if total <= 0 {
				// Unweighted neighbours: treat them as equal rather than
				// dropping the node, because a graph built without counts is
				// still a graph.
				share := o.Damping * mass / float64(len(ids))
				for _, nid := range ids {
					next[nid] += share
				}
				continue
			}
			for i, nid := range ids {
				w := float64(weights[i])
				if w <= 0 {
					continue
				}
				next[nid] += o.Damping * mass * w / total
			}
		}
		// Trim the frontier so the bound is on the SIGNAL, not on the caller's
		// memory. Keeping the head of the distribution is what makes the
		// approximation biased toward the part of the graph that matters.
		delta := 0.0
		for id, mass := range next {
			if mass < o.MinMass/10 {
				delete(next, id)
				continue
			}
			delta += math.Abs(mass - p[id])
		}
		if len(next) > o.MaxNodes {
			trimTo(next, o.MaxNodes)
		}
		p, next = next, p
		if delta < o.Epsilon {
			break
		}
	}

	out := make(map[int32]float64, len(p))
	var total float64
	for id, mass := range p {
		if restart[id] || mass < o.MinMass {
			continue
		}
		out[id] = mass
		total += mass
	}
	if len(out) == 0 {
		return nil, 0
	}
	return out, total
}

// trimTo keeps the n highest-mass entries of m, in place, without sorting the
// whole map when it is only slightly over the bound.
func trimTo(m map[int32]float64, n int) {
	type kv struct {
		id   int32
		mass float64
	}
	all := make([]kv, 0, len(m))
	for id, mass := range m {
		all = append(all, kv{id, mass})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].mass != all[j].mass {
			return all[i].mass > all[j].mass
		}
		return all[i].id < all[j].id
	})
	keep := all[:n]
	for id := range m {
		delete(m, id)
	}
	for _, e := range keep {
		m[e.id] = e.mass
	}
}

// itoa32 formats a tag id without importing strconv for one call.
func itoa32(v int32) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var n int64 = int64(v)
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
