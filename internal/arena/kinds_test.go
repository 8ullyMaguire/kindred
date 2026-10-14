// SPDX-License-Identifier: AGPL-3.0-only

package arena

import "testing"

func TestParseKindDefaultsToWork(t *testing.T) {
	// The point of the default: every caller that predates `?kind=` keeps its
	// behaviour, and an unknown kind is a downgrade rather than a failure.
	for _, in := range []string{"", "   ", "nonsense", "ao3_", "WORK", "work", "AO3_Work"} {
		got := ParseKind(in)
		if in == "WORK" || in == "work" || in == "AO3_Work" {
			if got != KindWork {
				t.Errorf("ParseKind(%q) = %q, want %q", in, got, KindWork)
			}
			continue
		}
		if got != KindWork {
			t.Errorf("ParseKind(%q) = %q, want the %q default", in, got, KindWork)
		}
	}
}

func TestParseKindRoundTripsEveryAllowlistedKind(t *testing.T) {
	for _, k := range Kinds {
		if got := ParseKind(string(k)); got != k {
			t.Errorf("ParseKind(%q) = %q, want %q", string(k), got, k)
		}
		if got := ParseKind("  " + string(k) + "  "); got != k {
			t.Errorf("ParseKind with padding = %q, want %q", got, k)
		}
	}
}

func TestZeroKindIsNotValid(t *testing.T) {
	// An unset kind must not pass a validity check by accident: the empty
	// EntityKind is not on the allowlist, so a missing default is a bug that
	// shows up rather than a kind that behaves like a work.
	var zero EntityKind
	if zero.Valid() {
		t.Fatal("the zero EntityKind must not be valid")
	}
}

func TestTagTypeOnlyForTagKinds(t *testing.T) {
	want := map[EntityKind]string{
		KindTag:          "freeforms",
		KindCharacter:    "characters",
		KindRelationship: "relationships",
		KindFandom:       "fandoms",
	}
	for _, k := range Kinds {
		got, ok := k.TagType()
		expected, isTag := want[k]
		if ok != isTag {
			t.Errorf("%s.TagType() ok = %v, want %v", k, ok, isTag)
		}
		if got != expected {
			t.Errorf("%s.TagType() = %q, want %q", k, got, expected)
		}
		if k.IsTag() != isTag {
			t.Errorf("%s.IsTag() = %v, want %v", k, k.IsTag(), isTag)
		}
	}
}

func TestAuthorEntityIDStability(t *testing.T) {
	// The plan's stated cases, plus the ones the corpus actually contains
	// (measured: `-Roulette- (Roulette_CV)`, `-  Scrap - Paper - (…)`).
	same := [][2]string{
		{"-Roulette- (Roulette_CV)", "-Roulette-"},
		{"-  Scrap - Paper - (multivers4l_m0r0n)", "- Scrap - Paper -"},
		{"10MinutesADay (moucche)", "10minutesaday"},
		{"  Name  ", "name"},
	}
	for _, pair := range same {
		if AuthorEntityID(pair[0]) != AuthorEntityID(pair[1]) {
			t.Errorf("AuthorEntityID(%q) != AuthorEntityID(%q): the pseud suffix or case must not change the id", pair[0], pair[1])
		}
	}

	// Punctuation is identity: `-Roulette-` and `Roulette` are different
	// accounts, and collapsing them would merge two people into one arena
	// entity.
	differ := [][2]string{
		{"-Roulette-", "Roulette"},
		{"ab", "abx"},
		{"Boku", "Buku"},
	}
	for _, pair := range differ {
		if AuthorEntityID(pair[0]) == AuthorEntityID(pair[1]) {
			t.Errorf("AuthorEntityID(%q) == AuthorEntityID(%q), want different ids", pair[0], pair[1])
		}
	}
}

func TestNameWithoutPseudKeepsBracketsThatAreTheName(t *testing.T) {
	cases := map[string]string{
		"(orphaned_account)":         "(orphaned_account)", // the whole name is bracketed
		"Name (pseud)":               "Name",
		"Name (pseud) (second)":      "Name (pseud)", // only ONE suffix is dropped
		"Name (not pseud":            "Name (not pseud",
		"Plain Name":                 "Plain Name",
		"  -Roulette- (Roulette_CV)": "-Roulette-",
	}
	for in, want := range cases {
		if got := NameWithoutPseud(in); got != want {
			t.Errorf("NameWithoutPseud(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitAuthorsReadsTheCrawlersFormat(t *testing.T) {
	// The crawler joins with ", " (internal/crawl/parse.go:142), so that is
	// the separator. Reading the whole column as one author would mint an
	// entity that does not exist the first time a multi-author work is
	// crawled — and the corpus held exactly one such row when this was
	// written, which is why the rule is pinned by a test rather than by
	// observation.
	got := SplitAuthors("Alpha (a), Beta (b)")
	if len(got) != 2 || got[0] != "Alpha (a)" || got[1] != "Beta (b)" {
		t.Fatalf("SplitAuthors = %#v, want two bylines", got)
	}
	if got := SplitAuthors("Solo (s)"); len(got) != 1 || got[0] != "Solo (s)" {
		t.Fatalf("SplitAuthors(single) = %#v", got)
	}
	if got := SplitAuthors("  "); len(got) != 0 {
		t.Fatalf("SplitAuthors(blank) = %#v, want none", got)
	}
	// Two authors on one work are two entities, and each keeps its own id.
	if AuthorEntityID("Alpha (a)") == AuthorEntityID("Beta (b)") {
		t.Fatal("distinct bylines must not share an id")
	}
}

func TestLabelsAreStableAndDistinct(t *testing.T) {
	seen := map[string]EntityKind{}
	for _, k := range Kinds {
		l := k.Label()
		if l == "" || l == string(k) {
			t.Errorf("%s has no label", k)
		}
		if prev, dup := seen[l]; dup {
			t.Errorf("%s and %s share the label %q", prev, k, l)
		}
		seen[l] = k
	}
}
