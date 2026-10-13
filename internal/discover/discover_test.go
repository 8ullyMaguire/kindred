package discover

// Tests for the BFS. Every one runs with no network: Fetcher is an interface and
// the fake below serves a fixed map of pages, which is also why the delay and
// retry live in crawl.Crawler.FetchOne rather than here — the politeness policy
// is the one thing this package must NOT be able to get wrong in a test and
// right in production.
//
// The fake records what was fetched, because "how many pages did the walk
// spend" is a first-class assertion here: at AO3's 30-second crawl delay a
// wasted fetch is half a minute of an operator's evening.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

type fakeFetcher struct {
	pages   map[string]string
	fetched []string
	fail    map[string]error
}

func (f *fakeFetcher) FetchOne(_ context.Context, rawURL string) ([]byte, error) {
	f.fetched = append(f.fetched, rawURL)
	if err, ok := f.fail[rawURL]; ok {
		return nil, err
	}
	body, ok := f.pages[rawURL]
	if !ok {
		return nil, fmt.Errorf("404 not found: %s", rawURL)
	}
	return []byte(body), nil
}

// listing builds a page linking the given work ids, plus whatever extra markup
// the test needs.
func listing(ids ...int64) string {
	var b strings.Builder
	b.WriteString(`<html><body><ol class="work index group">`)
	for _, id := range ids {
		fmt.Fprintf(&b, `<li class="work blurb"><h4><a href="/works/%d">W%d</a></h4></li>`, id, id)
	}
	b.WriteString(`</ol></body></html>`)
	return b.String()
}

const root = "https://archiveofourown.org"

func TestWalkHarvestsWorkIDsFromASeed(t *testing.T) {
	f := &fakeFetcher{pages: map[string]string{root + "/tags/Dark/works": listing(1, 2, 3)}}
	res, err := Walk(context.Background(), f, Options{Seeds: []string{root + "/tags/Dark/works"}})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !reflect.DeepEqual(res.Works, []int64{1, 2, 3}) {
		t.Errorf("Works = %v; want [1 2 3]", res.Works)
	}
	if res.Pages != 1 {
		t.Errorf("Pages = %d; want 1", res.Pages)
	}
	if res.Stopped != StopDrained {
		t.Errorf("Stopped = %q; want %q", res.Stopped, StopDrained)
	}
}

