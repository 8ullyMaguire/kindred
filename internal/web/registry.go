package web

// The page registry.
//
// A page in this server was four facts that had to agree, kept in four
// hand-written lists: the route switch in route(), the template-name slice in
// render.go's init(), navKey()'s two aliases, and the test-side sweep table in
// render_pages_test.go. Adding one page (/mirror) touched five files, and two
// of the five were checked by nothing -- the route switch itself and the
// template-name slice were both "hand-synced, one of them unchecked", which is
// the shape this repo has already paid for twice (arena_ratings declared twice
// under CREATE TABLE IF NOT EXISTS, and the duplicate index scan that turned out
// to be correct).
//
// pageSpecs is now the single list. Everything else is derived from it:
//
//   - `pages` (the parsed template set per page) is built from Template
//   - route() iterates it, first match wins
//   - navKey() reads Nav
//   - the render sweep, the routing check and the empty-state check iterate it
//
// registry_test.go pins the invariants that were previously nobody's job: no two
// specs claim the same path, every Template exists as an asset, every page has a
// sweep case, every page is reachable and dispatches, and every list page marks
// its empty state.
//
// What this file deliberately does NOT do is decide routing precedence by
// declaration order for overlapping patterns. A prefix pattern that would
// shadow a later pattern is a test failure, not a subtlety: axum 0.8 was found
// in this estate to merge duplicate routes and shadow the earlier one silently,
// so "the first match wins" is only safe when nothing can be shadowed at all.

import (
	"net/http"
	"strings"
	"sync"
)

var (
	specsOnce sync.Once
	specs     []pageSpec
)

// shellTemplate is the shared shell every page's template set is parsed with.
//
// It is a fragment rather than a page: layout.html's body is a single
// {{define "layout"}}, and every page's set parses it plus that one page's file
// so that "content" is unique per set (see the comment on `pages`).
const shellTemplate = "layout.html"

// pageSpec is one URL this server answers with a page.
//
// A template may serve more than one spec: / and /search are two URLs and one
// page, exactly as they were two cases and one handler before.
type pageSpec struct {
	// Template is the file under assets/ this page renders, and the key its
	// parsed template set is stored under.
	Template string

	// Paths are the URL patterns that reach this page. With Prefix set they
	// match by prefix and the rest of the path is handed to the handler as its
	// argument; otherwise each matches one exact path.
	//
	// Empty means the template is a page with no URL of its own: notfound.html
	// and error.html are rendered by notFound() and fail() directly, and must
	// not be reachable by a path, because a page that can be fetched directly
	// is a page that can be cached or linked as if it were a real answer.
	Paths []string

	// Prefix marks Paths as prefix patterns.
	//
	// It is an explicit flag rather than the obvious "a pattern ending in /",
	// which is the first thing this cost: the home page's pattern IS "/", so
	// reading a trailing slash as "prefix" made the home page a prefix of every
	// path on the site. Every unknown URL then rendered the home page with a
	// 200 where the site has to answer 404, and what caught it was the test
	// asserting unknown paths are 404 -- three failing cases, one cause.
	Prefix bool

	// Nav is the nav item this page marks as current, or "" for a page the nav
	// does not name. Work, tag, rank and author pages are reached from every
	// section, so marking one would be a claim about how the reader arrived
	// rather than about where they are; navKey() returns the path unchanged for
	// those, and the nav has no such entry to mark.
	Nav string

	// List is set when the page renders a list of rows from a query, which
	// means it has a state where the list is empty and the page has to say
	// something a reader and a test can both find. Every List page's template
	// carries data-empty="true" on each element that says so -- see
	// TestEveryListPageMarksItsEmptyState, which is the check the row in the
	// tracker said could not exist, and its two complements: a page WITH rows
	// must not claim emptiness, and the pages that print a note in both states
	// must say it once.
	//
	// data-empty rather than a data-testid="empty" because an element that
	// already has a specific id (search.html's no-works, surprise.html's
	// surprise-empty, tag.html's no-matches) cannot carry a second
	// data-testid: the attribute would have to be renamed to the uniform one
	// and the specific hook lost, which is the choice that made the spellings
	// differ in the first place. The six pages that had already settled on
	// data-testid="empty" keep it -- the browser suite selects on it -- and
	// carry data-empty too, so one selector now finds the empty state on every
	// list page instead of on half of them.
	List bool

	// AcceptsPost marks a page whose own URL is a form target, so its handler
	// builds a page from a POST as well as a GET. It is not a general
	// "mutable" flag: the one-button POSTs below (pageForms) are handled
	// before dispatch and are not pages at all.
	AcceptsPost bool

	// Handler builds the page. arg is the tail of a prefix pattern and "" for
	// an exact one.
	Handler func(d Deps, w http.ResponseWriter, r *http.Request, arg string)
}

