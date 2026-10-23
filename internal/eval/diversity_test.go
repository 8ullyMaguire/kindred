package eval

// The diversity metric's own tests.
//
// Each one pins a way the number could be wrong-but-plausible:
//
//   - a list of near-duplicates must score HIGH overlap (if it scored low, the
//     metric would be measuring nothing);
//   - a list that repeats a work must not be scored as diverse (the duplicate
//     pair would otherwise be "one work compared with itself");
//   - a single work has no pairs, so overlap is 0 and the reader is EXCLUDED
//     from the average rather than counted as perfectly diverse;
//   - a recommender that cannot supply tags must report "not measured", not 0.0.

import (
	"context"
	"fmt"
	"testing"
)

type fakeSource struct {
	tags map[int64]WorkTags
	err  error
}

func (f fakeSource) TagsFor(_ context.Context, ids []int64) (map[int64]WorkTags, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[int64]WorkTags, len(ids))
	for _, id := range ids {
		if wt, ok := f.tags[id]; ok {
			out[id] = wt
		}
	}
	return out, nil
}

func tags(ids ...int32) WorkTags {
	names := make([]string, 0, len(ids))
	for range ids {
		names = append(names, "x")
	}
	return WorkTags{IDs: ids, Names: names}
}

func TestANearDuplicateListScoresHighOverlap(t *testing.T) {
	// Four works sharing three tags out of four: every pair is 3/5 = 0.6.
	src := fakeSource{tags: map[int64]WorkTags{
		1: tags(10, 11, 12, 99),
		2: tags(10, 11, 12, 98),
		3: tags(10, 11, 12, 97),
		4: tags(10, 11, 12, 96),
	}}
	d, err := Measure(context.Background(), src, []int64{1, 2, 3, 4}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if d.Items != 4 {
		t.Fatalf("items = %d, want 4", d.Items)
	}
	if d.TagOverlap < 0.59 || d.TagOverlap > 0.61 {
		t.Fatalf("overlap = %.4f, want 0.6 (3 shared of 5 union)", d.TagOverlap)
	}
}

func TestAVariedListScoresLowOverlap(t *testing.T) {
	src := fakeSource{tags: map[int64]WorkTags{
		1: tags(10, 11),
		2: tags(20, 21),
		3: tags(30, 31),
		4: tags(40, 41),
	}}
	d, err := Measure(context.Background(), src, []int64{1, 2, 3, 4}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if d.TagOverlap != 0 {
		t.Fatalf("overlap = %.4f for four works sharing no tag, want 0", d.TagOverlap)
	}
}

// The duplicate is the trap: comparing a work with itself yields Jaccard 1.0, so
// counting it would make a list that repeats a work look MORE diverse than it is.
func TestARepeatedWorkIsNotScoredAsADiverseOne(t *testing.T) {
	src := fakeSource{tags: map[int64]WorkTags{
		1: tags(10, 11),
		2: tags(10, 11),
		3: tags(10, 11),
	}}
	d, err := Measure(context.Background(), src, []int64{1, 1, 2, 3}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if d.Items != 3 {
		t.Fatalf("items = %d, want 3 (the repeat is dropped)", d.Items)
	}
	if d.TagOverlap != 1 {
		t.Fatalf("overlap = %.4f for three identical works, want 1", d.TagOverlap)
	}
}

func TestASingleWorkHasNoPairsAndIsNotPerfectlyDiverse(t *testing.T) {
	src := fakeSource{tags: map[int64]WorkTags{1: tags(10, 11)}}
	d, err := Measure(context.Background(), src, []int64{1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if d.Items != 1 || d.TagOverlap != 0 {
		t.Fatalf("diversity of one work = %+v, want items 1 and overlap 0 (no pairs to compare)", d)
	}
}

func TestWorksWithoutTagsAreCountedOutLoud(t *testing.T) {
	src := fakeSource{tags: map[int64]WorkTags{
		1: tags(10),
		3: tags(30),
	}}
	d, err := Measure(context.Background(), src, []int64{1, 2, 3}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if d.Items != 2 {
		t.Fatalf("items = %d, want 2 (work 2 has no tags and cannot be compared)", d.Items)
	}
}

func TestKBoundsTheMeasuredList(t *testing.T) {
	src := fakeSource{tags: map[int64]WorkTags{
		1: tags(10),
		2: tags(10, 11, 12, 13, 14, 15, 16, 17), // very different
		3: tags(10),
		4: tags(10),
	}}
	full, err := Measure(context.Background(), src, []int64{1, 2, 3, 4}, 10)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := Measure(context.Background(), src, []int64{1, 2, 3, 4}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if cut.Items != 2 {
		t.Fatalf("items with k=2 = %d, want 2", cut.Items)
	}
	if cut.TagOverlap == full.TagOverlap {
		t.Fatalf("k made no difference (%.4f both ways); the list past the cut-off is being measured", full.TagOverlap)
	}
}

func TestATagLookupFailureIsAnErrorNotZeroOverlap(t *testing.T) {
	src := fakeSource{err: fmt.Errorf("corpus gone")}
	if _, err := Measure(context.Background(), src, []int64{1, 2}, 10); err == nil {
		t.Fatal("a failed tag lookup returned a number instead of an error")
	}
}

func TestJaccardSorted(t *testing.T) {
	tests := []struct {
		a, b []int32
		want float64
	}{
		{[]int32{1, 2}, []int32{1, 2}, 1},
		{[]int32{1, 2}, []int32{3, 4}, 0},
		{[]int32{1, 2}, []int32{2, 3}, 1.0 / 3.0},
		{[]int32{}, []int32{1}, 0},
		{[]int32{1, 3, 5}, []int32{3, 5, 7}, 2.0 / 4.0},
	}
	for _, tt := range tests {
		got := jaccardSorted(tt.a, tt.b)
		if got < tt.want-1e-9 || got > tt.want+1e-9 {
			t.Errorf("jaccardSorted(%v, %v) = %.4f, want %.4f", tt.a, tt.b, got, tt.want)
		}
	}
}
