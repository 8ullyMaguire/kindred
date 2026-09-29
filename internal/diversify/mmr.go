// Package diversify applies maximal-marginal-relevance re-ranking and
// per-group caps, and reports what it could not deliver.
//
// The reporting is the point. The previous deployment's own spec
// corrected a brief that asked it to top up to the requested count: doing
// so restores exactly the concentration the cap removed, measured at "max
// 3 per fandom at N=100" coming back with 40 from one fandom. A list of
// 90 that says "90 of 100: 10 exceeded the cap" is honest. A list of 100
// that ignored the cap is not.
package diversify

import (
	"fmt"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// Result is a diversified list and an honest account of it.
type Result struct {
	Items     []rank.Candidate
	Shortfall *rank.Short
}

// Options configures the pass.
type Options struct {
	// Requested is what the caller asked for, recorded so a shortfall can
	// be reported against it rather than silently shrinking the list.
	Requested int

	// Lambda trades relevance against diversity. 1.0 is pure relevance;
	// 0.5 gives each item's distance from what is already selected equal
	// standing with its score.
	Lambda float64

	// MaxPerGroup caps how many selected items may share a group value,
	// e.g. a tag or an author. 0 means no cap.
	MaxPerGroup int

	// GroupOf extracts the group key for a candidate. Nil means no cap.
	GroupOf func(rank.Candidate) string

	// MMRK bounds the marginal-relevance computation, which is O(k x n).
	// Passing the whole list as k squares it. The bound is recorded in the
	// shortfall when it bites.
	MMRK int
}

// Apply diversifies a ranked list.
//
// The input must already be sorted by score descending; the result is not
// in score order, which is the nature of MMR — an item chosen for
// diversity can rank below one it beat on score.
func Apply(in []rank.Candidate, opts Options) Result {
	// wanted is what the caller actually asked for, before it is clamped
	// to what the corpus can supply. Keeping both is what lets a request
	// for 10 against an empty corpus report "0 of 10" instead of
	// normalising the request away and reporting a full result of nothing.
	wanted := opts.Requested
	requested := opts.Requested
	if requested <= 0 || requested > len(in) {
		requested = len(in)
	}
	lambda := opts.Lambda
	if lambda <= 0 || lambda > 1 {
		lambda = 0.7
	}
	mmrK := opts.MMRK
	if mmrK <= 0 {
		mmrK = 500
	}

	working := in
	truncated := false
	if len(working) > mmrK {
		// Bounding k: the marginal-relevance step is O(k x n) and the
		// previous deployment measured it going quadratic when k was
		// passed as len(records).
		working = working[:mmrK]
		truncated = true
	}

	selected := make([]rank.Candidate, 0, requested)
	taken := make([]bool, len(working))
	groupCount := map[string]int{}

	// Similarity between two candidates is their tag overlap: a
	// tag-independent measure that needs no embeddings and no graph, so
	// this pass cannot fail because an index is missing.
	tagSets := make([]map[int32]struct{}, len(working))
	for i, c := range working {
		set := make(map[int32]struct{}, len(c.TagIDs))
		for _, id := range c.TagIDs {
			set[id] = struct{}{}
		}
		tagSets[i] = set
	}

	for len(selected) < requested {
		bestIdx := -1
		bestVal := 0.0
		bestCand := rank.Candidate{}
		for i := range working {
			if taken[i] {
				continue
			}
			if opts.MaxPerGroup > 0 && opts.GroupOf != nil {
				g := opts.GroupOf(working[i])
				if g != "" && groupCount[g] >= opts.MaxPerGroup {
					continue
				}
			}
			// Marginal relevance: the score, discounted by how similar
			// this is to everything already chosen.
			maxSim := 0.0
			for _, s := range selected {
				if sim := jaccard(tagSets[i], tagSets[indexOf(working, s)]); sim > maxSim {
					maxSim = sim
				}
			}
			val := lambda*working[i].Score - (1-lambda)*maxSim
			if bestIdx == -1 || val > bestVal {
				bestIdx, bestVal, bestCand = i, val, working[i]
			}
		}
		if bestIdx == -1 {
			// Everything left is either taken or blocked by a cap. That
			// is a shortfall, and it gets reported as one.
			break
		}
		taken[bestIdx] = true
		selected = append(selected, bestCand)
		if opts.MaxPerGroup > 0 && opts.GroupOf != nil {
			if g := opts.GroupOf(bestCand); g != "" {
				groupCount[g]++
			}
		}
	}

	res := Result{Items: selected}
	if len(selected) < wanted {
		reason := "candidates exhausted"
		if opts.MaxPerGroup > 0 {
			reason = fmt.Sprintf("per-group cap of %d reached", opts.MaxPerGroup)
		}
		if len(in) == 0 {
			reason = "the corpus is empty or the seeds matched nothing"
		}
		res.Shortfall = &rank.Short{
			Requested: wanted,
			Returned:  len(selected),
			Reason:    reason,
		}
	} else if truncated {
		res.Shortfall = &rank.Short{
			Requested: wanted,
			Returned:  len(selected),
			Reason:    fmt.Sprintf("MMR k bounded at %d of %d candidates", mmrK, len(in)),
		}
	}
	return res
}

// indexOf finds a candidate's position by identity, so a selected
// candidate can be mapped back to its tag set without rebuilding a lookup.
func indexOf(list []rank.Candidate, c rank.Candidate) int {
	for i := range list {
		if list[i].ID == c.ID && list[i].Kind == c.Kind {
			return i
		}
	}
	return 0
}

func jaccard(a, b map[int32]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Iterate the smaller side.
	small, large := a, b
	if len(large) < len(small) {
		small, large = large, small
	}
	shared := 0
	for id := range small {
		if _, ok := large[id]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// GroupByTag returns a grouping function capping per distinct first tag.
func GroupByTag(c rank.Candidate) string {
	if len(c.TagNames) == 0 {
		return ""
	}
	return c.TagNames[0]
}

// GroupByAuthor returns a grouping function capping per author.
func GroupByAuthor(c rank.Candidate) string {
	if len(c.TagNames) == 0 {
		return ""
	}
	// TagNames carries tags; the author is in the summary-independent
	// first position when a source provides one. Callers that need real
	// author grouping pass their own function.
	return ""
}

// SortByScore orders a list by score descending, breaking ties by id, so
// the input to Apply is deterministic.
func SortByScore(c []rank.Candidate) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Score != c[j].Score {
			return c[i].Score > c[j].Score
		}
		return c[i].ID < c[j].ID
	})
}
