package web

// The registry's invariants.
//
// Each of these was previously nobody's job, and each is a way a page can be
// broken without anything noticing:
//
//   - a spec whose path is claimed twice, where the loser answers with the
//     wrong handler instead of 404ing (axum 0.8 merged duplicate routes and
//     shadowed the earlier one silently, and the estate paid for it)
//   - a spec no path reaches, which is a page that exists in the source and
//     not on the site
//   - a spec with no handler, which panics on first request
//   - a page template with no route, or a route with no template file
//   - a list page that renders nothing and says nothing
//
// The first three are checked here; the last two live beside the render sweep
// (render_pages_test.go), because that is where the page list is read.

import (
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"
)

// representativePath turns a spec's patterns into a path a request could use.
// Prefix patterns get a tail, because the tail is what the handler is handed.
func representativePath(spec pageSpec, pattern string) string {
	if spec.Prefix {
		return pattern + "1"
	}
	return pattern
}

// TestEveryPageHasAHandler keeps a spec from existing only as a declaration.
func TestEveryPageHasAHandler(t *testing.T) {
	for _, spec := range allPages() {
		if spec.Handler == nil {
			// A page with no path is rendered directly by name (error.html is
			// fail()'s, notfound.html is notFound()'s), so it has no handler
			// and no route: reaching it by request is the thing that must not
			// be possible.
			if len(spec.Paths) == 0 {
				continue
			}
			t.Errorf("%s is routed but has no handler; the first request panics",
				spec.Paths[0])
		}
	}
}

// TestUnroutedPagesAreTheErrorPages names the pages that are deliberately
// unreachable by URL.
//
// A template with no path is a page nobody can fetch, which is right for the
// two error documents and wrong for everything else: error.html rendered at
// /error is a page that can be linked and cached as if it were an answer.
func TestUnroutedPagesAreTheErrorPages(t *testing.T) {
	// template -> why it must not be reachable
	unrouted := map[string]string{
		"error.html":    "fail() renders it for any handler error; a URL for it would be cacheable",
		"notfound.html": "notFound() renders it, and falls back to plain text if it cannot",
	}

	for _, spec := range allPages() {
		if len(spec.Paths) == 0 {
			if _, ok := unrouted[spec.Template]; !ok {
				t.Errorf("%s has no path and is not a declared error page; either route it "+
					"or say here why it must stay unreachable", spec.Template)
			}
			continue
		}
		if why, ok := unrouted[spec.Template]; ok {
			t.Errorf("%s is declared unreachable (%s) but has path %s",
				spec.Template, why, spec.Paths[0])
		}
	}

	// And the other direction, so a deleted route cannot hide: the error pages
	// must still be the only unrouted ones.
	gave := map[string]bool{}
	for _, spec := range allPages() {
		if len(spec.Paths) == 0 {
			gave[spec.Template] = true
		}
	}
	for tpl := range unrouted {
		if !gave[tpl] {
			t.Errorf("%s is no longer registered at all; it is in the asset set and "+
				"renders from a handler that expects it", tpl)
		}
	}
}

// TestNoPathIsClaimedTwice is the shadowing check.
//
// findPage returns the first match, so two specs that both answer a path is not
// an error at request time -- it is a silent reroute, and the shadowed page
// answers with the wrong handler rather than 404ing. Two things are therefore
// refused: any path matched by two specs, and any prefix pattern that swallows
// another declared pattern (which is the same fault one step earlier, and
// catches it even when the swallowed pattern's own path is never hit in tests).
func TestNoPathIsClaimedTwice(t *testing.T) {
	specs := allPages()

	// Every concrete path a spec can answer must resolve back to that spec.
	for _, spec := range specs {
		for _, p := range spec.Paths {
			path := representativePath(spec, p)
			got, _, ok := findPage(path)
			if !ok {
				t.Errorf("no spec answers %s, which %s declares", path, spec.Template)
				continue
			}
			if got.Template != spec.Template {
				t.Errorf("%s is declared by %s and answered by %s",
					path, spec.Template, got.Template)
			}
		}
	}

	// No prefix pattern may swallow another spec's pattern. Declaration order
	// decides the winner, so this is what keeps "first match wins" from
	// becoming a precedence puzzle.
	for _, a := range specs {
		if !a.Prefix {
			continue
		}
		for _, ap := range a.Paths {
			for _, b := range specs {
				for _, bp := range b.Paths {
					if b.Template == a.Template {
						continue
					}
					if strings.HasPrefix(bp, ap) {
						t.Errorf("%s's prefix %s shadows %s's %s: the second can never "+
							"be reached, and moving either spec would reroute it",
							a.Template, ap, b.Template, bp)
					}
				}
			}
		}
	}
}

