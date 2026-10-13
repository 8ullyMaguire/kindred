package web

// W2: the router (`route`), the API/page split (`guardAPI`), the freshness
// middleware and the static handler. These decide *which* branch a request
// reaches and what status it gets back, and none of them was tested.
//
// They are deliberately tested through http.Handler values with a zero Deps
// (nil Engine). That is the point: nothing here should need a corpus to know
// whether a POST to /search is a 405 or whether /api/v1 reaches the API.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// markerHandler answers a fixed status and records that it was reached.
type markerHandler struct {
	called bool
	status int
	body   string
}

func (m *markerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.called = true
	if m.status == 0 {
		m.status = http.StatusOK
	}
	w.WriteHeader(m.status)
	_, _ = w.Write([]byte(m.body))
}

func TestGuardAPISendsTheAPIPathsToTheAPIHandler(t *testing.T) {
	// SPEC: an unmatched /api/v1/ request must reach the API so a JSON client
	// still gets the API's JSON 404, not an HTML page it cannot parse.
	for _, path := range []string{
		"/api/", "/api/v1/", "/api/v1/recommend", "/api/v1/nonexistent",
		"/healthz", "/stats",
	} {
		api, pages := &markerHandler{}, &markerHandler{}
		d := Deps{}
		h := d.guardAPI(api, pages)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if !api.called || pages.called {
			t.Errorf("guardAPI(%q): api.called=%v pages.called=%v; want the API", path, api.called, pages.called)
		}
	}
}

func TestGuardAPISendsEverythingElseToThePages(t *testing.T) {
	for _, path := range []string{"/", "/search", "/work/1", "/arena/judge", "/static/style.css"} {
		api, pages := &markerHandler{}, &markerHandler{}
		d := Deps{}
		h := d.guardAPI(api, pages)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if api.called || !pages.called {
			t.Errorf("guardAPI(%q): api.called=%v pages.called=%v; want the pages", path, api.called, pages.called)
		}
	}
}

func TestGuardAPIDoesNotMatchABareApiPath(t *testing.T) {
	// The match is HasPrefix "/api/", not "/api". A bare "/api" is not an API
	// route, and widening the test to "/api" would silently start sending a
	// page URL to the JSON handler.
	api, pages := &markerHandler{}, &markerHandler{}
	d := Deps{}
	rec := httptest.NewRecorder()
	d.guardAPI(api, pages).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api", nil))

	if api.called {
		t.Error(`guardAPI("/api") reached the API handler; want the pages branch`)
	}
	if !pages.called {
		t.Error(`guardAPI("/api") did not reach the pages branch`)
	}
}

func TestFreshnessHeadersAreStampedOnEveryResponse(t *testing.T) {
	// SPEC §3.2.2: the index age goes on EVERY response. The middleware is on
	// the outermost handler precisely so this holds for every branch and every
	// status, so the stub below returns both a success and a not-found.
	//
	// Note this drives withFreshnessHeaders DIRECTLY rather than through
	// Pages() with a page path: Pages() routes a page path into `route` and
	// thence into a real renderer, which needs a non-nil Engine. Testing the
	// middleware through a renderer would be testing the renderer.
	d := Deps{Version: "test-version"}

	for _, tc := range []struct {
		name   string
		status int
	}{
		{"a success", http.StatusOK},
		{"a not-found", http.StatusNotFound},
		{"a server error", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := withFreshnessHeaders(d, &markerHandler{status: tc.status})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anywhere", nil))

			if rec.Code != tc.status {
				t.Errorf("status = %d; want %d passed through untouched", rec.Code, tc.status)
			}
			for _, hdr := range []string{
				"X-Kindred-Index-Age",
				"X-Kindred-Index-Age-Seconds",
				"X-Kindred-Index-Version",
			} {
				if got := rec.Header().Get(hdr); got == "" {
					t.Errorf("%s is missing; SPEC says every response", hdr)
				}
			}
			// SPEC §3.2.3: identify as kindred, and do not disclose a stack.
			if got := rec.Header().Get("Server"); got != "kindred" {
				t.Errorf("Server = %q; want \"kindred\"", got)
			}
			if got, ok := rec.Header()["X-Powered-By"]; !ok {
				// Cleared rather than merely not set: assert it is explicitly
				// empty, because "absent" is what a naive implementation does
				// by accident and this one claims removal.
				t.Error("X-Powered-By is absent; want it explicitly cleared to \"\"")
			} else if len(got) > 0 && got[0] != "" {
				t.Errorf("X-Powered-By = %q; want empty", got)
			}
		})
	}
}

func TestPagesSendsAnUnmatchedAPIPathToTheAPIHandler(t *testing.T) {
	// Pages() is mounted in front of the API, so the risk it has to survive is
	// swallowing a JSON client's 404 and answering HTML. Drive it the way the
	// server does and assert the /api/ branch is the one that ran.
	api := &markerHandler{status: http.StatusNotFound, body: `{"error":"not found"}`}
	d := Deps{Version: "test-version"}
	rec := httptest.NewRecorder()
	d.Pages(api).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nonexistent", nil))

	if !api.called {
		t.Fatal("Pages() did not pass /api/v1/nonexistent to the API handler")
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q; an API path must not come back as HTML", ct)
	}
}

func TestRouteRejectsAPostToAReadOnlyPage(t *testing.T) {
	// Only /profiles and /profile accept a POST, plus the three one-button
	// forms handled before this gate. Everything else must be a 405 that names
	// the allowed methods.
	for _, path := range []string{"/search", "/arena", "/fandoms", "/leaderboard", "/work/1"} {
		rec := httptest.NewRecorder()
		Deps{}.route(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d; want 405", path, rec.Code)
		}
	}
}

func TestRouteRejectsMethodsThatAreNotPageMethods(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		Deps{}.route(rec, httptest.NewRequest(method, "/search", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /search = %d; want 405", method, rec.Code)
		}
	}
}

func TestRouteUnknownPathIsNotFound(t *testing.T) {
	for _, path := range []string{"/nope", "/definitely/not/a/page", "/api/v2/thing"} {
		rec := httptest.NewRecorder()
		Deps{}.route(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d; want 404", path, rec.Code)
		}
		// The 404 must be HTML (it is a page) and must not be a 403: a status
		// that varies with existence makes the site enumerable.
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q; want text/html", path, ct)
		}
	}
}

func TestStaticServesTheStylesheetAndNothingElse(t *testing.T) {
	h := http.StripPrefix("/static/", Static())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/style.css", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /static/style.css = %d; want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("style.css Content-Type = %q; want text/css", ct)
	}

	// Exactly one file is reachable, and the point is that the *other* files
	// in the embedded FS are the server-side templates. A missing file would
	// 404 with or without the guard, so the cases below name templates that
	// really exist: they are the ones that would be served as raw source if
	// the guard were removed. Probed first (they are in internal/web/assets/),
	// because a test that requests absent files cannot detect the removal.
	for _, path := range []string{
		"/static/", "/static/index.html",
		"/static/layout.html", "/static/work.html", "/static/notfound.html",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d; want 404 (this file is in the embedded FS and must not be served)", path, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "{{") {
			t.Errorf("GET %s returned template source: %q", path, firstLine(body))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