func TestWalkReportsAWorkOnceHoweverManyListingsLinkIt(t *testing.T) {
	// The same work appearing in several listings is the normal case on AO3.
	// Reporting it once is what makes the id list a work list rather than a
	// fetch list.
	f := &fakeFetcher{pages: map[string]string{
		root + "/tags/A/works": listing(1, 2) + `<a href="/tags/B/works">b</a>`,
		root + "/tags/B/works": listing(2, 3),
	}}
	res, err := Walk(context.Background(), f, Options{
		Seeds:       []string{root + "/tags/A/works"},
		MaxDepth:    1,
		FollowKinds: []string{"tag"},
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// Seed A links /tags/B/works, so B is one hop out and its works are found;
	// 2 is in both and must appear once.
	if !reflect.DeepEqual(res.Works, []int64{1, 2, 3}) {
		t.Errorf("Works = %v; want [1 2 3] with 2 reported once", res.Works)
	}
}

func TestWalkDepthZeroFetchesOnlyTheSeeds(t *testing.T) {
	// MaxDepth 0 is "show me exactly what I asked for". A walk that expanded
	// here would make the cheapest option cost the most.
	link := `<a href="/tags/Other/works">other</a>`
	f := &fakeFetcher{pages: map[string]string{
		root + "/tags/A/works":     listing(1) + link,
		root + "/tags/Other/works": listing(2),
	}}
	res, err := Walk(context.Background(), f, Options{
		Seeds:       []string{root + "/tags/A/works"},
		MaxDepth:    0,
		FollowKinds: []string{"tag"},
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Pages != 1 {
		t.Errorf("Pages = %d; want 1 — depth 0 must not expand", res.Pages)
	}
	if !reflect.DeepEqual(res.Works, []int64{1}) {
		t.Errorf("Works = %v; want [1]", res.Works)
	}
}

func TestWalkFollowsOnlyTheRequestedKinds(t *testing.T) {
	// The page links a tag AND a user's bookmarks. Following both doubles the
	// budget on half the intent.
	body := listing(1) +
		`<a href="/tags/Other/works">tag</a>` +
		`<a href="/users/someone/bookmarks">bookmarks</a>`
	f := &fakeFetcher{pages: map[string]string{
		root + "/tags/A/works":            body,
		root + "/tags/Other/works":        listing(2),
		root + "/users/someone/bookmarks": listing(3),
	}}
	res, err := Walk(context.Background(), f, Options{
		Seeds:       []string{root + "/tags/A/works"},
		MaxDepth:    1,
		FollowKinds: []string{"tag"},
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !reflect.DeepEqual(res.Works, []int64{1, 2}) {
		t.Errorf("Works = %v; want [1 2] — the bookmarks page must not be walked", res.Works)
	}
	for _, u := range f.fetched {
		if strings.Contains(u, "bookmarks") {
			t.Errorf("fetched %q; FollowKinds did not exclude it", u)
		}
	}
}

func TestWalkPaginatesAtTheSameDepth(t *testing.T) {
	// Page 2 is the same distance from the seed as page 1. A walk that treated
	// pagination as a level deeper would spend its whole depth budget on the
	// first listing.
	page1 := listing(1) + `<a rel="next" href="/tags/A/works?page=2">Next</a>`
	f := &fakeFetcher{pages: map[string]string{
		root + "/tags/A/works":        page1,
		root + "/tags/A/works?page=2": listing(2),
	}}
	res, err := Walk(context.Background(), f, Options{
		Seeds:    []string{root + "/tags/A/works"},
		MaxDepth: 0, // no expansion at all, but pagination still runs
		Paginate: true,
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !reflect.DeepEqual(res.Works, []int64{1, 2}) {
		t.Errorf("Works = %v; want [1 2] — pagination must not be depth-gated", res.Works)
	}
}

func TestWalkStopsAtMaxWorks(t *testing.T) {
	// The bound that matters in practice: a tag paginates to thousands, and
	// "give me a few hundred" should not require reasoning about depth.
	f := &fakeFetcher{pages: map[string]string{
		root + "/tags/A/works": listing(1, 2, 3, 4, 5),
	}}
	res, err := Walk(context.Background(), f, Options{
		Seeds:    []string{root + "/tags/A/works"},
		MaxWorks: 3,
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Works) != 3 {
		t.Errorf("Works = %v; want exactly 3", res.Works)
	}
	if res.Stopped != StopMaxWorks {
		t.Errorf("Stopped = %q; want %q", res.Stopped, StopMaxWorks)
	}
}

func TestWalkStopsAtMaxPages(t *testing.T) {
	// MaxPages is what bounds PAGINATION, since the depth gate does not apply
	// to it. Without this bound a paginated walk of one tag runs for hours.
	next := func(n int) string {
		return fmt.Sprintf(`<a rel="next" href="/tags/A/works?page=%d">Next</a>`, n)
	}
	f := &fakeFetcher{pages: map[string]string{
		root + "/tags/A/works":        listing(1) + next(2),
		root + "/tags/A/works?page=2": listing(2) + next(3),
		root + "/tags/A/works?page=3": listing(3) + next(4),
		root + "/tags/A/works?page=4": listing(4),
	}}
	res, err := Walk(context.Background(), f, Options{
		Seeds:    []string{root + "/tags/A/works"},
		Paginate: true,
		MaxPages: 2,
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Pages != 2 {
		t.Errorf("Pages = %d; want 2", res.Pages)
	}
	if res.Stopped != StopMaxPages {
		t.Errorf("Stopped = %q; want %q", res.Stopped, StopMaxPages)
	}
	if !reflect.DeepEqual(res.Works, []int64{1, 2}) {
		t.Errorf("Works = %v; want [1 2]", res.Works)
	}
}

func TestWalkDoesNotLoopOnACyclicFrontier(t *testing.T) {
	// Two tags that link each other is the ordinary shape of a tag graph, not a
	// contrived one. Without the visited set this is an infinite crawl.
	f := &fakeFetcher{pages: map[string]string{
		root + "/tags/A/works": listing(1) + `<a href="/tags/B/works">b</a>`,
		root + "/tags/B/works": listing(2) + `<a href="/tags/A/works">a</a>`,
	}}
	res, err := Walk(context.Background(), f, Options{
		Seeds:       []string{root + "/tags/A/works"},
		MaxDepth:    3,
		FollowKinds: []string{"tag"},
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Pages != 2 {
		t.Errorf("Pages = %d; want 2 — a page must be fetched once however often it is linked", res.Pages)
	}
}

func TestWalkRecordsFailuresAndCarriesOn(t *testing.T) {
	// A BFS that stopped at the first 503 would discover nothing on a site that
	// returns them under load.
	f := &fakeFetcher{
		pages: map[string]string{
			root + "/tags/Good/works": listing(7),
		},
		fail: map[string]error{
			root + "/tags/Bad/works": errors.New("503 service unavailable\nlong html body follows"),
		},
	}
	res, err := Walk(context.Background(), f, Options{
		Seeds: []string{root + "/tags/Bad/works", root + "/tags/Good/works"},
	})
	if err != nil {
		t.Fatalf("Walk returned an error for a fetch failure: %v", err)
	}
	if !reflect.DeepEqual(res.Works, []int64{7}) {
		t.Errorf("Works = %v; want [7] — the good seed must still be walked", res.Works)
	}
	msg, ok := res.Failed[root+"/tags/Bad/works"]
	if !ok {
		t.Fatalf("Failed = %v; want the bad seed recorded", res.Failed)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("Failed message has a newline; a report is read as a table: %q", msg)
	}
}

func TestWalkBoundsTheFailureList(t *testing.T) {
	// A six-hour walk against a flaky site must not accumulate one string per
	// request, and the report must not imply the list is complete when it is
	// not.
	f := &fakeFetcher{pages: map[string]string{}, fail: map[string]error{}}
	var seeds []string
	for i := 0; i < maxRecordedFailures+20; i++ {
		u := fmt.Sprintf("%s/tags/T%d/works", root, i)
		seeds = append(seeds, u)
		f.fail[u] = errors.New("boom")
	}
	res, err := Walk(context.Background(), f, Options{Seeds: seeds})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Failed) != maxRecordedFailures {
		t.Errorf("len(Failed) = %d; want %d", len(res.Failed), maxRecordedFailures)
	}
	if res.Truncated != 20 {
		t.Errorf("Truncated = %d; want 20 — an omitted failure must still be counted", res.Truncated)
	}
}

func TestWalkCallsOnWorkInDiscoveryOrder(t *testing.T) {
	// A caller persisting ids as it goes needs them in the order they were
	// found, and needs its error to stop the walk rather than be dropped.
	var got []int64
	f := &fakeFetcher{pages: map[string]string{root + "/tags/A/works": listing(5, 4, 3)}}
	_, err := Walk(context.Background(), f, Options{
		Seeds:  []string{root + "/tags/A/works"},
		OnWork: func(id int64) error { got = append(got, id); return nil },
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !reflect.DeepEqual(got, []int64{5, 4, 3}) {
		t.Errorf("OnWork saw %v; want [5 4 3]", got)
	}
}

func TestWalkPropagatesAnOnWorkError(t *testing.T) {
	// A sink that fails must not be silently ignored: the alternative is a walk
	// that reports hundreds of works it never wrote.
	sentinel := errors.New("sink is down")
	f := &fakeFetcher{pages: map[string]string{root + "/tags/A/works": listing(1, 2, 3)}}
	_, err := Walk(context.Background(), f, Options{
		Seeds:  []string{root + "/tags/A/works"},
		OnWork: func(int64) error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Walk error = %v; want the sink's error", err)
	}
}

func TestWalkStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeFetcher{pages: map[string]string{root + "/tags/A/works": listing(1)}}
	res, err := Walk(ctx, f, Options{Seeds: []string{root + "/tags/A/works"}})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Pages != 0 {
		t.Errorf("Pages = %d; want 0 — a cancelled walk must not fetch", res.Pages)
	}
	if res.Stopped != StopCanceled {
		t.Errorf("Stopped = %q; want %q", res.Stopped, StopCanceled)
	}
}

func TestWalkRefusesNoFetcher(t *testing.T) {
	if _, err := Walk(context.Background(), nil, Options{}); err == nil {
		t.Error("Walk(nil fetcher) returned no error")
	}
}

func TestWalkFetchesEachPageOnceEvenWhenSeedsOverlap(t *testing.T) {
	f := &fakeFetcher{pages: map[string]string{root + "/tags/A/works": listing(1)}}
	res, err := Walk(context.Background(), f, Options{Seeds: []string{
		root + "/tags/A/works", root + "/tags/A/works",
	}})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Pages != 1 {
		t.Errorf("Pages = %d; want 1 for a duplicated seed", res.Pages)
	}
	if len(f.fetched) != 1 {
		t.Errorf("fetched %v; want one request", f.fetched)
	}
}

func TestListingKindCoversTheShapesAO3ActuallyUses(t *testing.T) {
	// Pinned here as well as in the crawl package because a wrong kind means a
	// frontier that misses listings or follows chrome, and both are invisible
	// in a result that merely looks smaller than expected.
	for _, tc := range []struct{ url, want string }{
		{root + "/tags/Dark%20Fic/works", "tag"},
		{root + "/tags/Dark%20Fic", "tag"},
		{root + "/works?work_search%5Bquery%5D=dragons", "search"},
		{root + "/works", ""},
		{root + "/users/someone/works", "user-works"},
		{root + "/users/someone/bookmarks", "user-bookmarks"},
		{root + "/users/someone/pseuds/p/bookmarks", "user-bookmarks"},
		{root + "/collections/foo/works", "collection"},
		{root + "/series/12345", "series"},
		{root + "/series/new", ""},
		{root + "/works/999", ""},
		{root + "/users/someone", ""},
		{root + "/", ""},
		{"not a url at all ://", ""},
	} {
		if got := crawl.ListingKind(tc.url); got != tc.want {
			t.Errorf("ListingKind(%q) = %q; want %q", tc.url, got, tc.want)
		}
	}
}

func TestWalkFollowsSeveralKindsWhenAsked(t *testing.T) {
	body := listing(1) +
		`<a href="/tags/Other/works">tag</a>` +
		`<a href="/users/someone/bookmarks">bookmarks</a>`
	f := &fakeFetcher{pages: map[string]string{
		root + "/tags/A/works":            body,
		root + "/tags/Other/works":        listing(2),
		root + "/users/someone/bookmarks": listing(3),
	}}
	res, err := Walk(context.Background(), f, Options{
		Seeds:       []string{root + "/tags/A/works"},
		MaxDepth:    1,
		FollowKinds: []string{"tag", "user-bookmarks"},
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sorted := append([]int64(nil), res.Works...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if !reflect.DeepEqual(sorted, []int64{1, 2, 3}) {
		t.Errorf("Works = %v; want 1, 2 and 3", res.Works)
	}
}