// TestEveryRoutedPageDispatches asks the router for every declared path.
//
// It asserts dispatch, not page health: a page with a missing dependency is
// supposed to answer 503 and say why (nil_deps_test.go pins that), so the only
// statuses that mean "this page was never reached" are the router's own 404 and
// 405. Without this, a spec can be declared, listed, sweep-tested and still
// never answer a request -- it would just be a name in a slice.
func TestEveryRoutedPageDispatches(t *testing.T) {
	routed := 0
	for _, spec := range allPages() {
		for _, p := range spec.Paths {
			routed++
			path := representativePath(spec, p)
			// Each path is its own subtest with a recover, so one page that
			// cannot answer does not hide the state of the pages behind it in
			// declaration order. Asking for one page at a time found the
			// missing-corpus panic in /tag/{slug} after two runs; the recover
			// finds every one of them in a single run and names the page.
			t.Run(path, func(t *testing.T) {
				defer func() {
					if rec := recover(); rec != nil {
						t.Errorf("GET %s panicked: %v\n	a page must answer with a status and "+
							"a reason when a dependency is missing, not close the connection\n%s",
							path, rec, debug.Stack())
					}
				}()
				rec := httptest.NewRecorder()
				Deps{}.route(rec, httptest.NewRequest(http.MethodGet, path, nil))

				if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
					t.Errorf("GET %s = %d; the spec declaring it never ran (the router "+
						"answered for it instead)", path, rec.Code)
				}
			})
		}
	}
	if routed == 0 {
		t.Fatal("no specs declare a path; the registry is empty")
	}
}

// TestEveryListPageMarksItsEmptyState is the check the tracker said could not
// exist for this page: a list page that finds nothing has to say so in a way a
// reader can see and a test can find, and "so" has to be the same everywhere
// rather than a different phrase on every page.
//
// The marker is data-empty="true" on the element that says it. The check is a
// source scan because the alternative -- constructing an empty result for every
// page's own data type -- would be 17 hand-written fixtures that would each
// silently stop matching when a field is renamed. The source scan cannot be
// fooled by a rename and cannot drift: it reads the file the parser reads.
func TestEveryListPageMarksItsEmptyState(t *testing.T) {
	const marker = `data-empty="true"`

	for _, spec := range allPages() {
		if !spec.List {
			continue
		}
		src, err := assets.ReadFile("assets/" + spec.Template)
		if err != nil {
			t.Errorf("%s is a list page but its template is unreadable: %v", spec.Template, err)
			continue
		}
		if !strings.Contains(string(src), marker) {
			t.Errorf("%s renders a list of rows and never marks its empty state with "+
				"%s; a reader who finds nothing is told nothing", spec.Template, marker)
		}
	}
}

// TestPageFormPathsAreUnique keeps the POST table from becoming a second
// dispatch order nobody reads. A duplicate here is not a compile error: both
// entries are valid, one wins, and the other silently stops being routed.
func TestPageFormPathsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range pageForms {
		if seen[f.Path] {
			t.Errorf("%s appears twice in pageForms; the second is unreachable", f.Path)
		}
		seen[f.Path] = true
		if f.Handler == nil {
			t.Errorf("%s has no handler; a POST to it panics", f.Path)
		}
	}
	if len(pageForms) == 0 {
		t.Error("pageForms is empty; the pages that own a form can no longer POST")
	}
}

// TestPostPagesAreNamedFromTheRegistry pins the 405's message against the
// registry. The message used to name /profiles and /profile as literals, which
// is right until a third page accepts one and then quietly wrong at exactly the
// moment a caller is reading it.
func TestPostPagesAreNamedFromTheRegistry(t *testing.T) {
	named := postPages()
	if len(named) == 0 {
		t.Fatal("no page accepts a POST; the profile and rating forms are unreachable")
	}
	for _, p := range named {
		spec, _, ok := findPage(p)
		if !ok {
			t.Errorf("%s is announced as a POST target but no spec answers GET on it", p)
			continue
		}
		if !spec.AcceptsPost {
			t.Errorf("%s is announced as a POST target but %s does not accept one", p, spec.Template)
		}
	}
}

