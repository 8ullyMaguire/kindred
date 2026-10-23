package web

import (
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
)

// TestDistinctiveTagsOrdersByRarity is idea 64 at its unit: the rarest tags
// come first, a tag on every work is not shown at all, and the count is
// bounded. Each of the three is a separate way the block could be wrong while
// still rendering something plausible.
func TestDistinctiveTagsOrdersByRarity(t *testing.T) {
	df := &corpus.DocFreq{
		Total: 1000,
		ByTag: map[int32]float64{
			1: 900, // on most works: says nothing about this one
			2: 12,  // rarest
			3: 40,
			4: 1000, // on every work: zero evidence, dropped
			5: 0,    // present with a genuine zero: maximally rare, kept
		},
	}
	tags := []corpus.TagPair{
		{ID: 1, Name: "fluff"},
		{ID: 2, Name: "selkie au"},
		{ID: 3, Name: "time travel"},
		{ID: 4, Name: "complete"},
		{ID: 5, Name: "one (1) fan"},
		{ID: 6, Name: "absent from the table"},
	}

	// Tag 5 is on zero works while this work carries it: the table and the
	// work disagree, so it is dropped rather than called the rarest thing
	// here (see the refusal in distinctive.go).
	got := distinctiveTags(tags, df, 3)
	want := []string{"selkie au", "time travel", "fluff"}
	if len(got) != len(want) {
		t.Fatalf("got %d tags (%v), want %d", len(got), names(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Errorf("position %d = %q, want %q (full: %v)", i, got[i].Name, want[i], names(got))
		}
	}

	if containsName(got, "one (1) fan") {
		t.Errorf("a zero-frequency tag (a stale table) was called distinctive: %v", names(got))
	}

	// A tag on EVERY work is dropped even when nothing else is available:
	// "distinctive: complete" would be a claim that every work is
	// distinguished by being complete.
	if only := distinctiveTags([]corpus.TagPair{{ID: 4, Name: "complete"}}, df, 5); len(only) != 0 {
		t.Errorf("a tag on every work was called distinctive: %v", names(only))
	}

	// No table is not "everything is rare".
	if none := distinctiveTags(tags, nil, 5); none != nil {
		t.Errorf("without a document-frequency table, distinctiveTags returned %v", names(none))
	}
	// A non-positive count is a caller bug, not "all of them".
	if none := distinctiveTags(tags, df, 0); none != nil {
		t.Errorf("count 0 returned %v", names(none))
	}
}

func containsName(tags []corpus.TagPair, name string) bool {
	for _, t := range tags {
		if t.Name == name {
			return true
		}
	}
	return false
}

func names(tags []corpus.TagPair) []string {
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = t.Name
	}
	return out
}