// pageForms are the page POSTs: one-button forms a page rendered, whose
// completion belongs on the page that drew it. A browser cannot land a reader
// back in a list from a JSON API, so these are the page's own writes.
//
// It is a table rather than four `if` statements so the set is enumerable: a
// POST surface that can only be read by reading the router is a POST surface
// nobody audits.
var pageForms = []pageForm{
	// The judge is the completion of the arena's own form and records exactly
	// what POST /api/v1/arena/compare records.
	{Path: "/arena/judge", Handler: func(d Deps, w http.ResponseWriter, r *http.Request) { d.postJudge(w, r) }},
	{Path: "/algo/judge", Handler: func(d Deps, w http.ResponseWriter, r *http.Request) { d.postAlgoJudge(w, r) }},
	// Blocking is a one-button form the /block page renders.
	{Path: "/block", Handler: func(d Deps, w http.ResponseWriter, r *http.Request) { d.postBlock(w, r) }},
	// Marking a work read or unread, from the result list. Without it the
	// read-exclusion half of the seen list would be unreachable in practice
	// while the shown half worked.
	{Path: "/seen", Handler: func(d Deps, w http.ResponseWriter, r *http.Request) { d.postSeen(w, r) }},
	// A like or a dislike, which has to redirect back into the list the reader
	// was reading rather than answer JSON.
	{Path: "/feedback", Handler: func(d Deps, w http.ResponseWriter, r *http.Request) { d.postFeedback(w, r) }},
}

type pageForm struct {
	Path    string
	Handler func(d Deps, w http.ResponseWriter, r *http.Request)
}

// allPages returns every page this server serves, in declaration order.
//
// It is a function around a sync.Once rather than a package-level slice
// because a page's handler calls baseOf, baseOf calls navKey, navKey calls
// findPage, and findPage reads this list: as a package-level variable that is
// an initialization cycle, which the compiler refuses outright. Built once on
// first use, so a lookup costs an atomic load rather than a rebuild.
func allPages() []pageSpec {
	specsOnce.Do(func() {
		specs = []pageSpec{
			{
				// The home page and the search page are two URLs and one handler. The
				// home page passes no query and the search page reads q, which is what
				// the two cases of the old switch did: the home page's own form posts
				// to /search, so a q on / is not a URL this site produces.
				Template: "search.html",
				Paths:    []string{"/"},
				Nav:      "/",
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderSearch(w, r, "") },
			},
			{
				Template: "search.html",
				Paths:    []string{"/search"},
				Nav:      "/",
				List:     true,
				Handler: func(d Deps, w http.ResponseWriter, r *http.Request, _ string) {
					d.renderSearch(w, r, r.URL.Query().Get("q"))
				},
			},
			{
				Template: "work.html",
				Paths:    []string{"/work/"},
				// A work page is a detail page, and it still renders a list of rows
				// from a query: its "more like this" section. It had no marker there,
				// so a work with no neighbours simply showed no section, which reads
				// as a page that failed to load one.
				List:    true,
				Prefix:  true,
				Handler: func(d Deps, w http.ResponseWriter, r *http.Request, arg string) { d.renderWork(w, r, arg) },
			},
			{
				Template: "tag.html",
				Paths:    []string{"/tag/"},
				Prefix:   true,
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, arg string) { d.renderTag(w, r, arg) },
			},
			{
				Template: "new.html",
				Paths:    []string{"/new"},
				// A list page: the empty state is the FIRST-VISIT case, which is
				// the normal state for every reader once.
				List:    true,
				Handler: func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderNew(w, r) },
			},
			{
				Template: "path.html",
				Paths:    []string{"/path/"},
				Prefix:   true,
				// A detail page that renders a list of rows. The list is the
				// page, so an empty one needs its marker like any other list.
				List:    true,
				Handler: func(d Deps, w http.ResponseWriter, r *http.Request, arg string) { d.renderPath(w, r, arg) },
			},
			{
				Template: "author.html",
				Paths:    []string{"/author"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderAuthor(w, r) },
			},
			{
				Template: "recommend.html",
				Paths:    []string{"/recommend"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderRecommend(w, r) },
			},
			{
				Template: "surprise.html",
				Paths:    []string{"/surprise"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderSurprise(w, r) },
			},
			{
				Template: "fandoms.html",
				Paths:    []string{"/fandoms"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderFandoms(w, r) },
			},
			{
				Template: "underrated.html",
				Paths:    []string{"/underrated"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderUnderrated(w, r) },
			},
			{
				Template: "neighbours.html",
				Paths:    []string{"/neighbours"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderNeighbours(w, r) },
			},
			{
				Template: "algo.html",
				Paths:    []string{"/algo"},
				Nav:      "/algo",
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderAlgo(w, r) },
			},
			{
				Template: "arena.html",
				Paths:    []string{"/arena"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderArena(w, r) },
			},
			{
				Template: "leaderboard.html",
				Paths:    []string{"/leaderboard"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderLeaderboard(w, r) },
			},
			{
				Template: "myranking.html",
				Paths:    []string{"/my-ranking"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderMyRanking(w, r) },
			},
			{
				Template: "rank.html",
				Paths:    []string{"/rank/"},
				Prefix:   true,
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, arg string) { d.renderRankPath(w, r, arg) },
			},
			{
				Template: "block.html",
				Paths:    []string{"/block"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderBlock(w, r) },
			},
			{
				Template:    "profiles.html",
				Paths:       []string{"/profiles"},
				List:        true,
				AcceptsPost: true,
				Handler:     func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderProfiles(w, r) },
			},
			{
				Template: "profile.html",
				Paths:    []string{"/profile"},
				// One profile is reached from the profiles list, so it marks that.
				Nav:         "/profiles",
				List:        true,
				AcceptsPost: true,
				Handler:     func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderProfile(w, r) },
			},
			{
				Template: "crawl.html",
				Paths:    []string{"/crawl"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderCrawl(w, r) },
			},
			{
				Template: "mirror.html",
				Paths:    []string{"/mirror"},
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderMirror(w, r) },
			},
			{
				// The evaluation dashboard: the offline numbers (persisted
				// eval runs), the online ones (algorithm votes and
				// interleaved credits) and what they are measuring across.
				// Read-only, and it is the only page whose subject is how
				// good the recommender is rather than what it recommends.
				Template: "eval.html",
				Paths:    []string{"/eval"},
				Nav:      "/eval",
				List:     true,
				Handler:  func(d Deps, w http.ResponseWriter, r *http.Request, _ string) { d.renderEval(w, r) },
			},
			{
				// No path: fail() renders it for any handler error that can be
				// described to a reader.
				Template: "error.html",
			},
			{
				// No path: notFound() renders it, and falls back to plain text if it
				// fails, because a broken error page must not become a 500 loop.
				Template: "notfound.html",
			},
		}
	})
	return specs
}