// TestListPagesEmptyStateIsTwoSided is the other half of the marker: the page
// must say it found nothing when it found nothing, and must not say so when it
// found something.
//
// The marker is an assertion, and an element that renders alongside rows while
// carrying it makes the assertion false -- worse than having no marker, because
// the next reader believes it. Both directions are driven by the populated
// sweep table, so this costs no fixtures of its own and covers every list page
// at once; each case declares which side it is on with hasEmptyList.
func TestListPagesEmptyStateIsTwoSided(t *testing.T) {
	isList := map[string]bool{}
	for _, spec := range allPages() {
		if spec.List {
			isList[spec.Template] = true
		}
	}

	checked, emptyCases := 0, 0
	emptySeen := map[string]bool{}
	for _, tc := range pageRenderCases {
		if !isList[tc.tpl] {
			continue
		}
		checked++
		body := renderOne(t, tc.tpl, tc.data)
		hasMarker := strings.Contains(body, "data-empty")

		switch {
		case tc.hasEmptyList && !hasMarker:
			t.Errorf("%s renders a list with no rows and marks nothing; a reader who "+
				"finds nothing is told nothing, and cannot tell an empty result from a "+
				"page that failed to load one", tc.name)
		case !tc.hasEmptyList && hasMarker:
			t.Errorf("%s renders its rows and still marks something empty (data-empty "+
				"appears in the populated render); a marker that shows with rows is the "+
				"same lie the other way round", tc.name)
		}
		if tc.hasEmptyList {
			emptyCases++
			emptySeen[tc.tpl] = true
		}
	}
	if checked == 0 || emptyCases == 0 {
		t.Fatalf("%d list cases checked, %d of them empty; both directions have to be "+
			"exercised for this test to mean anything", checked, emptyCases)
	}

	// Every list page needs an empty case in this table, or the marker on it is
	// only ever checked as text (the source scan in
	// TestEveryListPageMarksItsEmptyState) and never as a render.
	//
	// That gap was real and the mutation gate found it: mirror.html has four
	// empty states, so removing ONE marker left the page's marker count
	// unchanged and the mutant survived -- until the two cases for mirror each
	// left exactly one empty state on the page. The five pages this check was
	// written for had a populated case and no empty one, which is that same
	// hole waiting to be reopened by the next edit to their templates.
	//
	// emptyCoveredElsewhere names the pages whose empty render is asserted
	// somewhere else instead, so this stays a statement about coverage rather
	// than a demand for duplicate fixtures.
	emptyCoveredElsewhere := map[string]string{
		"myranking.html": "TestTheEmptyStateIsSaidOnce",
	}
	for tpl := range isList {
		if emptySeen[tpl] {
			continue
		}
		where, covered := emptyCoveredElsewhere[tpl]
		if covered {
			t.Logf("%s has no empty case here; its empty render is asserted by %s", tpl, where)
			continue
		}
		t.Errorf("%s is a list page with no case that renders it empty, so a marker "+
			"moved off its empty branch would survive this test and the source scan "+
			"alike; add a case with hasEmptyList: true", tpl)
	}

	t.Logf("%d list cases: %d with rows, %d with an empty list", checked, checked-emptyCases, emptyCases)
}

// TestTheEmptyStateIsSaidOnce catches the other way this page can lie: saying
// the same thing twice.
//
// These three pages print their note in the branch that explains the empty
// state AND (before this test) once more at the end of the page, which read as
// two separate facts when it was one sentence printed twice. Only the three
// pages with that shape are listed: a page with two lists legitimately has two
// empty states, and counting markers across a page would punish the pages that
// mark both.
func TestTheEmptyStateIsSaidOnce(t *testing.T) {
	cases := []struct {
		name string
		data any
		// mustAppear is the sentence the empty state owes the reader. It is a
		// distinctive fragment rather than the whole string, because the note
		// is built with a count in it.
		mustAppear string
	}{
		{
			name: "myranking.html",
			data: MyRankingPage{
				Base: sweepBase, Judged: 1, MinimumForView: 5,
				Note: "Not enough comparisons yet (1 of 5 needed). The arena needs a few " +
					"choices before its opinion of your preferences is worth showing.",
			},
			mustAppear: "Not enough comparisons yet",
		},
		{
			name: "block.html",
			data: BlockPage{
				Base: sweepBase, Empty: true, Judged: 1, MinimumForView: 5,
				Note: "Nothing to suggest yet. A tag becomes a candidate when the arena " +
					"learns you choose against it.",
			},
			mustAppear: "Nothing to suggest yet",
		},
		{
			name: "leaderboard.html",
			data: LeaderboardPage{
				Base: sweepBase, Empty: true,
				Note: "Nothing has been rated yet. The arena needs comparisons before a " +
					"ranking means anything.",
			},
			mustAppear: "Nothing has been rated yet",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := renderOne(t, tc.name, tc.data)
			if n := strings.Count(body, tc.mustAppear); n != 1 {
				t.Errorf("%s says %q %d times; the empty state is one fact and the page "+
					"states it once", tc.name, tc.mustAppear, n)
			}
			if n := strings.Count(body, "data-empty"); n != 1 {
				t.Errorf("%s marks %d element(s) empty in its empty state; one page, one "+
					"list, one marker", tc.name, n)
			}
		})
	}
}

func TestBrowsePageIsRegisteredAndRouted(t *testing.T) {
	spec, _, ok := findPage("/browse")
	if !ok {
		t.Fatal("/browse is not in the page registry")
	}
	if spec.Nav == "" {
		t.Error("/browse has no Nav, so no nav item can mark it current")
	}
	if spec.Handler == nil {
		t.Error("/browse has no Handler")
	}
	if !spec.List {
		t.Error("/browse renders a list of rows but does not declare List: true, " +
			"so the two-sided empty-state contract is not applied to it")
	}
}
