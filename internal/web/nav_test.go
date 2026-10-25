package web

// The nav's active marker. Its failure mode is silent in the worst way: a nav
// entry that can never be marked current still renders, still links, and simply
// never tells the reader where they are -- so nothing looks broken.

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestNavKeyMapsTheAliasesAndPassesEverythingElseThrough(t *testing.T) {
	for _, tc := range []struct{ path, want, why string }{
		{"/", "/", "the home page is its own nav item"},
		{"/search", "/", "the search results are the home page's form action"},
		{"/profile", "/profiles", "one profile is reached from the profiles list"},
		{"/crawl", "/crawl", "everything else is itself"},
		{"/works/12345", "/works/12345", "a work page is reachable from every section"},
		{"/tags/7", "/tags/7", "so is a tag page, so it marks nothing"},
		{"/rank/9", "/rank/9", "so is a ranking"},
	} {
		if got := navKey(tc.path); got != tc.want {
			t.Errorf("navKey(%q) = %q; want %q (%s)", tc.path, got, tc.want, tc.why)
		}
	}
}

// navHrefs reads the nav's link targets out of the layout that ships.
//
// Read from the embedded asset rather than from a copy in the test: a list
// duplicated in a test passes while the real nav drifts, which is the failure
// this whole file is about.
func navHrefs(t *testing.T) []string {
	t.Helper()
	b, err := fs.ReadFile(assets, "assets/layout.html")
	if err != nil {
		t.Fatalf("reading the embedded layout: %v", err)
	}
	nav := string(b)
	i := strings.Index(nav, `<nav id="nav"`)
	j := strings.Index(nav, "</nav>")
	if i < 0 || j < i {
		t.Fatal("no nav found in layout.html")
	}
	re := regexp.MustCompile(`href="([^"]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(nav[i:j], -1) {
		out = append(out, m[1])
	}
	if len(out) < 10 {
		t.Fatalf("parsed only %d nav hrefs; the parser or the nav is broken", len(out))
	}
	return out
}

func TestEveryNavEntryCanMarkItselfCurrent(t *testing.T) {
	// The invariant: a nav item that navKey maps somewhere else can never
	// highlight itself, so the reader is told they are on a different page than
	// the one they clicked. Adding a link without checking navKey is exactly how
	// that happens.
	for _, href := range navHrefs(t) {
		if strings.HasPrefix(href, "/api/") {
			// The JSON API has no page, so it has no current state by design.
			continue
		}
		if got := navKey(href); got != href {
			t.Errorf("nav links to %q but navKey maps it to %q, so that link can never show as current", href, got)
		}
	}
}

func TestTheNavHasNoDuplicateTargets(t *testing.T) {
	// Two links to one page is a reader choosing between identical options.
	seen := map[string]int{}
	for _, h := range navHrefs(t) {
		seen[h]++
	}
	var dupes []string
	for h, n := range seen {
		if n > 1 {
			dupes = append(dupes, h)
		}
	}
	sort.Strings(dupes)
	if len(dupes) > 0 {
		t.Errorf("nav links to the same place twice: %v", dupes)
	}
}

func TestTheNavGroupsAreLabelled(t *testing.T) {
	// An unlabelled group is a grouping nobody can see -- which is what the nav
	// was before: three bare <ul>s whose relationship existed only in the markup.
	b, err := fs.ReadFile(assets, "assets/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	nav := string(b)
	i, j := strings.Index(nav, `<nav id="nav"`), strings.Index(nav, "</nav>")
	body := nav[i:j]

	groups := strings.Count(body, `class="nav-group"`)
	labels := strings.Count(body, `class="nav-label"`)
	if groups == 0 {
		t.Fatal("the nav has no groups")
	}
	if groups != labels {
		t.Errorf("%d nav groups but %d labels; every group needs a name", groups, labels)
	}
	// And each label must have text, not just the class.
	for _, m := range regexp.MustCompile(`<h2 class="nav-label">([^<]*)</h2>`).FindAllStringSubmatch(body, -1) {
		if strings.TrimSpace(m[1]) == "" {
			t.Error("a nav label is empty")
		}
	}
}

func TestAPageMarksItsOwnNavEntry(t *testing.T) {
	// End to end through the router: the marker is on the RIGHT link, not merely
	// present somewhere in the page.
	d, _ := newCrawlDeps(t)
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/crawl", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /crawl = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	want := `<a href="/crawl" class="is-current" aria-current="page">Crawl queue</a>`
	if !strings.Contains(body, want) {
		t.Errorf("the crawl page does not mark its own nav entry:\n  want %s", want)
	}
	// Exactly one item is current. Two would say the reader is in two places.
	if n := strings.Count(body, `class="is-current"`); n != 1 {
		t.Errorf("%d nav items are marked current; want 1", n)
	}
	// And a page the nav does not name marks nothing rather than guessing.
	rec2 := httptest.NewRecorder()
	d.route(rec2, httptest.NewRequest(http.MethodGet, "/works/12345", nil))
	if n := strings.Count(rec2.Body.String(), `class="is-current"`); n != 0 {
		t.Errorf("/works/1 marked %d nav items current; a work page is reachable from every section", n)
	}
}
