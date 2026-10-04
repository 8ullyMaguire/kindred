package web

// Tests for internal/web's pure helpers.
//
// The package had no test file at all -- 2,029 lines, the largest untested
// surface in the repo, and the one clause docs/goal-check.py was red on.
// internal/api covers the *composite* (web mounted in front of the API mux), so
// nothing exercised these directly.
//
// What these tests are for is not coverage for its own sake. Four of these
// functions decide what a reader sees, and two of them were wrong in ways that
// reading the code does not show:

//   - shorten counted every '.' in the string, so "Dr. Smith has 3.5k words.
//     Then more." rendered as "Dr." -- a summary reduced to a title, on four
//     pages. It also indexed by byte, so a period after a multibyte rune could
//     split the rune, while trim() 60 lines away in arena.go documents the
//     hazard it avoids ("shortens text on a RUNE boundary, not a byte one").
//   - percent and commas and stat feed every number on the page.
//
// So each test names the input and the exact expected output, and the ones
// with a discovered bug say what the bug was in the comment, because "why this
// assertion exists" is the part a future reader cannot reconstruct.

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// ------------------------------------------------------------------ shorten

func TestShortenKeepsWholeSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		// The ordinary cases, which the old implementation also passed.
		{"one sentence of two", "First one. Second one.", 1, "First one."},
		{"first of three", "First one. Second one. Third one.", 1, "First one."},
		{"two of three", "First one. Second one. Third one.", 2, "First one. Second one."},
		{"fewer sentences than max", "Only one.", 3, "Only one."},

		// The abbreviations that broke it. Every one of these returned the
		// abbreviation alone, so the reader saw "Dr." where the summary began.
		{"title abbreviation", "Dr. Smith has 3.5k words. Then more.", 1, "Dr. Smith has 3.5k words."},
		{"honorific", "Mr. Smith wrote it. Then more text.", 1, "Mr. Smith wrote it."},
		{"initial sequence", "A. B. C. wrote this. And that.", 1, "A. B. C. wrote this."},
		{"decimal number", "He runs 12.5km daily. Then rests.", 1, "He runs 12.5km daily."},
		{"url", "See https://example.com/a. It works. Done.", 1, "See https://example.com/a."},
		{"version number", "Release 1.2.3 fixed it. Nothing else changed.", 1, "Release 1.2.3 fixed it."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shorten(c.in, c.max); got != c.want {
				t.Errorf("shorten(%q, %d)\n got  %q\n want %q", c.in, c.max, got, c.want)
			}
		})
	}
}

func TestShortenDoesNotSplitARune(t *testing.T) {
	// "Café naïve." is 11 bytes and 10 runes. Counting bytes and slicing at the
	// period index is safe here only by luck of where the period lands; the
	// ellipsis and multibyte cases below are where it breaks.
	// The CJK case is absent on purpose: Japanese writes the full stop as
	// U+3002, not '.', so shorten has no boundary to find and returns the whole
	// string. That is correct for this function and is not a rune bug.
	for _, in := range []string{
		"Café naïve. Second sentence here.",
		"Nice 🎉 party. Second sentence here.",
		"Ω-notation intro. Second sentence here.",
		"日本語の要約です. Second sentence here.",
	} {
		got := shorten(in, 1)
		if !isValidUTF8(got) {
			t.Errorf("shorten(%q, 1) = %q, which is not valid UTF-8: the cut landed inside a rune", in, got)
		}
		if strings.Contains(in, ".") && !strings.HasSuffix(got, ".") {
			t.Errorf("shorten(%q, 1) = %q, want it to end at a sentence boundary", in, got)
		}
	}
}

func TestShortenDegenerateInputs(t *testing.T) {
	// max <= 0 means "no limit", not "cut at zero": these pages pass literal 1,
	// 2 and 3, but the guard exists so a caller cannot get an empty page.
	for _, max := range []int{0, -1} {
		const in = "One. Two. Three."
		if got := shorten(in, max); got != in {
			t.Errorf("shorten(%q, %d) = %q, want the whole string: max<=0 is not a limit", in, max, got)
		}
	}
	if got := shorten("   ", 1); got != "" {
		t.Errorf("shorten(%q, 1) = %q, want empty", "   ", got)
	}
	if got := shorten("", 1); got != "" {
		t.Errorf("shorten(%q, 1) = %q, want empty", "", got)
	}
	// No period at all: the whole string is the answer, never a truncation at len(max).
	const unpunctuated = "a summary with no full stop"
	if got := shorten(unpunctuated, 1); got != unpunctuated {
		t.Errorf("shorten(%q, 1) = %q, want it unchanged", unpunctuated, got)
	}
}