// findPage resolves a request path to the spec that answers it.
//
// First match wins, and the order in pageSpecs is the precedence -- which is
// only safe because TestNoPathIsClaimedTwice refuses a prefix pattern that
// would shadow a later one. Without that test, moving a spec would silently
// reroute a page, and a shadowed route answers with the WRONG handler rather
// than 404ing, so nothing would look broken.
func findPage(path string) (pageSpec, string, bool) {
	for _, spec := range allPages() {
		for _, p := range spec.Paths {
			if spec.Prefix {
				if strings.HasPrefix(path, p) {
					return spec, strings.TrimPrefix(path, p), true
				}
				continue
			}
			if path == p {
				return spec, "", true
			}
		}
	}
	return pageSpec{}, "", false
}

// pageFormHandler resolves a POST path to the page form that answers it.
func pageFormHandler(path string) (func(d Deps, w http.ResponseWriter, r *http.Request), bool) {
	for _, f := range pageForms {
		if f.Path == path {
			return f.Handler, true
		}
	}
	return nil, false
}

// postPages names the pages that accept a POST at their own URL, for the 405
// that tells a caller which methods do work.
//
// Derived from the registry rather than written into the message: the message
// said "only /profiles and /profile accept a POST" as a literal, so adding a
// third would have made the error text wrong in exactly the case a caller is
// reading it.
func postPages() []string {
	var out []string
	for _, spec := range allPages() {
		if spec.AcceptsPost && len(spec.Paths) > 0 {
			out = append(out, spec.Paths[0])
		}
	}
	return out
}

// navKey maps a request path to the nav item it belongs to.
//
// Two pages are reached by a URL the nav does not name: /search is the home
// page's form action, and /profile is one profile reached from /profiles. Both
// carry Nav in the registry; every other page passes through unchanged, and the
// ones the nav has no entry for (work, tag, rank, author) then mark nothing --
// which is a statement about the page, not a guess about the reader.
func navKey(path string) string {
	if spec, _, ok := findPage(path); ok && spec.Nav != "" {
		return spec.Nav
	}
	return path
}
