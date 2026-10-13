package main

// End-to-end for `kindred discover`: a fixture server speaking AO3-shaped
// listing HTML, a real state+corpus pair, and the real crawl queue.
//
// The unit tests cover the walk and the seed parser separately. This one covers
// the seam between them, which is where the interesting failures live: a walk
// that finds works nobody queues has scraped nothing, and a queue whose rows the
// auto-crawler cannot read is a silent no-op.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// ao3Listing renders a page shaped like an AO3 listing: work links, a tag link
// to walk through, and the /works/new chrome every real page carries.
func ao3Listing(ids ...int64) string {
	body := `<html><head><title>Works</title></head><body>`
	body += `<div id="header"><a href="/works/new">Post New Work</a></div>`
	body += `<ol class="work index group">`
	for _, id := range ids {
		body += `<li class="work blurb"><h4 class="heading"><a href="/works/` +
			itoa(id) + `">W` + itoa(id) + `</a></h4></li>`
	}
	body += `</ol></body></html>`
	return body
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func newDiscoverFixture(t *testing.T) (statePath, corpusPath string) {
	t.Helper()
	dir := t.TempDir()
	corpusPath = filepath.Join(dir, "corpus.db")
	statePath = filepath.Join(dir, "state.db")

	cdb, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cdb.Exec(testcorpus.Schema); err != nil {
		t.Fatal(err)
	}
	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}
	return statePath, corpusPath
}

