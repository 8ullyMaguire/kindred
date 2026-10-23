package api

// Every API route must be either reachable from a page or explicitly recorded
// as not being a reader's route.
//
// The audited claim is narrow and worth stating precisely, because the obvious
// stronger claim is not what this proves: the table records WHICH ROUTE HAS A
// PAGE. It does not check that the page shows the endpoint's data -- that is per
// -page behavioural work. What it does prevent is the unrecorded route: a new
// endpoint that quietly becomes reachable only by curl. The API is this
// project's product and that is a legitimate way to ship a feature; shipping one
// WITHOUT SAYING SO is how "all the functionality has a page" stops being true
// while every page still renders.
//
// The route list is read out of server.go with go/ast rather than copied into a
// slice here, because a copy is the thing that drifts: it would keep passing
// while the real table grew.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// routeAudit says, for each API route, either the page a reader reaches it
// through or why there is none. Exactly one of the two fields is set.
type routeAudit struct {
	// Page is the page path, with {id} for a per-entity page.
	Page string
	// Why is the reason there is no page. Empty when Page is set.
	Why string
}

var routeTable = map[string]routeAudit{
	// --- feeds and the search descriptor ---
	// No page, because these are not a reader's route THROUGH the site: a feed
	// is read by a reader's client, and the descriptor by a browser's search
	// settings. The page half exists and is tested where it lives — the tag page
	// discovers its feed with rel=alternate and links it in words
	// (TestTagPageOffersItsFeed).
	"GET /feed/tag/{id}": {Why: "read by a subscriber's client; /tag/{id} carries " +
		"the discovery link and the words"},
	"GET /feed/seed/{kind}/{id}": {Why: "read by a subscriber's client; /work/{id} " +
		"is the page it ranks from"},
	"GET /opensearch.xml": {Why: "read by a browser's search-engine settings, " +
		"not by a reader"},

	// --- reached by a page ---
	"GET /api/v1/recommend":                {Page: "/recommend"},
	"GET /api/v1/tags/{id}/similar":        {Page: "/neighbours"},
	"GET /api/v1/authors/similar":          {Page: "/author"},
	"GET /api/v1/ao3/works":                {Page: "/search"},
	"GET /api/v1/ao3/works/{id}":           {Page: "/work/{id}"},
	"GET /api/v1/ao3/works/{id}/recommend": {Page: "/work/{id}"},
	"GET /api/v1/works/{id}/co-bookmarked": {Page: "/work/{id}"},
	"GET /api/v1/works/{id}/bridges":       {Page: "/work/{id}"},
	// Work-pair similarity (#78). The page is the work page's compare form,
	// which renders the report in place under ?compare={other}.
	"GET /api/v1/works/{id}/similarity/{other}": {Page: "/work/{id}?compare={other}"},
	// A reading path (#40) has its own page: a walk is ten embedding queries and
	// the work page must not pay for it.
	"GET /api/v1/works/{id}/path": {Page: "/path/{id}"},
	// The algorithm A/B surface. An algorithm is a named tune; the page
	// shows one seed ranked through two of them, side by side, and records
	// which list the reader preferred.
	"GET /api/v1/algo/list":      {Page: "/algo"},
	"GET /api/v1/algo/compare":   {Page: "/algo"},
	"POST /api/v1/algo/judge":    {Page: "/algo"},
	"GET /api/v1/algo/standings": {Page: "/algo"},
	// The per-kind algorithm registry and the list it produces. /choose is the
	// picker (it POSTs the choice), /related renders what one algorithm returns;
	// the two API routes are those pages seen through their other face.
	"GET /api/v1/algo/kinds":   {Page: "/choose"},
	"POST /api/v1/algo/choice": {Page: "/choose"},
	"GET /api/v1/related":      {Page: "/related"},
	// The merged-list experiment. Both routes are the /algo page seen
	// through its other mode: the page renders the merged list and records
	// the impressions, and the standings are the same table the page prints.
	"GET /api/v1/interleave":             {Page: "/algo?mode=interleave"},
	"GET /api/v1/interleave/standings":   {Page: "/algo?mode=interleave"},
	"GET /api/v1/ao3/tags":               {Page: "/search"},
	"GET /api/v1/ao3/tags/{id}":          {Page: "/tag/{id}"},
	"GET /api/v1/ao3/tags/{id}/works":    {Page: "/tag/{id}"},
	"GET /api/v1/corpus-query/{mode}":    {Page: "/fandoms | /underrated | /neighbours | /surprise"},
	"GET /api/v1/arena/pair":             {Page: "/arena"},
	"POST /api/v1/arena/compare":         {Page: "/arena"},
	"GET /api/v1/arena/leaderboard":      {Page: "/leaderboard"},
	"GET /api/v1/arena/rank/{id}":        {Page: "/rank/{id}"},
	"GET /api/v1/arena/rank/{kind}/{id}": {Why: "the page is the frontend app's route, not this repo's; this repo owes the API, and no page exists for it yet"},
	"GET /api/v1/arena/my-ranking":       {Page: "/my-ranking"},
	"GET /api/v1/crawl-queue":            {Page: "/crawl"},

	// --- deliberately no page ---
	"POST /api/v1/recommend": {
		Why: "the seed form on /recommend is a GET; this variant exists for seed " +
			"lists too long for a URL, which no browser form can produce",
	},
	"POST /api/v1/arena/batch": {
		Why: "bulk judgement import. /arena submits ONE comparison at a time, " +
			"which is what a reader does; a batch is a restore or bulk-load tool",
	},
	"GET /healthz": {
		Why: "a liveness probe for machines. /mirror links it for an operator " +
			"who wants it, and renders none of it",
	},
	"GET /stats": {
		Why: "machine-readable. /mirror is the reader's page for the same facts, " +
			"which is why the nav no longer links this",
	},
	"GET /api/v1/stats": {
		Why: "the same handler under the versioned prefix, for callers",
	},
	"/": {
		Why: "the catch-all mount for the PAGES handler, not an API route",
	},
}

