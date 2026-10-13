package web

// Tests for the pure helpers in handlers.go. The file is the largest
// untested surface in the repo (3,042 lines, no sibling _test.go), and the
// functions below are string->value parsers sitting directly on public query
// parameters — the same defect class as engine's ParseSeed, which accepted
// "12abc" as 12. Each case below names the behaviour it pins.

import (
	"strings"
	"testing"
)

func TestParseRatingAcceptsEverySpellingItDocuments(t *testing.T) {
	for _, in := range []string{"like", "love", "1", "true", "yes", "LIKE", " True "} {
		got, err := parseRating(in)
		if err != nil {
			t.Errorf("parseRating(%q) returned error %v; want true, nil", in, err)
			continue
		}
		if !got {
			t.Errorf("parseRating(%q) = false; want true", in)
		}
	}
	for _, in := range []string{"dislike", "0", "false", "no", "NO", " dislike "} {
		got, err := parseRating(in)
		if err != nil {
			t.Errorf("parseRating(%q) returned error %v; want false, nil", in, err)
			continue
		}
		if got {
			t.Errorf("parseRating(%q) = true; want false", in)
		}
	}
}

func TestParseRatingRejectsNeutralAndGarbage(t *testing.T) {
	// "neutral" and "" are deliberately errors, not a third state: recording
	// nothing is what the caller already does by not sending a rating.
	for _, in := range []string{"neutral", "", "  ", "NEUTRAL"} {
		if _, err := parseRating(in); err == nil {
			t.Errorf("parseRating(%q) = nil error; want an error naming why neutral records nothing", in)
		} else if !strings.Contains(err.Error(), "neutral") {
			t.Errorf("parseRating(%q) error = %q; want it to mention 'neutral'", in, err)
		}
	}
	for _, in := range []string{"maybe", "2", "-1", "likeish"} {
		_, err := parseRating(in)
		if err == nil {
			t.Errorf("parseRating(%q) = nil error; want it rejected", in)
			continue
		}
		if !strings.Contains(err.Error(), "not a rating") {
			t.Errorf("parseRating(%q) error = %q; want it to say 'not a rating'", in, err)
		}
	}
}

