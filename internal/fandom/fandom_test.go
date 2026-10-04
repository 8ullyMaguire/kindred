package fandom

import "testing"

// The measured reason this package exists: fandom tags arrive as freeforms.
// Both rows below are the same real fandom, and only name shape separates them
// from a trope tag.
func TestLooksLikeFandomOnRealFandomTags(t *testing.T) {
	for _, n := range []string{
		"harry potter - all media types",
		"harry potter - fandom",
		"naruto (anime)",
		"the hobbit - fandom",
		"the last of us (video game)",
		"star wars - all media types",
		"Harry Potter (book)",
		"bluey (cartoon & comics)",
	} {
		if !LooksLikeFandom(n) {
			t.Errorf("LooksLikeFandom(%q) = false, want true", n)
		}
	}
}

// The false-positive half is the half that matters. Every one of these is a
// real freeform tag in the corpus, and misreading any as a fandom collapses
// unrelated works into one capped group.
func TestTropeAndCharacterTagsAreNotFandoms(t *testing.T) {
	for _, n := range []string{
		"dark", "explicit", "hurt no comfort", "alternate universe",
		"hermione granger", "sirius black", "f/m", "m/m",
		"first fan", "reader insert", "fix-it",
		// The shape word inside, not as a qualifier. This is the case an
		// unanchored regex would get wrong.
		"harry potter (tagged) in a crossover (anime-ish)",
		"the video game",
	} {
		if LooksLikeFandom(n) {
			t.Errorf("LooksLikeFandom(%q) = true, want false", n)
		}
	}
}

// A bare fandom name with no qualifier is deliberately not claimed. This is the
// case that produced a ranking with `bluey` and `fionna and cake` in a
// dark-villain reader's top 20.
func TestBareNamesAreNotClaimed(t *testing.T) {
	for _, n := range []string{"harry potter", "star wars", "harry potter fandom"} {
		if LooksLikeFandom(n) {
			t.Errorf("LooksLikeFandom(%q) = true; bare names are not a shape", n)
		}
	}
}

// The qualifier must be an EXACT match from the list. `harry potter (book)`
// qualifies; `harry potter (book 3)` and `harry potter (novelisation)` are
// not on the list and must not be guessed at.
func TestQualifierMatchingIsExact(t *testing.T) {
	if !LooksLikeFandom("naruto (anime)") {
		t.Error("naruto (anime) should qualify")
	}
	if LooksLikeFandom("something (anime-ish)") {
		t.Error("a near-miss qualifier was accepted")
	}
}

// The property the whole cap depends on: qualifiers must not be able to smuggle
// the same fandom past the cap as separate groups.
func TestCanonicalCollapsesQualifiersToOneFandom(t *testing.T) {
	for _, pair := range [][2]string{
		{"Harry Potter - All Media Types", "Harry Potter (book)"},
		{"Harry Potter", "harry potter - all media types"},
	} {
		if Canonical(pair[0]) != Canonical(pair[1]) {
			t.Errorf("Canonical(%q)=%q != Canonical(%q)=%q; the cap can be defeated by the qualifier",
				pair[0], Canonical(pair[0]), pair[1], Canonical(pair[1]))
		}
	}
	if got := Canonical("Harry Potter - All Media Types"); got != "harry potter" {
		t.Errorf("Canonical = %q, want %q", got, "harry potter")
	}
}

func TestGroupKeyPicksTheFandomOutOfAFlatTagList(t *testing.T) {
	// The realistic shape: a blurb-flattened tag list where the fandom is one
	// entry among many freeforms.
	tags := []string{"dark", "villain", "harry potter - all media types", "m/m"}
	if got := GroupKey(tags); got != "harry potter" {
		t.Errorf("GroupKey = %q, want harry potter", got)
	}
}

// "" must mean "do not cap", not "group under unknown". If it meant unknown,
// every non-fandom work in the corpus would share one capped bucket and the
// monocrop would come back under a different label.
func TestGroupKeyIsEmptyWhenNoFandomIsPresent(t *testing.T) {
	for _, tags := range [][]string{
		{},
		{"dark", "explicit"},
		{"hermione granger", "hurt no comfort"},
	} {
		if got := GroupKey(tags); got != "" {
			t.Errorf("GroupKey(%v) = %q, want empty", tags, got)
		}
	}
}
