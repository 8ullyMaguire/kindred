package web

// W3: the remaining pure helpers — weightRows, recommendFilterRows,
// parseBoundedInt, likeEscape, atoiDefault. No store, no corpus: each is a
// function of its arguments, which is exactly why they are cheap evidence and
// why a bug in one is a wrong page rather than a crash.

import (
	"net/url"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/engine"
)

func TestWeightRowsSortsHeaviestFirstAndDropsZero(t *testing.T) {
	got := weightRows(map[string]float64{
		"slow burn": 0.4,
		"fluff":     0.9,
		"angst":     0.0, // dropped: a row of 0.00 teaches nothing
		"hurt":      0.4,
	})

	if len(got) != 3 {
		t.Fatalf("weightRows returned %d rows (%v); want 3 — the zero weight must be dropped", len(got), got)
	}
	// Descending by weight, and ties broken by name ascending. The tie-break
	// is the load-bearing half: map iteration order is random in Go, so
	// without it the same input renders a different table every request.
	want := []string{"fluff", "hurt", "slow burn"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("row %d = %q (%.2f); want %q — rows must be deterministic", i, got[i].Name, got[i].Weight, w)
		}
	}
}

func TestWeightRowsOnEmptyAndAllZeroInputs(t *testing.T) {
	if got := weightRows(nil); len(got) != 0 {
		t.Errorf("weightRows(nil) = %v; want empty", got)
	}
	if got := weightRows(map[string]float64{"a": 0, "b": 0}); len(got) != 0 {
		t.Errorf("weightRows(all zero) = %v; want empty", got)
	}
}

func TestWeightRowsIsStableAcrossRepeatedCalls(t *testing.T) {
	// Same input, same output — the property the tie-break exists to provide.
	in := map[string]float64{"b": 0.5, "a": 0.5, "c": 0.5, "d": 0.5}
	first := weightRows(in)
	for i := 0; i < 20; i++ {
		again := weightRows(in)
		for j := range first {
			if first[j].Name != again[j].Name {
				t.Fatalf("call %d differs at row %d: %q vs %q", i, j, first[j].Name, again[j].Name)
			}
		}
	}
}

func TestParseBoundedIntRefusesRatherThanClamps(t *testing.T) {
	// The comment on parseBoundedInt states the contract: out of range is a
	// 400, not a clamp, "because a silently clamped cap is a cap the reader
	// did not ask for". A clamp here would be invisible in the UI.
	if _, err := parseBoundedInt("10000", 0, 100, 20); err == nil {
		t.Error("parseBoundedInt(10000) accepted a value above hi; want an error")
	}
	if _, err := parseBoundedInt("-1", 0, 100, 20); err == nil {
		t.Error("parseBoundedInt(-1) accepted a value below lo; want an error")
	}
}

func TestParseBoundedIntAcceptsTheBoundsAndTheDefault(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 20}, {"   ", 20}, {"0", 0}, {"100", 100}, {"50", 50}, {" 7 ", 7},
	} {
		got, err := parseBoundedInt(tc.in, 0, 100, 20)
		if err != nil {
			t.Errorf("parseBoundedInt(%q) error = %v; want nil", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBoundedInt(%q) = %d; want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseBoundedIntRejectsNonNumbers(t *testing.T) {
	for _, in := range []string{"abc", "1.5", "12abc", "1e3"} {
		if _, err := parseBoundedInt(in, 0, 100, 20); err == nil {
			t.Errorf("parseBoundedInt(%q) accepted a non-number; want an error", in)
		}
	}
}

func TestLikeEscapeNeutralisesTheLikeWildcards(t *testing.T) {
	// Without this, searching "100%" returns every tag and "a_b" matches
	// "axb" — a wrong answer that looks plausible, which is why it was never
	// reported. The escape character itself must be escaped first, or "a\%b"
	// becomes ambiguous.
	for _, tc := range []struct{ in, want string }{
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`back\slash`, `back\\slash`},
		{`a%\_b`, `a\%\\\_b`},
		{"plain words", "plain words"},
		{"", ""},
	} {
		if got := likeEscape(tc.in); got != tc.want {
			t.Errorf("likeEscape(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// NOTE: atoiDefault is NOT tested here — render_test.go's TestAtoiDefault
// already covers it, including the " 12"/"12 " whitespace cases this file would
// otherwise have missed. Duplicating it would have added a second weaker test
// for the same function.

func TestRecommendFilterRowsNormalisesTheCompleteValue(t *testing.T) {
	// Every spelling that means "finished" collapses to "complete"; "all" and
	// "any" collapse to "any"; anything unrecognised falls back to the first
	// legal option rather than emitting a value the select cannot show.
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", ""},
		{"any", "any"},
		{"ALL", "any"},
		{"complete", "complete"},
		{"completed", "complete"},
		{"true", "complete"},
		{"1", "complete"},
		{"yes", "complete"},
		{" COMPLETE ", "complete"},
		{"nonsense", "in-progress"},
	} {
		rows := recommendFilterRows(url.Values{"complete": {tc.in}}, engine.Filter{})
		var got string
		var seen bool
		for _, r := range rows {
			if r.Name == "complete" {
				got, seen = r.Value, true
			}
		}
		if !seen {
			t.Fatal("recommendFilterRows returned no 'complete' row")
		}
		if got != tc.want {
			t.Errorf("complete=%q normalised to %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestRecommendFilterRowsSelectsExactlyTheChosenOption(t *testing.T) {
	rows := recommendFilterRows(url.Values{"complete": {"any"}}, engine.Filter{})
	for _, r := range rows {
		if r.Name != "complete" {
			continue
		}
		selected := 0
		for _, o := range r.Values {
			if o.Selected {
				selected++
				if o.Value != "any" {
					t.Errorf("selected option = %q; want \"any\"", o.Value)
				}
			}
		}
		if selected != 1 {
			t.Errorf("%d options selected; want exactly 1", selected)
		}
	}
}

func TestRecommendFilterRowsJoinsCsvAndEchoesNumbers(t *testing.T) {
	q := url.Values{"min_words": {"5000"}, "max_words": {""}, "min_kudos": {"10"}}
	rows := recommendFilterRows(q, engine.Filter{
		Ratings:   []string{"General Audiences", "Explicit"},
		Languages: []string{"English"},
	})

	byName := map[string]FilterRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if got := byName["min_words"].Value; got != "5000" {
		t.Errorf("min_words Value = %q; want the raw query value echoed", got)
	}
	if got := byName["max_words"].Value; got != "" {
		t.Errorf("max_words Value = %q; want empty when unset", got)
	}
	if got := byName["rating"].Value; got != "General Audiences, Explicit" {
		t.Errorf("rating Value = %q; want the list joined with \", \"", got)
	}
	if got := byName["lang"].Value; got != "English" {
		t.Errorf("lang Value = %q; want \"English\"", got)
	}
	// The displayed order is the template's, and the reader fills it by name.
	if want := []string{"min_words", "max_words", "min_kudos", "complete", "rating", "lang"}; len(rows) != len(want) {
		t.Errorf("got %d rows; want %d (%v)", len(rows), len(want), want)
	}
}