func TestDiscoverWalkQueuesTheWorksItFinds(t *testing.T) {
	statePath, corpusPath := newDiscoverFixture(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			_, _ = w.Write([]byte("User-agent: *\nCrawl-delay: 30\n"))
		case "/tags/Test/works":
			// The tag link is what makes this a walk rather than a fetch: the
			// seed is one hop from /tags/Side, and work 103 is only reachable
			// through it.
			_, _ = w.Write([]byte(ao3Listing(101, 102) +
				`<a class="tag" href="/tags/Side/works">Side</a>`))
		case "/tags/Side/works":
			_, _ = w.Write([]byte(ao3Listing(102, 103)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	// --crawl-delay 0 keeps the test off the real 30-second delay; --db/--corpus
	// are the config's own flags, so this drives the command the way an operator
	// does rather than through a parallel code path.
	err := runDiscover(ctx, []string{
		"--seed", "tag:Test",
		"--base-url", srv.URL,
		"--crawl-delay", "0",
		"--db", statePath,
		"--corpus", corpusPath,
		"--depth", "1",
		"--kinds", "tag",
		"--enqueue",
	})
	if err != nil {
		t.Fatalf("runDiscover: %v", err)
	}

	// Read every queued row directly. NOT via NextCrawlJob: that is a pure
	// read, not a take, so calling it in a loop returns the same oldest row
	// every time and a three-work queue looks like a one-work queue.
	s, err := store.Open(ctx, statePath, corpusPath)
	if err != nil {
		t.Fatalf("opening the store to read the queue: %v", err)
	}
	defer s.Close()

	rows, err := s.DB.QueryContext(ctx, `SELECT url, source FROM crawl_queue ORDER BY url`)
	if err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	defer rows.Close()

	got := map[int64]bool{}
	for rows.Next() {
		var u, src string
		if err := rows.Scan(&u, &src); err != nil {
			t.Fatalf("scanning a queue row: %v", err)
		}
		if src != "discover" {
			t.Errorf("queued %q with source %q; want \"discover\"", u, src)
		}
		got[crawlWorkID(u)] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating the queue: %v", err)
	}

	for _, want := range []int64{101, 102, 103} {
		if !got[want] {
			t.Errorf("work %d was discovered but never queued; queue = %v", want, got)
		}
	}
	// The seed's own page carries /works/new, and the walk must not have queued
	// it: the queue is drained by a fetcher that would spend 30 seconds on it.
	if got[0] {
		t.Error("the /works/new link from the page chrome was queued as work 0")
	}
}

// crawlWorkID pulls the trailing digit run out of a work URL.
func crawlWorkID(u string) int64 {
	start := len(u)
	for start > 0 && u[start-1] >= '0' && u[start-1] <= '9' {
		start--
	}
	var id int64
	for _, c := range u[start:] {
		id = id*10 + int64(c-'0')
	}
	return id
}

// TestDiscoverResumesAcrossRuns is the point of the checkpoint. Before it, a
// walk had no memory at all and every re-run re-read every listing page: a
// 40-page tag is ~20 minutes against AO3's 30-second crawl delay, repaid in
// full each time. Counting the fixture's own requests is the only way to assert
// this -- a result-only assertion passes over a walk that fetched everything
// and then reported it as skipped.
func TestDiscoverResumesAcrossRuns(t *testing.T) {
	statePath, corpusPath := newDiscoverFixture(t)

	var fetches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			_, _ = w.Write([]byte("User-agent: *\nCrawl-delay: 30\n"))
		case "/tags/Test/works":
			atomic.AddInt32(&fetches, 1)
			// The tag link is what makes this two pages; without it there is
			// nothing to expand and "want 2" is wrong.
			_, _ = w.Write([]byte(ao3Listing(101, 102) +
				`<a class="tag" href="/tags/Side/works">Side</a>`))
		case "/tags/Side/works":
			atomic.AddInt32(&fetches, 1)
			_, _ = w.Write([]byte(ao3Listing(102, 103)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	run := func(extra ...string) error {
		args := append([]string{
			"--seed", "tag:Test",
			"--base-url", srv.URL,
			"--crawl-delay", "0",
			"--db", statePath,
			"--corpus", corpusPath,
			"--depth", "1",
			"--kinds", "tag",
			"--enqueue",
		}, extra...)
		return runDiscover(ctx, args)
	}

	// The first run walks everything and records both pages.
	if err := run(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := atomic.LoadInt32(&fetches)
	if first != 2 {
		t.Fatalf("first run fetched %d page(s); want 2 (the seed and the tag it links)", first)
	}

	// The second run must not pay for the walk again, AND must not look like a
	// failure: a resumed walk that skipped everything found nothing new, which
	// is a success. Exiting non-zero here is how a finished tag reads as a
	// broken command.
	if err := run(); err != nil {
		t.Fatalf("second run: %v -- a fully-resumed walk is not a failure", err)
	}
	if second := atomic.LoadInt32(&fetches); second != first {
		t.Errorf("second run fetched %d page(s) total, was %d; the checkpoint did not hold", second, first)
	}

	// --fresh is the escape hatch that makes a sticky checkpoint safe: a busy
	// tag's page 1 holds different works every week, so a permanent memory would
	// make the walk miss the only page that changed.
	if err := run("--fresh"); err != nil {
		t.Fatalf("--fresh run: %v", err)
	}
	if fresh := atomic.LoadInt32(&fetches); fresh != first*2 {
		t.Errorf("after --fresh the total is %d; want %d (every page re-read)", fresh, first*2)
	}
}

// TestDiscoverRecordsNothingWithoutAPageFetch guards the other direction: a
// checkpoint that records pages it never fetched would make the first real run
// skip everything.
func TestDiscoverWithNoResumeAlwaysFetches(t *testing.T) {
	statePath, corpusPath := newDiscoverFixture(t)

	var fetches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nCrawl-delay: 30\n"))
			return
		}
		atomic.AddInt32(&fetches, 1)
		_, _ = w.Write([]byte(ao3Listing(101)))
	}))
	defer srv.Close()

	ctx := context.Background()
	base := []string{"--seed", "tag:Test", "--base-url", srv.URL, "--crawl-delay", "0",
		"--db", statePath, "--corpus", corpusPath, "--print-urls", "--no-resume"}

	for i := 0; i < 2; i++ {
		if err := runDiscover(ctx, base); err != nil {
			t.Fatalf("run with --no-resume: %v", err)
		}
	}
	if got := atomic.LoadInt32(&fetches); got != 2 {
		t.Errorf("--no-resume fetched %d page(s) over two runs; want 2 (no checkpoint at all)", got)
	}

	// And the checkpoint stayed empty, so nothing was silently recorded.
	s, err := store.Open(ctx, statePath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.DiscoveryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pages != 0 {
		t.Errorf("checkpoint has %d page(s); --no-resume must keep none", st.Pages)
	}
}
