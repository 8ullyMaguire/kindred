package web

import "testing"

// TestSafeReturnToRejectsOffsiteTargets pins the open-redirect guard on
// POST /seen.
//
// The form posts back to wherever the reader was reading. That value comes
// from the request, so a page crafted to POST here with
// return_to=https://evil.example/phish produces a 303 to that host on the
// back of an action the reader believes is "I have read this" — a form whose
// label and destination disagree, which is the whole of a phishing primitive.
//
// The `//` case matters and is easy to miss: `strings.HasPrefix(v, "/")`
// accepts it, and `//evil.example` is a protocol-relative URL that browsers
// resolve to https://evil.example.
func TestSafeReturnToRejectsOffsiteTargets(t *testing.T) {
	for _, v := range []string{
		"https://evil.example/phish",
		"http://evil.example",
		"//evil.example/phish",
		"///evil.example",
		"evil.example",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"",
		" ",
		"\t\n",
		"\\evil.example",
		"/\\evil.example",
	} {
		if got := safeReturnTo(v); got != "/recommend" {
			t.Errorf("safeReturnTo(%q) = %q, want the safe default /recommend", v, got)
		}
	}
}

// TestSafeReturnToAcceptsLocalPaths: the guard must not be so broad that it
// breaks the feature. A reader marking a work read from a tag-seeded list
// belongs back on that list.
func TestSafeReturnToAcceptsLocalPaths(t *testing.T) {
	for _, v := range []string{
		"/recommend",
		"/recommend?seed=ao3_work:26523892&n=20",
		"/tag/1",
		"/arena",
		"/",
		"/path/with%20encoding",
	} {
		if got := safeReturnTo(v); got != v {
			t.Errorf("safeReturnTo(%q) = %q, want it echoed unchanged", v, got)
		}
	}
}
