package eval

// The diversity metric as the RUN computes it: averaged per reader, excluded
// where the list is too short to have a pair, and reported as unmeasured when
// the recommender cannot supply tags.
//
// These are separate from diversity_test.go because that file tests ONE list's
// arithmetic; this file tests the wiring — that Run measures the ids it just
// received, at the same k as recall, and that a recommender without tags does not
// silently report 0.0 overlap.

import (
	"context"
	"testing"
)

// taggedRec is a Recommender that can also supply tags.
type taggedRec struct {
	ids  map[int64][]int64
	tags map[int64]WorkTags
}

func (r taggedRec) Recommend(_ context.Context, seeds []int64, n int) ([]int64, error) {
	// Every reader gets the same list; the split's own ids are irrelevant here.
	for _, list := range r.ids {
		return list, nil
	}
	return nil, nil
}

func (r taggedRec) TagsFor(_ context.Context, ids []int64) (map[int64]WorkTags, error) {
	out := make(map[int64]WorkTags, len(ids))
	for _, id := range ids {
		out[id] = r.tags[id]
	}
	return out, nil
}

func diversitySplit() Split {
	return Split{
		Users:     2,
		Bookmarks: 8,
		Test: map[int64][]int64{
			1: {900, 901},
			2: {902, 903},
		},
		Train: map[int64][]int64{
			1: {10, 11, 12, 13},
			2: {20, 21, 22, 23},
		},
	}
}

func TestRunMeasuresDiversityOfTheListItReturned(t *testing.T) {
	// Four works in two pairs: two of the six pairs are identical works and
	// four share nothing, so the mean is 2/6 = 1/3 exactly.
	rec := taggedRec{
		ids: map[int64][]int64{1: {1, 2, 3, 4}},
		tags: map[int64]WorkTags{
			1: tags(10, 11),
			2: tags(10, 11),
			3: tags(20, 21),
			4: tags(20, 21),
		},
	}
	rep, err := Run(context.Background(), rec, diversitySplit(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DiversityRuns != 2 {
		t.Fatalf("diversity was measured over %d readers, want 2", rep.DiversityRuns)
	}
	if rep.TagOverlap < 0.332 || rep.TagOverlap > 0.335 {
		t.Fatalf("tag overlap = %.4f, want 0.333 (two identical pairs of six)", rep.TagOverlap)
	}
	if rep.DiversitySkipped != "" {
		t.Fatalf("a measured run reported a skip reason: %q", rep.DiversitySkipped)
	}
}

func TestARecommenderWithoutTagsReportsUnmeasuredRatherThanZero(t *testing.T) {
	rec := &fakeRecommender{order: []int64{1, 2, 3}}
	rep, err := Run(context.Background(), rec, diversitySplit(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DiversityRuns != 0 {
		t.Fatalf("a recommender with no tag source reported %d measured readers", rep.DiversityRuns)
	}
	if rep.DiversitySkipped == "" {
		t.Fatal("an unmeasured run said nothing about why; a report that shows 0.000 overlap " +
			"is claiming a perfectly diverse list it never looked at")
	}
	if rep.TagOverlap != 0 || rep.DistinctFandoms != 0 {
		t.Fatalf("unmeasured run carries numbers: overlap %.3f fandoms %.3f", rep.TagOverlap, rep.DistinctFandoms)
	}
}

// A list of one work has no pairs. It must not enter the average at all: folding
// it in as 0.0 overlap would let a run whose recommendations mostly returned one
// item report a beautifully diverse corpus.
func TestASingleItemListDoesNotPullTheAverageDown(t *testing.T) {
	rec := taggedRec{
		ids:  map[int64][]int64{1: {1}},
		tags: map[int64]WorkTags{1: tags(10, 11)},
	}
	rep, err := Run(context.Background(), rec, diversitySplit(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DiversityRuns != 0 {
		t.Fatalf("single-item lists were counted (%d readers); overlap is undefined there", rep.DiversityRuns)
	}
}

func TestDiversityUsesTheSameCutOffAsRecall(t *testing.T) {
	// The first two works are identical; the rest are all different. At k=2 the
	// overlap is 1.0; at k=4 it drops. If the metric ignored k it would measure
	// a list nobody received.
	rec := taggedRec{
		ids: map[int64][]int64{1: {1, 2, 3, 4}},
		tags: map[int64]WorkTags{
			1: tags(10, 11),
			2: tags(10, 11),
			3: tags(30, 31),
			4: tags(40, 41),
		},
	}
	short, err := Run(context.Background(), rec, diversitySplit(), 2)
	if err != nil {
		t.Fatal(err)
	}
	long, err := Run(context.Background(), rec, diversitySplit(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if short.TagOverlap != 1 {
		t.Fatalf("overlap at k=2 = %.3f, want 1.0 (the two returned works are identical)", short.TagOverlap)
	}
	if long.TagOverlap >= short.TagOverlap {
		t.Fatalf("overlap at k=4 (%.3f) is not lower than at k=2 (%.3f); the cut-off is ignored",
			long.TagOverlap, short.TagOverlap)
	}
}