func TestShortenCollapsesWhitespace(t *testing.T) {
	const in = "  First   one.\n\n  Second one.  "
	const want = "First one."
	if got := shorten(in, 1); got != want {
		t.Errorf("shorten(%q, 1) = %q, want %q", in, got, want)
	}
}

// ------------------------------------------------------------------- commas

func TestCommas(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{2384712, "2,384,712"},
		{1000000, "1,000,000"},
		// Negative: the sign must survive, and the digit grouping must not
		// count the sign as a digit.
		{-1, "-1"},
		{-999, "-999"},
		{-1000, "-1,000"},
		{-2384712, "-2,384,712"},
	}
	for _, c := range cases {
		if got := commas(c.in); got != c.want {
			t.Errorf("commas(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ------------------------------------------------------------------- percent

func TestPercent(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.0%"},
		{1, "100.0%"},
		{0.5, "50.0%"},
		{0.1464, "14.6%"}, // the doc's example: round1 keeps it honest
		{1.0 / 3.0, "33.3%"},
	}
	for _, c := range cases {
		if got := percent(c.in); got != c.want {
			t.Errorf("percent(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------- stat

// stat must distinguish "no such key" from "zero". A page that renders 0 for a
// stat the corpus does not have is making a claim about data it does not have,
// which is what dash is for.
func TestStatDistinguishesMissingFromZero(t *testing.T) {
	m := map[string]float64{"kudos": 0, "hits": 12}

	if got := stat(nil, "kudos"); got != dash {
		t.Errorf("stat(nil, kudos) = %q, want the dash", got)
	}
	if got := stat(m, "absent"); got != dash {
		t.Errorf("stat(m, absent) = %q, want the dash", got)
	}
	if got := stat(m, "kudos"); got != "0" {
		t.Errorf("stat(m, kudos) = %q, want \"0\": a real zero is not a missing stat", got)
	}
	if got := stat(m, "hits"); got != "12" {
		t.Errorf("stat(m, hits) = %q, want \"12\"", got)
	}
}

func TestStatGroupsOnlyCountedStats(t *testing.T) {
	// The count-like keys get thousands separators; the rest stay plain. A
	// rating of 1464.0583 must not print as "1,464.0583".
	m := map[string]float64{
		"bookmarks":  2384712,
		"word_count": 12345,
		"score":      1464.0583,
	}
	for key, want := range map[string]string{
		"bookmarks":  "2,384,712",
		"word_count": "12,345",
		"score":      "1464.0583",
	} {
		if got := stat(m, key); got != want {
			t.Errorf("stat(m, %q) = %q, want %q", key, got, want)
		}
	}
}

// ----------------------------------------------------------------- round1

func TestRound1(t *testing.T) {
	if got := round1(1464.0583); got != 1464.1 {
		t.Errorf("round1(1464.0583) = %v, want 1464.1: the measurement has no more precision than that", got)
	}
	if got := round1(0); got != 0 {
		t.Errorf("round1(0) = %v, want 0", got)
	}
}

// ------------------------------------------------------------------- trim

// trim cuts to a byte budget, and arena.go's own comment says it cuts on a rune
// boundary. shorten did not, which is why both are here.
func TestTrimRuneBoundaryAndBudget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"ascii within budget", "short", 20},
		{"ascii over budget", "a much longer summary than fits", 20},
		{"multibyte within budget", "日本語の要約", 40},
		{"multibyte over budget", "日本語の要約です。这是二番目です。", 20},
		{"emoji over budget", "party 🎉🎉🎉🎉🎉🎉 time", 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := trim(c.in, c.max)
			if !isValidUTF8(got) {
				t.Errorf("trim(%q, %d) = %q, not valid UTF-8", c.in, c.max, got)
			}
			if len(got) > c.max {
				t.Errorf("trim(%q, %d) = %q, which is %d bytes: over budget", c.in, c.max, got, len(got))
			}
		})
	}
}

// -------------------------------------------------------------- atoiDefault

func TestAtoiDefault(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"42", 7, 42},
		{"0", 7, 0},
		{"", 7, 7},
		{"abc", 7, 7},
		{"-3", 7, -3},
		// Atoi accepts neither leading nor trailing space. Callers pass
		// URL query values, which can carry either, so this is the default
		// path rather than an error path -- pinned because it looks like it
		// should parse.
		{" 12", 7, 7},
		{"12 ", 7, 7},
		{"+12", 7, 12},                 // an explicit sign is accepted
		{"99999999999999999999", 7, 7}, // overflow is a parse error, not a clamp
	}
	for _, c := range cases {
		if got := atoiDefault(c.in, c.def); got != c.want {
			t.Errorf("atoiDefault(%q, %d) = %d, want %d", c.in, c.def, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- clampInt

func TestClampInt(t *testing.T) {
	cases := []struct {
		n, lo, hi int
		want      int
	}{
		{5, 1, 10, 5},   // inside
		{0, 1, 10, 1},   // below
		{11, 1, 10, 10}, // above
		{1, 1, 10, 1},   // on the low bound
		{10, 1, 10, 10}, // on the high bound
	}
	for _, c := range cases {
		if got := clampInt(c.n, c.lo, c.hi); got != c.want {
			t.Errorf("clampInt(%d, %d, %d) = %d, want %d", c.n, c.lo, c.hi, got, c.want)
		}
	}
	// An inverted range must not panic and must not silently return the input.
	if got := clampInt(5, 10, 1); got == 5 {
		t.Errorf("clampInt(5, 10, 1) = %d: an inverted range returned the input unchanged", got)
	}
}

// --------------------------------------------------------- CandidateTags

func TestCandidateTags(t *testing.T) {
	c := rank.Candidate{
		TagNames: []string{"science fiction", "slow burn"},
		TagIDs:   []int32{101, 202},
	}
	got := CandidateTags(c)
	if len(got) != 2 {
		t.Fatalf("CandidateTags returned %d hits, want 2", len(got))
	}
	if got[0].Name != "science fiction" || got[0].ID != 101 {
		t.Errorf("hit 0 = %+v, want {science fiction 101}", got[0])
	}
	if got[1].Name != "slow burn" || got[1].ID != 202 {
		t.Errorf("hit 1 = %+v, want {slow burn 202}", got[1])
	}
}

func TestCandidateTagsHandlesMissingIDs(t *testing.T) {
	// engine.go:608-622 builds TagNames and TagIDs in the same loop, so in
	// production they always match. This pins what happens if a caller does not,
	// because the guard exists and silently returning id 0 would link to whatever
	// tag happens to be id 0.
	got := CandidateTags(rank.Candidate{TagNames: []string{"lonely"}})
	if len(got) != 1 {
		t.Fatalf("got %d hits, want 1", len(got))
	}
	if got[0].Name != "lonely" {
		t.Errorf("name = %q, want \"lonely\"", got[0].Name)
	}
	if got[0].ID != 0 {
		t.Errorf("id = %d, want 0: no id was supplied, so there is nothing to link to", got[0].ID)
	}
}

func TestCandidateTagsEmpty(t *testing.T) {
	// Must be non-nil so a template ranging over it does not print "null".
	got := CandidateTags(rank.Candidate{})
	if got == nil {
		t.Fatal("CandidateTags returned nil; templates range over this and would print null")
	}
	if len(got) != 0 {
		t.Errorf("got %d hits, want 0", len(got))
	}
}

// --------------------------------------------------------------- rendering

// The render layer must not write a partial page: render() buffers precisely so
// that a template failing halfway yields a 500 rather than 200 with a truncated
// page. This pins the error half of that contract, which nothing covered.
func TestRenderUnknownTemplateIsAnError(t *testing.T) {
	var buf bytes.Buffer
	err := Render(&buf, "no-such-page.html", nil)
	if err == nil {
		t.Fatal("Render with an unknown template returned nil error; a typo in a page name would serve a blank 200")
	}
	if !strings.Contains(err.Error(), "no-such-page.html") {
		t.Errorf("error %q does not name the template, so the caller cannot tell which page failed", err)
	}
}

// Every page named in the init() list must exist as an asset. The map is built
// with template.Must, so a missing file panics at process start rather than at
// request time -- which means this clause is really a guard against someone
// adding a name to that list without adding the file, and the panic message
// being the only feedback.
func TestEveryPageInTheMapHasATemplate(t *testing.T) {
	for name := range pages {
		if pages[name] == nil {
			t.Errorf("pages[%q] is nil", name)
		}
	}
	// The count is a floor, not an exact figure: it fails if someone deletes a
	// page wholesale, which is the change this clause is watching for. 12 is
	// layout.html plus the 11 pages listed in init() -- recounted from
	// render.go, not assumed.
	if len(pages) < 12 {
		t.Errorf("pages has %d entries, want at least 12 (layout + 11 pages)", len(pages))
	}
	if _, ok := pages["layout.html"]; !ok {
		t.Error("layout.html is not in pages; every ExecuteTemplate call targets \"layout\"")
	}
}

func isValidUTF8(s string) bool { return utf8.ValidString(s) }
