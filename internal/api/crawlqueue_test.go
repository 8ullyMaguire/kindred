package api

// /api/v1/crawl-queue is the only view showing discovery working: everything
// else describes what the mirror HOLDS, and a mirror that is holding nothing
// new looks exactly like a mirror that is working.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func newQueueTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := &Server{
		Engine:    &engine.Engine{},
		Store:     st,
		Version:   "test",
		StartedAt: time.Now().Add(-time.Minute),
		Lite:      true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, st
}

func TestCrawlQueueEndpointReportsAnEmptyQueueAsZero(t *testing.T) {
	// A fresh mirror must read as "nothing queued", not as an error and not as
	// a missing field a client has to special-case.
	ts, _ := newQueueTestServer(t)
	status, body := get(t, ts, "/api/v1/crawl-queue")
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (%v)", status, body)
	}
	for _, k := range []string{"pending", "done", "parked"} {
		if v, ok := body[k]; !ok || v.(float64) != 0 {
			t.Errorf("%s = %v; want 0", k, v)
		}
	}
	if rows, ok := body["recent"].([]any); !ok || len(rows) != 0 {
		t.Errorf("recent = %v; want an empty list (not null)", body["recent"])
	}
}

func TestCrawlQueueEndpointSeparatesTheThreeStates(t *testing.T) {
	ts, st := newQueueTestServer(t)
	ctx := context.Background()

	if err := st.EnqueueCrawl(ctx, "https://a/works/1", "discover"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueCrawl(ctx, "https://a/works/2", "reader-requested"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueCrawl(ctx, "https://a/works/3", "discover"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishCrawlJob(ctx, "https://a/works/3", ""); err != nil {
		t.Fatal(err)
	}

	status, body := get(t, ts, "/api/v1/crawl-queue")
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (%v)", status, body)
	}
	if got := body["pending"].(float64); got != 2 {
		t.Errorf("pending = %v; want 2", got)
	}
	if got := body["done"].(float64); got != 1 {
		t.Errorf("done = %v; want 1", got)
	}
	if got := body["parked"].(float64); got != 0 {
		t.Errorf("parked = %v; want 0", got)
	}

	bySource, ok := body["by_source"].(map[string]any)
	if !ok {
		t.Fatalf("by_source = %v; want an object", body["by_source"])
	}
	if bySource["discover"].(float64) != 1 {
		t.Errorf("by_source[discover] = %v; want 1 — the finished row is not pending", bySource["discover"])
	}
	if bySource["reader-requested"].(float64) != 1 {
		t.Errorf("by_source[reader-requested] = %v; want 1", bySource["reader-requested"])
	}
}

func TestCrawlQueueEndpointCarriesTheFailureReason(t *testing.T) {
	// The reason has to reach the client, or the page can say a fetch failed
	// but never why -- and "404, the work is deleted" and "503, retry later"
	// are the same row without it.
	ts, st := newQueueTestServer(t)
	ctx := context.Background()
	if err := st.EnqueueCrawl(ctx, "https://a/works/7", "discover"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishCrawlJob(ctx, "https://a/works/7", "404 not found"); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, ts, "/api/v1/crawl-queue")
	rows, ok := body["recent"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("recent = %v; want one row", body["recent"])
	}
	row := rows[0].(map[string]any)
	if row["last_error"] != "404 not found" {
		t.Errorf("last_error = %v; want 404 not found", row["last_error"])
	}
	if row["attempts"].(float64) != 1 {
		t.Errorf("attempts = %v; want 1", row["attempts"])
	}
	if row["done"] != false {
		t.Errorf("done = %v; want false — a failed row is not finished", row["done"])
	}
	if row["source"] != "discover" {
		t.Errorf("source = %v; want discover", row["source"])
	}
}

func TestCrawlQueueEndpointRejectsAnUnusableRecentValue(t *testing.T) {
	// recent goes straight to a LIMIT. Unvalidated, "recent=abc" becomes a
	// zero-limit that silently returns nothing while reporting success, and a
	// negative would be worse.
	ts, _ := newQueueTestServer(t)
	for _, q := range []string{"recent=abc", "recent=-1", "recent=501", "recent=1.5", "recent="} {
		status, body := get(t, ts, "/api/v1/crawl-queue?"+q)
		if q == "recent=" {
			// An empty value means "unset", which is the default, not an error.
			if status != http.StatusOK {
				t.Errorf("%s gave status %d; want 200 (empty means default)", q, status)
			}
			continue
		}
		if status != http.StatusBadRequest {
			t.Errorf("%s gave status %d; want 400 (%v)", q, status, body)
		}
		if body["error"] == nil {
			t.Errorf("%s gave no error message", q)
		}
	}
}

func TestCrawlQueueEndpointHonoursTheRecentCap(t *testing.T) {
	ts, st := newQueueTestServer(t)
	ctx := context.Background()
	for _, u := range []string{"https://a/works/1", "https://a/works/2", "https://a/works/3"} {
		if err := st.EnqueueCrawl(ctx, u, "discover"); err != nil {
			t.Fatal(err)
		}
	}
	_, body := get(t, ts, "/api/v1/crawl-queue?recent=2")
	if rows := body["recent"].([]any); len(rows) != 2 {
		t.Errorf("len(recent) = %d; want 2", len(rows))
	}
	// The counts are not affected by the cap on the row list: a client asking
	// for three rows still learns the true backlog.
	if got := body["pending"].(float64); got != 3 {
		t.Errorf("pending = %v; want 3 regardless of the recent cap", got)
	}
}

func TestCrawlQueueEndpointWithoutAStoreIs503NotZero(t *testing.T) {
	// A read-only deployment has no queue. Reporting "0 pending" there would be
	// a measurement of nothing, which reads as "the crawler is idle and done".
	srv := &Server{Engine: &engine.Engine{}, Version: "test", StartedAt: time.Now()}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/crawl-queue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("503 body is not JSON: %v", err)
	}
	if body["error"] == nil {
		t.Error("503 gave no reason")
	}
}

// TestTheCrawlPageIsWiredThroughRoutes covers the seam the unit tests on either
// side cannot: that Server.Routes builds web.Deps with the Store attached. A
// page that renders correctly in isolation and is never wired up is a page
// nobody can reach, and the failure looks like "the feature does not exist".
func TestTheCrawlPageIsWiredThroughRoutes(t *testing.T) {
	ts := newTestServer(t)

	code, body, _ := getRaw(t, ts, "/crawl")
	if code != http.StatusOK {
		t.Fatalf("GET /crawl = %d; want 200", code)
	}
	for _, want := range []string{"Crawl queue", "mirror is not growing", "kindred discover"} {
		if !strings.Contains(body, want) {
			t.Errorf("/crawl does not contain %q -- the page is rendered but has no queue", want)
		}
	}
	// And it must NOT be the no-queue variant: newTestServer has a Store.
	if strings.Contains(body, "no crawl queue") {
		t.Error("/crawl reported no queue on a server that has one; Deps.Queue was not wired")
	}
}
