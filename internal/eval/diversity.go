package eval

// Intra-list diversity: what the ranking BOUGHT for the recall it cost.
//
// The evaluator could say how many held-out works a ranking found (recall), how
// high it placed them (NDCG) and how many readers got anything at all
// (coverage). It could not say whether the list a reader receives is twelve
// variations on one fic — which is the thing the MMR pass exists to prevent and
// the thing a reader complains about first. Two tunes can have identical recall
// and be completely different products.
//
// # Why this is measured at all, and what it is compared against
//
// `diversify.Apply` enforces marginal relevance at lambda 0.75 by default, which
// is a deliberate trade: some recall given up for a list that is not one work
// repeated. Without a diversity number in `eval_runs`, that trade is an
// assertion rather than a measurement: a tune change that quietly made the pass
// irrelevant (or turned it off) would show up as a recall gain, and nobody could
// tell it apart from an improvement.
//
// # The numbers
//
//   - TagOverlap is the mean pairwise Jaccard similarity of the tag sets in the
//     returned list. 0.0 means no two works share a tag; 1.0 means one work
//     repeated. It is the same similarity the MMR pass minimises, so the two are
//     directly comparable.
//   - DistinctFandoms counts distinct fandom groups among the returned works,
//     using the same shape-based grouping (`fandom.GroupKey`) the per-group cap
//     uses. A cap keyed on `tag_type='fandoms'` would count nothing on this
//     mirror — the column is dead — so the count uses the grouper the ranking
//     actually applies.
//
// Both are averaged per reader, like recall: a reader who receives 50 results
// must not outweigh one who receives 10.
//
// # Absent is not zero
//
// `eval.Recommender` returns ids, which carry no tags. A recommender that cannot
// say what tags a work has gets no diversity number — and the report says so, in
// DiversitySkipped, rather than reporting 0.0 overlap. Zero overlap is a GOOD
// result and "not measured" is not; a report that renders them the same way is
// the accepted-and-ignored shape this project refuses elsewhere.

import (
	"context"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/fandom"
)

// WorkTags is what a returned work carries, for the diversity measurement.
type WorkTags struct {
	IDs   []int32
	Names []string
}

// TagSource is the optional half of a Recommender.
//
// It is a separate interface rather than a method on Recommender because the
// evaluator must keep working with a recommender that cannot supply tags (a stub
// in a test, a deployment with no corpus): diversity is then reported as
// unmeasured instead of failing the run.
type TagSource interface {
	TagsFor(ctx context.Context, ids []int64) (map[int64]WorkTags, error)
}

// Diversity is one reader's list, measured.
//
// Items is how many works had tags and therefore entered the calculation: a
// list of twelve works where four carry no tags measures eight of them, and the
// report says which. Overlap is undefined below two items and is then 0 with
// Items < 2, which the averaging excludes rather than counting as "perfectly
// diverse".
type Diversity struct {
	Items           int     `json:"items"`
	TagOverlap      float64 `json:"tag_overlap"`
	DistinctFandoms int     `json:"distinct_fandoms"`
}

// Measure computes the diversity of one returned list.
//
// k bounds the list the same way recall@k does: a recommender asked for 50 ids
// is not made more diverse by the ones past the cut-off.
func Measure(ctx context.Context, src TagSource, ids []int64, k int) (Diversity, error) {
	var d Diversity
	if src == nil || len(ids) == 0 {
		return d, nil
	}
	if k > 0 && len(ids) > k {
		ids = ids[:k]
	}
	tags, err := src.TagsFor(ctx, ids)
	if err != nil {
		return d, err
	}

	// Deduplicate the ids first: a list that repeats a work would otherwise be
	// scored as ONE work's overlap with itself, i.e. maximally diverse, which is
	// the opposite of the truth.
	seen := make(map[int64]bool, len(ids))
	sets := make([][]int32, 0, len(ids))
	fandoms := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		wt, ok := tags[id]
		if !ok || len(wt.IDs) == 0 {
			continue
		}
		sets = append(sets, wt.IDs)
		if key := fandom.GroupKey(wt.Names); key != "" {
			fandoms[key] = true
		}
	}
	d.Items = len(sets)
	d.DistinctFandoms = len(fandoms)
	if len(sets) < 2 {
		return d, nil
	}

	// Mean pairwise Jaccard over SORTED, deduplicated tag ids: intersection over
	// union, computed without allocating a set per comparison.
	sorted := make([][]int32, len(sets))
	for i, s := range sets {
		cp := make([]int32, len(s))
		copy(cp, s)
		sort.Slice(cp, func(a, b int) bool { return cp[a] < cp[b] })
		// Deduplicate in place: work_tags can carry the same tag under two
		// types, and a repeated id would inflate the intersection.
		out := cp[:0]
		for j, v := range cp {
			if j == 0 || v != cp[j-1] {
				out = append(out, v)
			}
		}
		sorted[i] = out
	}
	sum := 0.0
	pairs := 0
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			sum += jaccardSorted(sorted[i], sorted[j])
			pairs++
		}
	}
	if pairs > 0 {
		d.TagOverlap = sum / float64(pairs)
	}
	return d, nil
}

// jaccardSorted computes intersection over union for two sorted id slices.
func jaccardSorted(a, b []int32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	i, j, inter := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			inter++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
