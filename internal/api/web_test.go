package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests cover the HTML frontend that wraps the API. The one thing
// that must not regress is the boundary: a browser gets HTML, and every
// existing JSON client gets exactly the JSON it got before, including its
// own 404.

// getText fetches a page as text. The existing get() helper decodes JSON
// and fatals on anything else, which is the right behaviour for an API
// test and the wrong behaviour for a page test.
func getText(t *testing.T, ts *httptest.Server, path string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

func TestRootServesHTMLNotJSON(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/")
	if status != http.StatusOK {
		t.Fatalf("GET / -> %d, want 200", status)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("GET / Content-Type is %q, want text/html", ct)
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "<!doctype html>") {
		t.Errorf("GET / does not start with a doctype; first 80 bytes: %.80q", body)
	}
}

// TestAPI404StaysJSON is the regression guard for the whole wrapping
// design. The web layer hands /api/ back to the API handler, and if that
// ever stops happening a JSON client gets HTML where it expected a
// message -- a parse error with no explanation.
func TestAPI404StaysJSON(t *testing.T) {
	ts := newTestServer(t)
	status, body := get(t, ts, "/api/v1/nonexistent")
	if status != http.StatusNotFound {
		t.Errorf("status %d, want 404", status)
	}
	if body["error"] == nil {
		t.Error("the API 404 must carry an error field, as it always has")
	}
}

func TestAPIRoutesUnaffected(t *testing.T) {
	ts := newTestServer(t)
	// Every one of these answered before the frontend existed. If the
	// wrapper swallowed any of them, this is where it shows.
	for _, path := range []string{
		"/healthz",
		"/stats",
		"/api/v1/stats",
		"/api/v1/ao3/works",
		"/api/v1/ao3/tags",
		"/api/v1/tags/1/similar",
	} {
		t.Run(path, func(t *testing.T) {
			status, body := get(t, ts, path)
			if status == http.StatusNotFound {
				t.Errorf("GET %s -> 404; the web layer ate an API route", path)
			}
			_ = body
		})
	}
}

func TestWorkPageRenders(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/work/1")
	if status != http.StatusOK {
		t.Fatalf("GET /work/1 -> %d, want 200", status)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
	if !strings.Contains(body, "Work 1") {
		t.Errorf("the work page does not contain the work's title:\n%.400s", body)
	}
	// The title must also be in the <title> element, not only the body:
	// a page that renders the work but names the tab "kindred" is wrong.
	if !strings.Contains(body, "<title>Work 1") {
		t.Error("the page title does not name the work")
	}
	// The shared layout must be on every page.
	if !strings.Contains(body, "/static/style.css") {
		t.Error("the page does not link the stylesheet")
	}
}

func TestTagPageRenders(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/tag/1")
	if status != http.StatusOK {
		t.Fatalf("GET /tag/1 -> %d, want 200", status)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
	if !strings.Contains(body, "tag1") {
		t.Errorf("the tag page does not name the tag:\n%.400s", body)
	}
	if !strings.Contains(body, "/work/") {
		t.Error("the tag page lists no works, but the fixture has some")
	}
}

func TestSearchPageRenders(t *testing.T) {
	ts := newTestServer(t)
	status, _, body := getText(t, ts, "/search?q=tag1")
	if status != http.StatusOK {
		t.Fatalf("GET /search -> %d, want 200", status)
	}
	if !strings.Contains(body, "tag1") {
		t.Errorf("the search page did not list the matching tag:\n%.400s", body)
	}
}

func TestSearchWithNoMatchIsStillAPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/search?q=zzzznothing")
	if status != http.StatusOK {
		t.Fatalf("a search with no results should be 200, got %d", status)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html even with no results", ct)
	}
	if !strings.Contains(body, "Nothing matches") {
		t.Errorf("no-results search does not say so:\n%.400s", body)
	}
}

func TestStaticCSSAndJS(t *testing.T) {
	ts := newTestServer(t)

	status, ct, body := getText(t, ts, "/static/style.css")
	if status != http.StatusOK {
		t.Fatalf("GET /static/style.css -> %d", status)
	}
	if !strings.Contains(ct, "text/css") {
		t.Errorf("CSS Content-Type is %q", ct)
	}
	// #900 is the AO3 link colour and the single value that decides
	// whether this looks like AO3. Assert it so a rewrite cannot quietly
	// turn the theme blue.
	if !strings.Contains(body, "#900") {
		t.Error("the stylesheet has lost the #900 link colour")
	}

	status, ct, _ = getText(t, ts, "/static/app.js")
	if status != http.StatusOK {
		t.Fatalf("GET /static/app.js -> %d", status)
	}
	if !strings.Contains(ct, "javascript") {
		t.Errorf("JS Content-Type is %q", ct)
	}
}

func TestStaticRefusesAnythingElse(t *testing.T) {
	ts := newTestServer(t)
	// The embedded filesystem holds only two files. Serving the directory
	// would let a request walk it, and serving an arbitrary name would
	// mean the switch above is the only thing deciding.
	for _, path := range []string{
		"/static/", "/static/embed.go", "/static/../embed.go",
	} {
		if status, _, _ := getText(t, ts, path); status != http.StatusNotFound {
			t.Errorf("GET %s -> %d, want 404", path, status)
		}
	}
}

func TestUnknownBrowserPathIsHTML404(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/nope")
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
	// A person who mistypes a URL gets a page, not JSON.
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q; a browser 404 should be HTML", ct)
	}
	if !strings.Contains(body, "<!doctype html>") {
		t.Error("the 404 page is not a document")
	}
}

func TestWorkPageEscapesHostileText(t *testing.T) {
	// A work titled <script>alert(1)</script> is a real title in a real
	// corpus. html/template is chosen for exactly this; the test is the
	// only thing that proves the right package is in use, because
	// text/template would also produce valid output, just executable.
	ts := newTestServer(t)
	const hostile = `<script>alert("xss")</script>`

	// Reach the escaping through the search page, which is the one page
	// that echoes the query verbatim.
	_, _, body := getText(t, ts, "/search?q="+hostile)
	if strings.Contains(body, hostile) {
		t.Error("the search query was echoed unescaped; html/template is not doing its job")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected the hostile query escaped in the output:\n%.400s", body)
	}
}

func TestPagesRejectNonGET(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/work/1", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /work/1 -> %d, want 405; pages must not accept writes", resp.StatusCode)
	}
}

func TestMultiSeedRecommendPage(t *testing.T) {
	ts := newTestServer(t)
	// The page form of the recommender: two seeds, ranked together. This
	// is the path the JS "seed from this" button submits to.
	status, ct, body := getText(t, ts, "/recommend?seed=ao3_work:1&seed=ao3_work:2")
	if status != http.StatusOK {
		t.Fatalf("GET /recommend -> %d, want 200\n%.400s", status, body)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
}

// The API has its own malformed-seed test (server_test.go). This one
// covers the page form, which parses seeds itself rather than delegating
// to the API handler -- so a bug in the page's parser would not be caught
// by the API's test.
func TestRecommendPageRejectsAMalformedSeed(t *testing.T) {
	ts := newTestServer(t)
	status, _, body := getText(t, ts, "/recommend?seed=nonsense")
	if status != http.StatusBadRequest {
		t.Errorf("a seed without a colon -> %d, want 400", status)
	}
	if !strings.Contains(body, "ao3_work:") {
		t.Errorf("the error page does not show the expected form:\n%.300s", body)
	}
}
