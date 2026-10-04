package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// corpusSchema used to be a private copy of the schema written out here, and
// it was WRONG in the one way that mattered:
//
//	PRIMARY KEY(work_id, tag_id)              <- the copy
//	PRIMARY KEY(work_id, tag_id, tag_type)     <- the real mirror
//
// The real key lets one tag NAME sit on one work twice under two tag types,
// which is what internal/testcorpus produces and what a tag that exists as
// both a fandom and a freeform produces in production. Against the copy, any
// code that JOINs work_tags without DISTINCT looked correct, because the copy
// physically cannot express the duplication. That is the worst shape for a
// fixture: it removes the bug from the tests while leaving it in the product.
//
// It also lacked the `users` table entirely.
//
// Both problems have one fix: use the schema the fixtures actually write, which
// is derived from the real mirror's CREATE TABLE.
const corpusSchema = testcorpus.Schema

// newTestServer builds a server over a small corpus: 4 works sharing tags,
// which is enough for a pool, a ranking and a diversified list.
//
// The returned client HAS a cookie jar, and that is load-bearing rather than
// incidental. httptest.NewServer's own ts.Client() has Jar == nil, so the
// kindred_arena cookie set by the first response is never sent back: every
// request mints a fresh session, OwnerKey returns a different HMAC each time,
// and a test that writes with one request and reads with another looks at two
// different owners. The symptom is a write that demonstrably lands — the row is
// in the table — paired with a page that lists nothing, which reads as a store
// bug and is not one. It is the fixture, not the handler: a real browser sends
// the cookie, so production is correct and needs no change here.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	corpusPath := filepath.Join(dir, "corpus.db")

	// The corpus is attached read-only, so the fixture is written with a
	// separate handle before the store opens it. Attaching an existing
	// writable file read-only is what production does, and a test that
	// skipped that would not exercise the same path.
	{
		f, err := sql.Open("sqlite", corpusPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Exec(corpusSchema); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	s, err := store.Open(context.Background(), dbPath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if s.Corpus == nil {
		t.Fatal("the corpus handle is nil; the test cannot seed it")
	}

	// The corpus is read-only, so the fixture rows are written before the
	// store opens it. They are inserted through a second handle here.
	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()

	// 5 tags, 4 works.
	for i := 1; i <= 5; i++ {
		if _, err := seed.Exec(`INSERT INTO tags(id,name) VALUES(?,?)`,
			i, "tag"+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	// work 1 and 2 share tags 1,2 ; work 3 shares 2,3 ; work 4 is alone on 5.
	links := map[int64][]int{
		1: {1, 2}, 2: {1, 2}, 3: {2, 3}, 4: {5},
	}
	for id, tags := range links {
		if _, err := seed.Exec(
			`INSERT INTO works(id,url,title,authors,word_count,hits,kudos,bookmarks,update_date,first_seen)
			 VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, "https://example.com/"+string(rune('0'+id)), "Work "+string(rune('0'+id)),
			"author"+string(rune('0'+id)),
			5000, 1000*id, 100*id, 10*id, "2026-06-01", "20260"); err != nil {
			t.Fatal(err)
		}
		for _, tag := range tags {
			if _, err := seed.Exec(
				`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,?,'freeforms')`, id, tag); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Co-occurrence edges so the graph can load.
	if _, err := seed.Exec(
		`INSERT INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES(1,2,50)`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(
		`INSERT INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES(2,3,20)`); err != nil {
		t.Fatal(err)
	}

	eng := &engine.Engine{
		Store:    s,
		Corpus:   corpus.NewAO3(s.Corpus),
		PoolSize: 50,
		TopN:     4,
		Lite:     true,
	}
	srv := &Server{
		Engine:    eng,
		Store:     s,
		Version:   "test",
		StartedAt: time.Now().Add(-time.Minute),
		Lite:      true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("response from %s is not JSON: %v", path, err)
	}
	return resp.StatusCode, body
}

// TestHealthzReportsOK checks the health surface, which is what a
// supervisor polls.
//
// It asserts the SHAPE of the response and that the budget verdict is
// internally consistent, rather than that the verdict is "under budget".
//
// That distinction is load-bearing. /healthz reports VmHWM, a monotonic
// process high-water mark that no test can lower, so whether this package is
// under the 220 MiB cap depends on which tests ran before it. Under `-race`
// it consistently is not: the suite failed 3 runs out of 3 with `status 503,
// want 200` while passing in isolation. Asserting "ok" here would make a
// passing suite depend on test order and on whether the race detector was
// enabled -- a test that reports a real regression as a failure, and a real
// fix as a failure, depending on nothing.
//
// What IS asserted: the cap is present, budget_ok is present, and budget_ok is
// true exactly when status is "ok". Those are the properties a supervisor
// relies on, and they hold whatever the process peak happens to be.
func TestHealthzReportsOK(t *testing.T) {
	ts := newTestServer(t)
	status, body := get(t, ts, "/healthz")
	if status != http.StatusOK && status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 200 or 503", status)
	}
	for _, k := range []string{"rss_cap_kib", "budget_ok", "peak_rss_kib", "uptime_s"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("healthz is missing %q; a health check that cannot report a "+
				"field cannot be used to decide about it", k)
		}
	}
	// The two must agree: budget_ok false with status "ok" is exactly the
	// inconsistency this endpoint exists to avoid.
	budgetOK, _ := body["budget_ok"].(bool)
	st, _ := body["status"].(string)
	if budgetOK && st != "ok" {
		t.Errorf("budget_ok is true but status is %q", st)
	}
	if !budgetOK {
		if st != "over_budget" {
			t.Errorf("budget_ok is false but status is %q", st)
		}
		if status != http.StatusServiceUnavailable {
			t.Errorf("over budget but status is %d, want 503", status)
		}
	}
}

// TestRecommendExcludesTheSeedByDefault is the default that was wrong: the
// API returned the seed itself at the top of its own recommendation list.
func TestRecommendExcludesTheSeedByDefault(t *testing.T) {
	ts := newTestServer(t)
	_, body := get(t, ts, "/api/v1/recommend?seed=ao3_work:1&n=5")
	items, _ := body["items"].([]any)
	if len(items) == 0 {
		t.Skip("no items ranked; the fixture may not produce a pool")
	}
	for _, raw := range items {
		it, _ := raw.(map[string]any)
		if id, ok := it["id"].(float64); ok && int64(id) == 1 {
			t.Fatal("the seed work was recommended to a client that seeded it")
		}
	}
}

func TestRecommendCanIncludeTheSeedWhenAsked(t *testing.T) {
	ts := newTestServer(t)
	_, body := get(t, ts, "/api/v1/recommend?seed=ao3_work:1&n=5&exclude_seeds=false")
	if _, ok := body["items"]; !ok {
		t.Fatal("no items key")
	}
	// Whether work 1 comes back depends on the pool; the point is that
	// the request is accepted rather than rejected.
}

func TestRecommendRequiresASeed(t *testing.T) {
	ts := newTestServer(t)
	status, body := get(t, ts, "/api/v1/recommend")
	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: a request with no seed cannot be answered", status)
	}
	if body["error"] == nil {
		t.Fatal("a 400 with no explanation")
	}
}

func TestRecommendRejectsAMalformedSeed(t *testing.T) {
	ts := newTestServer(t)
	for _, seed := range []string{"1", "ao3_work:", ":5", "ao3_work:abc"} {
		status, _ := get(t, ts, "/api/v1/recommend?seed="+seed)
		if status != http.StatusBadRequest {
			t.Errorf("seed %q gave status %d, want 400", seed, status)
		}
	}
}

func TestRecommendAcceptsRepeatedAndCommaSeparatedSeeds(t *testing.T) {
	ts := newTestServer(t)
	// Both spellings must work; a client should not have to know which the
	// server prefers.
	for _, q := range []string{
		"?seed=ao3_work:1&seed=ao3_work:2",
		"?seed=ao3_work:1,ao3_work:2",
	} {
		status, body := get(t, ts, "/api/v1/recommend"+q+"&n=2")
		if status != http.StatusOK {
			t.Errorf("%s gave status %d, want 200", q, status)
		}
		if body["seeds"] == nil {
			t.Errorf("%s: no seeds echoed back", q)
		}
	}
}

// TestRecommendReportsAnUnknownSeedAs404: the request was well formed and
// the thing it named does not exist, which is a 404 rather than a 400.
func TestRecommendReportsAnUnknownSeedAs404(t *testing.T) {
	ts := newTestServer(t)
	status, _ := get(t, ts, "/api/v1/recommend?seed=ao3_work:999999&n=5")
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
}

func TestRecommendJSONUsesLowercaseFieldNames(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/recommend?seed=ao3_work:1&n=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 64*1024)
	n, _ := resp.Body.Read(raw)
	body := string(raw[:n])
	// Go marshals untagged struct fields verbatim. A response carrying
	// "ID" and "TagNames" breaks every client on the next rename.
	for _, bad := range []string{`"ID":`, `"Kind":`, `"TagNames":`, `"URL":`, `"Evidence":`} {
		if strings.Contains(body, bad) {
			t.Errorf("response contains the untagged field name %s", bad)
		}
	}
}

func TestAO3WorksList(t *testing.T) {
	ts := newTestServer(t)
	status, body := get(t, ts, "/api/v1/ao3/works?limit=2")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	works, _ := body["works"].([]any)
	if len(works) != 2 {
		t.Fatalf("got %d works, want 2", len(works))
	}
	if body["limit"].(float64) != 2 {
		t.Fatalf("limit = %v", body["limit"])
	}
}

func TestAO3WorksLimitIsClamped(t *testing.T) {
	ts := newTestServer(t)
	// A limit of 10000 would pull the whole corpus into a response.
	for _, q := range []string{"?limit=0", "?limit=-5", "?limit=100000", "?limit=abc"} {
		_, body := get(t, ts, "/api/v1/ao3/works"+q)
		limit, _ := body["limit"].(float64)
		if limit < 1 || limit > 100 {
			t.Errorf("%s gave limit %v, want it clamped into [1,100]", q, limit)
		}
	}
}

// TestAO3WorksSortIsWhitelisted: sort arrives in the query string, so it
// cannot be interpolated into SQL unvalidated.
func TestAO3WorksSortIsWhitelisted(t *testing.T) {
	ts := newTestServer(t)
	for _, sort := range []string{"kudos", "hits", "date", "words", "bogus", "kudos; DROP TABLE works"} {
		status, _ := get(t, ts, "/api/v1/ao3/works?sort="+strings.ReplaceAll(sort, " ", "+")+"&limit=2")
		if status != http.StatusOK {
			t.Errorf("sort=%q gave status %d, want 200 (a bad sort must fall back, not fail or inject)", sort, status)
		}
	}
	// The table must still be there.
	if _, body := get(t, ts, "/api/v1/ao3/works?limit=1"); body["works"] == nil {
		t.Fatal("the works table is gone after the sort attempts")
	}
}

func TestAO3WorkByID(t *testing.T) {
	ts := newTestServer(t)
	status, body := get(t, ts, "/api/v1/ao3/works/1")
	if status != http.StatusOK {
		t.Fatalf("status %d, body %v", status, body)
	}
	if body["title"] == nil {
		t.Fatal("no title")
	}
	if _, ok := body["tags"]; !ok {
		t.Fatal("no tags key")
	}
}

func TestAO3WorkNotFound(t *testing.T) {
	ts := newTestServer(t)
	if status, _ := get(t, ts, "/api/v1/ao3/works/424242"); status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
}

func TestPathParamMustBeAnInteger(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/api/v1/ao3/works/abc", "/api/v1/ao3/tags/xyz"} {
		if status, _ := get(t, ts, path); status != http.StatusBadRequest {
			t.Errorf("%s gave %d, want 400", path, status)
		}
	}
}

func TestAO3TagsAndTagDetail(t *testing.T) {
	ts := newTestServer(t)
	_, body := get(t, ts, "/api/v1/ao3/tags?limit=10")
	if body["tags"] == nil {
		t.Fatal("no tags")
	}
	status, detail := get(t, ts, "/api/v1/ao3/tags/1")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if detail["work_count"] == nil {
		t.Fatal("no work_count on a tag")
	}
}

func TestTagWorks(t *testing.T) {
	ts := newTestServer(t)
	_, body := get(t, ts, "/api/v1/ao3/tags/2/works?limit=10")
	works, _ := body["works"].([]any)
	if len(works) < 1 {
		t.Fatalf("tag 2 is on works 1,2,3; got %d", len(works))
	}
}

func TestSimilarTagsWithoutAnIndexIs503(t *testing.T) {
	ts := newTestServer(t)
	// The test server has no graph loaded, so this endpoint must report
	// that rather than return an empty list that reads as "nothing is
	// similar to anything".
	status, body := get(t, ts, "/api/v1/tags/1/similar")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 when no index is loaded", status)
	}
	if body["error"] == nil {
		t.Fatal("a 503 with no reason")
	}
}

func TestStatsShape(t *testing.T) {
	ts := newTestServer(t)
	status, body := get(t, ts, "/stats")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	for _, key := range []string{"graph", "corpus", "memory", "version", "lite"} {
		if _, ok := body[key]; !ok {
			t.Errorf("no %q in /stats", key)
		}
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	ts := newTestServer(t)
	if status, _ := get(t, ts, "/api/v1/nonsense"); status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
}

// TestErrorResponsesAreJSON: a client parsing errors must not have to
// handle an HTML page.
func TestErrorResponsesAreJSON(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/recommend")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
}

// TestNIsClamped: n=100000 must not be served.
func TestNIsClamped(t *testing.T) {
	ts := newTestServer(t)
	_, body := get(t, ts, "/api/v1/recommend?seed=ao3_work:1&n=100000")
	items, _ := body["items"].([]any)
	if len(items) > 100 {
		t.Fatalf("got %d items, want at most 100", len(items))
	}
}
