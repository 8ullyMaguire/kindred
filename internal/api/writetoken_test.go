package api

// The write guard's own tests. Three claims:
//
//  1. Every mutating route refuses a caller who presents no token, or the wrong
//     one, and accepts the right one.
//  2. A deployment with no token at all refuses writes with a 503 that names
//     the variable, rather than accepting them or answering a bare 401.
//  3. Reads are unaffected, including the POST that only reads.
//
// Plus the structural claim the audit-style tests in this package make: the
// route list in writetoken.go is checked against server.go with go/ast, so a
// new mutating route cannot be registered without being guarded.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// testWriteToken is the token every fixture server is configured with.
const testWriteToken = "test-write-token"

// writeJSONRequest performs a mutating request, with the token when one is
// given. Empty token means "present nothing", which is the interesting case.
func writeJSONRequest(t *testing.T, method, url, body, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(WriteTokenHeader, token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// stampWriteToken puts the fixture token on a request a test built itself.
// Every mutating API call in this package needs it; the page forms do not,
// because the pages write through the web layer, not through these routes.
func stampWriteToken(req *http.Request) *http.Request {
	req.Header.Set(WriteTokenHeader, testWriteToken)
	return req
}

// postFormWithToken is client.PostForm plus the fixture token.
func postFormWithToken(t *testing.T, c *http.Client, url string, form url.Values) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.Do(stampWriteToken(req))
}

func bodyOf(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEveryMutatingRouteRefusesAnUntokenedCaller(t *testing.T) {
	ts, _ := newTestServerFull(t)
	for pattern := range mutatingRoutes {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("malformed route %q", pattern)
		}
		// A body the handler would accept if it got that far: the point is
		// that the guard refuses before the handler reads it.
		body := `{"seed":"ao3_work:1","a":"default","b":"default","choice":"a","tags":[]}`

		res := writeJSONRequest(t, method, ts.URL+path, body, "")
		got := bodyOf(t, res)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with no token = %d, want 401 (%s)", pattern, res.StatusCode, got)
		}

		res = writeJSONRequest(t, method, ts.URL+path, body, "not-the-token")
		got = bodyOf(t, res)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with the wrong token = %d, want 401 (%s)", pattern, res.StatusCode, got)
		}

		res = writeJSONRequest(t, method, ts.URL+path, body, testWriteToken)
		got = bodyOf(t, res)
		// The guard lets it through; what the handler then says is its own
		// business (a taste import with no session is a 401 from the handler,
		// for instance). The assertion is that the refusal is not the guard's.
		if res.StatusCode == http.StatusServiceUnavailable || strings.Contains(got, "this route writes") {
			t.Errorf("%s with the right token was refused by the guard: %d (%s)",
				pattern, res.StatusCode, got)
		}
	}
}

func TestWritesAreOffWhenTheDeploymentNamesNoToken(t *testing.T) {
	ts, _ := newTestServerWithWriteToken(t, "")
	res := writeJSONRequest(t, http.MethodPut, ts.URL+"/api/v1/taste",
		`{"tags":[{"name":"tag1","weight":0.5}]}`, "")
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PUT with no token configured = %d, want 503 (%s)", res.StatusCode, body)
	}
	// The refusal has to name what the operator is missing. A bare 503 on a
	// deployment that thinks writes are on is a puzzle, not a diagnosis.
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("503 body is not JSON: %v (%s)", err, body)
	}
	msg, _ := doc["error"].(string)
	if !strings.Contains(msg, "KINDRED_WRITE_TOKEN") {
		t.Fatalf("the 503 does not name KINDRED_WRITE_TOKEN: %q", msg)
	}
	if doc["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("status field = %v, want 503", doc["status"])
	}

	// A token presented to a deployment that has none is still a refusal: the
	// deployment cannot know it is the right one.
	res = writeJSONRequest(t, http.MethodPut, ts.URL+"/api/v1/taste",
		`{"tags":[]}`, testWriteToken)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PUT with a token on an unconfigured deployment = %d, want 503", res.StatusCode)
	}
}

func TestReadsSurviveTheWriteGuard(t *testing.T) {
	ts, _ := newTestServerWithWriteToken(t, "")
	// A GET, unauthenticated, on a real read route.
	res, err := http.Get(ts.URL + "/api/v1/algo/list")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/algo/list = %d, want 200", res.StatusCode)
	}

	// The one POST that only reads. It decodes a body for the seeds, which is
	// why it is a POST; guarding it would refuse the recommender's own
	// question on a deployment that serves no writes.
	res = writeJSONRequest(t, http.MethodPost, ts.URL+"/api/v1/recommend",
		`{"seeds":["ao3_work:1"]}`, "")
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/recommend = %d, want 200 (%s)", res.StatusCode, body)
	}
}

func TestBearerSpellingIsAccepted(t *testing.T) {
	ts, _ := newTestServerFull(t)
	// /api/v1/arena/batch needs no session and no body, so the only thing that
	// can refuse it is the guard: a 200 here means the Bearer spelling was read.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/arena/batch", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testWriteToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Bearer token rejected: %d (%s)", res.StatusCode, body)
	}
}

// TestTheGuardCoversEveryRegisteredWriteRoute is the structural half. It reads
// server.go the same way routes_audit_test.go does, because a hand-maintained
// copy of the route list is the thing that drifts: it would keep passing while
// a new POST was registered unguarded.
func TestTheGuardCoversEveryRegisteredWriteRoute(t *testing.T) {
	seen := map[string]bool{}
	for _, pattern := range routesInSource(t) {
		seen[pattern] = true
		method, _, ok := strings.Cut(pattern, " ")
		if !ok {
			// The bare "/" is the pages mount / JSON 404 catch-all, and it
			// writes nothing; TestAnUnknownPostIsStillANotFound is what covers
			// the behaviour it actually has.
			if pattern != "/" {
				t.Errorf("route %q has no method, which this test cannot classify", pattern)
			}
			continue
		}
		if method == http.MethodGet || method == http.MethodHead {
			continue
		}
		if mutatingRoutes[pattern] || writeFreeRoutes[pattern] {
			continue
		}
		t.Errorf("route %q is registered with method %s but is neither guarded in "+
			"mutatingRoutes nor recorded as read-only in writeFreeRoutes", pattern, method)
	}
	for pattern := range mutatingRoutes {
		if !seen[pattern] {
			t.Errorf("mutatingRoutes lists %q, which server.go does not register", pattern)
		}
	}
	for pattern := range writeFreeRoutes {
		if !seen[pattern] {
			t.Errorf("writeFreeRoutes lists %q, which server.go does not register", pattern)
		}
	}
}

// A POST to a path that does not exist is a 404, not a 401. The guard is keyed
// on the exact registered pattern rather than on the method, and this is the
// behaviour that keying buys.
func TestAnUnknownPostIsStillANotFound(t *testing.T) {
	ts, _ := newTestServerWithWriteToken(t, "")
	res := writeJSONRequest(t, http.MethodPost, ts.URL+"/api/v1/nope", `{}`, "")
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /api/v1/nope = %d, want 404 (%s)", res.StatusCode, body)
	}
}