func TestParseWorkList(t *testing.T) {
	tests := []struct {
		in   string
		want []int64
	}{
		{"", nil},
		{"   ", nil},
		{"111", []int64{111}},
		{"111,222", []int64{111, 222}},
		// A stray comma is not an error; the empty part is skipped, so a
		// trailing comma from a copy-pasted list still works.
		{"111,222,", []int64{111, 222}},
		{" 111 , 222 ", []int64{111, 222}},
		{"111,,222", []int64{111, 222}},
	}
	for _, tc := range tests {
		got, err := parseWorkList(tc.in)
		if err != nil {
			t.Errorf("parseWorkList(%q) error = %v; want nil", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseWorkList(%q) = %v; want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseWorkList(%q) = %v; want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestParseWorkListRejectsNonPositiveAndNonNumeric(t *testing.T) {
	// "12abc" is the ParseSeed bug in this shape: ParseInt rejects it, and the
	// test exists so a future "be lenient" change has to argue with a red test.
	for _, in := range []string{"abc", "12abc", "0", "-5", "111,abc", "1.5", "111,0"} {
		if _, err := parseWorkList(in); err == nil {
			t.Errorf("parseWorkList(%q) = nil error; want it rejected", in)
		}
	}
	// Zero and negatives are rejected by name, not by accident of ParseInt.
	if _, err := parseWorkList("0"); err == nil || !strings.Contains(err.Error(), "not positive") {
		t.Errorf("parseWorkList(\"0\") error = %v; want it to say 'not positive'", err)
	}
	if _, err := parseWorkList("abc"); err == nil || !strings.Contains(err.Error(), "comma separated") {
		t.Errorf("parseWorkList(\"abc\") error = %v; want it to explain the list format", err)
	}
}

func TestSearchWorkID(t *testing.T) {
	tests := []struct {
		in       string
		wantID   int64
		wantFrom bool
	}{
		{"123", 123, false},
		{" 123 ", 123, false},
		{"/works/456", 456, true},
		{"https://archiveofourown.org/works/456", 456, true},
		{"https://example.invalid/some/path/works/789?view_adult=true", 789, true},
		// Negative cases: these must NOT resolve to an id.
		{"", 0, false},
		{"12abc", 0, false},
		{"abc", 0, false},
		// The regexes cap at 12 digits; 13 must not be silently truncated.
		{"1234567890123", 0, false},
		{"-123", 0, false},
		{"1.5", 0, false},
	}
	for _, tc := range tests {
		id, fromURL := searchWorkID(tc.in)
		if id != tc.wantID || fromURL != tc.wantFrom {
			t.Errorf("searchWorkID(%q) = (%d, %v); want (%d, %v)",
				tc.in, id, fromURL, tc.wantID, tc.wantFrom)
		}
	}
}

func TestSearchTokensCapsTheFieldCount(t *testing.T) {
	if got := searchTokens("a b c"); len(got) != 3 {
		t.Errorf("searchTokens(\"a b c\") = %v; want 3 fields", got)
	}
	in := "w1 w2 w3 w4 w5 w6 w7 w8 w9 w10"
	if got := searchTokens(in); len(got) != 8 {
		t.Errorf("searchTokens(10 fields) returned %d fields; want 8", len(got))
	}
	if got := searchTokens("   "); len(got) != 0 {
		t.Errorf("searchTokens(whitespace) = %v; want empty", got)
	}
}

func TestJoinIntsCapsAndCountsTheRemainder(t *testing.T) {
	if got := joinInts(nil); got != "" {
		t.Errorf("joinInts(nil) = %q; want empty", got)
	}
	if got := joinInts([]int64{1, 2, 3}); got != "1, 2, 3" {
		t.Errorf("joinInts([1 2 3]) = %q; want \"1, 2, 3\"", got)
	}
	// The cap exists so an error message listing 400 ids stays readable; the
	// remainder must still be counted, or the message understates the problem.
	var ids []int64
	for i := int64(1); i <= 15; i++ {
		ids = append(ids, i)
	}
	got := joinInts(ids)
	if !strings.Contains(got, "... and 3 more") {
		t.Errorf("joinInts(15 ids) = %q; want it to end with '... and 3 more'", got)
	}
	if !strings.HasPrefix(got, "1, ") {
		t.Errorf("joinInts(15 ids) = %q; want it to start with the first id", got)
	}
}

func TestShortfallReasonPicksTheRightExplanation(t *testing.T) {
	// Enough results: nothing to explain.
	if got := shortfallReason(10, 10, 0, ""); got != "" {
		t.Errorf("shortfallReason(10,10,...) = %q; want empty", got)
	}
	if got := shortfallReason(10, 11, 0, ""); got != "" {
		t.Errorf("shortfallReason(10,11,...) = %q; want empty", got)
	}
	// A cap that bound is the explanation, and the cap must be named.
	got := shortfallReason(10, 3, 5, "fandom")
	if !strings.Contains(got, "cap of 5 per fandom") {
		t.Errorf("shortfallReason(10,3,5,fandom) = %q; want it to name the cap", got)
	}
	// capN set but no groupBy: not the cap's fault, fall through to the
	// corpus explanation rather than blaming a cap that was not applied.
	got = shortfallReason(10, 3, 5, "")
	if strings.Contains(got, "cap of") {
		t.Errorf("shortfallReason with no groupBy = %q; want the corpus explanation", got)
	}
	// The default explanation states both numbers.
	got = shortfallReason(10, 3, 0, "")
	if !strings.Contains(got, "3 of 10") {
		t.Errorf("shortfallReason(10,3,0,\"\") = %q; want it to state '3 of 10'", got)
	}
}

func TestTaggedCountCountsOnlyTaggedRows(t *testing.T) {
	page := &TagPage{Works: []WorkHit{
		{ID: 1, HasTag: true},
		{ID: 2, HasTag: false},
		{ID: 3, HasTag: true},
	}}
	if got := taggedCount(page); got != 2 {
		t.Errorf("taggedCount = %d; want 2", got)
	}
	if got := taggedCount(&TagPage{}); got != 0 {
		t.Errorf("taggedCount(empty) = %d; want 0", got)
	}
}

func TestAbsf(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{-1.5, 1.5}, {1.5, 1.5}, {0, 0}, {-0.0001, 0.0001},
	} {
		if got := absf(tc.in); got != tc.want {
			t.Errorf("absf(%v) = %v; want %v", tc.in, got, tc.want)
		}
	}
}
