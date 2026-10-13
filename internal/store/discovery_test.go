package store

// The walk checkpoint's contract. The interesting cases are the ones the type
// exists for: a fresh mirror, a page walked twice (which here is NOT a no-op,
// unlike crawl_queue), and clearing.

import (
	"context"
	"testing"
)

func TestDiscoveryPagesOnAFreshMirrorIsEmptyNotAnError(t *testing.T) {
	// SUM and MAX over zero rows are NULL. A new deployment is the state this
	// type meets first, and it must read as "nothing walked" rather than as a
	// failed query.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	pages, err := s.DiscoveryPages(ctx)
	if err != nil {
		t.Fatalf("DiscoveryPages on a fresh store: %v", err)
	}
	if len(pages) != 0 {
		t.Errorf("pages = %v; want empty", pages)
	}

	st, err := s.DiscoveryStats(ctx)
	if err != nil {
		t.Fatalf("DiscoveryStats on a fresh store: %v", err)
	}
	if st.Pages != 0 || st.WorksFound != 0 || st.LastFetched != "" {
		t.Errorf("stats = %+v; want all zero with an empty LastFetched", st)
	}
}

func TestDiscoveryPagesRoundTripsEachPageDistinctly(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const a = "https://archiveofourown.org/tags/A/works"
	const b = "https://archiveofourown.org/tags/A/works?page=2"
	for _, u := range []string{a, b} {
		if err := s.MarkDiscoveryPage(ctx, u, 20); err != nil {
			t.Fatal(err)
		}
	}

	pages, err := s.DiscoveryPages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || !pages[a] || !pages[b] {
		t.Errorf("pages = %v; want exactly %s and %s", pages, a, b)
	}
	// Page 1 and page 2 of the SAME listing are different pages. A key that
	// dropped the query string would treat the whole tag as one page and skip
	// every page after the first.
	if pages["https://archiveofourown.org/tags/A/works?page=3"] {
		t.Error("a page never marked reads as walked")
	}
}

func TestMarkDiscoveryPageTwiceUpdatesTheCount(t *testing.T) {
	// The opposite of crawl_queue's upsert, and the reason is the semantic
	// difference: a re-crawled work has nothing new to say, a re-walked page
	// does. Leaving the stale count would make it a number that looks like a
	// measurement and is not one.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const u = "https://archiveofourown.org/tags/A/works"
	if err := s.MarkDiscoveryPage(ctx, u, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDiscoveryPage(ctx, u, 17); err != nil {
		t.Fatal(err)
	}

	st, err := s.DiscoveryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pages != 1 {
		t.Errorf("Pages = %d; want 1 — re-marking one page must not add a row", st.Pages)
	}
	if st.WorksFound != 17 {
		t.Errorf("WorksFound = %d; want 17, the later count", st.WorksFound)
	}
}

func TestMarkDiscoveryPageIgnoresAnEmptyURL(t *testing.T) {
	// A page whose URL resolved to nothing is not a page. Recording it would
	// put a row in the checkpoint that no walk can ever match, and the count on
	// a status page would be wrong forever.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, u := range []string{"", "   ", "\n"} {
		if err := s.MarkDiscoveryPage(ctx, u, 3); err != nil {
			t.Fatalf("MarkDiscoveryPage(%q): %v", u, err)
		}
	}
	st, err := s.DiscoveryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pages != 0 {
		t.Errorf("Pages = %d; want 0 — a blank URL must not be recorded", st.Pages)
	}
}

func TestMarkDiscoveryPageClampsANegativeCount(t *testing.T) {
	// A negative count cannot come from the walk, but it would silently reduce
	// WorksFound and make the total wrong in a way nothing reports.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.MarkDiscoveryPage(ctx, "https://a/tags/A/works", -4); err != nil {
		t.Fatal(err)
	}
	st, err := s.DiscoveryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.WorksFound != 0 {
		t.Errorf("WorksFound = %d; want 0 after a negative count", st.WorksFound)
	}
}

func TestDiscoveryStatsFieldsAreNotTransposed(t *testing.T) {
	// Three pages with distinct counts, so a swapped Pages/WorksFound pair or a
	// COUNT/SUM mix-up fails instead of coincidentally matching. Distinct values
	// are the whole point: an aggregate-only assertion passes over a
	// transposition when the numbers happen to be equal.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for u, n := range map[string]int{
		"https://a/tags/A/works":        5,
		"https://a/tags/A/works?page=2": 7,
		"https://a/tags/B/works":        11,
	} {
		if err := s.MarkDiscoveryPage(ctx, u, n); err != nil {
			t.Fatal(err)
		}
	}

	st, err := s.DiscoveryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pages != 3 {
		t.Errorf("Pages = %d; want 3", st.Pages)
	}
	if st.WorksFound != 23 {
		t.Errorf("WorksFound = %d; want 23 (5+7+11)", st.WorksFound)
	}
	if st.LastFetched == "" {
		t.Error("LastFetched is empty with three pages recorded")
	}
}

func TestForgetDiscoveryPagesClearsAndCounts(t *testing.T) {
	// The escape hatch that makes a default-on checkpoint safe: a busy tag's
	// page 1 changes weekly, so a permanent memory would make the walk miss the
	// only page that changed.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, u := range []string{"https://a/tags/A/works", "https://a/tags/A/works?page=2"} {
		if err := s.MarkDiscoveryPage(ctx, u, 4); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.ForgetDiscoveryPages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("cleared %d rows; want 2", n)
	}
	pages, err := s.DiscoveryPages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 0 {
		t.Errorf("pages = %v after clearing; want empty", pages)
	}
	// Idempotent: clearing an empty checkpoint is not an error.
	if n, err := s.ForgetDiscoveryPages(ctx); err != nil {
		t.Fatalf("second clear: %v", err)
	} else if n != 0 {
		t.Errorf("second clear reported %d rows; want 0", n)
	}
}