// routesInSource extracts every HandleFunc pattern registered in server.go.
func routesInSource(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing server.go: %v", err)
	}

	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// A pattern built at runtime cannot be audited here; say so rather
			// than skipping it silently, because an unparsed-but-registered
			// route is precisely the gap this file exists to close.
			t.Errorf("HandleFunc at %s takes a non-literal pattern, which cannot be audited",
				fset.Position(call.Pos()))
			return true
		}
		p, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("unquoting %s: %v", lit.Value, err)
			return true
		}
		found = append(found, p)
		return true
	})

	if len(found) < 15 {
		t.Fatalf("parsed only %d routes from server.go; the parser or the table has moved", len(found))
	}
	return found
}

func TestEveryAPIRouteIsAccountedFor(t *testing.T) {
	// The direction that matters: a route nobody recorded. Adding an endpoint
	// and forgetting this file is the whole failure mode.
	for _, pattern := range routesInSource(t) {
		if _, ok := routeTable[pattern]; !ok {
			t.Errorf("route %q is registered but not accounted for: give it a page or record why it has none", pattern)
		}
	}
}

func TestTheRouteAuditHasNoStaleEntries(t *testing.T) {
	// The other direction. An entry for a route that no longer exists is a
	// table describing a server that is not running, and it would hide the
	// removal of a capability: the audit would still say the route has a page.
	// The AST is not sorted and has duplicates, so compare as a set.
	live := map[string]bool{}
	for _, p := range routesInSource(t) {
		live[p] = true
	}
	for pattern := range routeTable {
		if !live[pattern] {
			t.Errorf("the audit lists %q, which server.go no longer registers", pattern)
		}
	}
}

func TestEveryRouteEntrySaysExactlyOneThing(t *testing.T) {
	// A row with neither field is an entry that records nothing, and a row with
	// both is a route whose status is ambiguous -- the reader cannot tell
	// whether the page is the answer or the reason for its absence.
	for pattern, e := range routeTable {
		switch {
		case e.Page == "" && e.Why == "":
			t.Errorf("%q records neither a page nor a reason", pattern)
		case e.Page != "" && e.Why != "":
			t.Errorf("%q records both a page (%s) and a reason; exactly one applies", pattern, e.Page)
		}
	}
}
