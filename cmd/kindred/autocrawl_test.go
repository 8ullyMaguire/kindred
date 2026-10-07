package main

// The auto-crawler's job step: fetch-and-save, skip-if-present, and honest
// failure accounting -- each exercised without the network, through the
// crawl.Client interface the crawler exists to abstract.

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
	_ "modernc.org/sqlite"
)

// autoCrawlPage is a minimal AO3 work page ParseWork accepts: the shape the
// skip rule and the sink both key on (id from URL, summary from the block).
const autoCrawlPage = `<!DOCTYPE html><html><head>
<meta name="works:title" content="Fetched Work"/>
<meta name="works:author" content="someauthor"/>
<meta name="works:words" content="1,200"/>
<meta name="works:kudos" content="34"/>
<meta name="works:hits" content="500"/>
</head><body>
<span class="status">Completed</span>
<div class="language">English</div>
<div class="summary"><blockquote>A summary the crawler brought home.</blockquote></div>
<ul class="tags">
<li class="freeforms"><a class="tag" href="/tags/dark">dark</a></li>
</ul>
</body></html>`

// stubClient counts fetches so a test can prove the SKIP path skipped.
type stubClient struct {
	body    string
	err     error
	fetches int
}

func (c *stubClient) Fetch(context.Context, string) ([]byte, error) {
	c.fetches++
	if c.err != nil {
		return nil, c.err
	}
	return []byte(c.body), nil
}

func (c *stubClient) Delay() time.Duration { return 0 } // no sleeping in tests
func (c *stubClient) Close() error         { return nil }

// newAutoCrawlFixture: state store + rw mirror, with work 5 incomplete and
// work 7 already complete.
func newAutoCrawlFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	corpusPath := filepath.Join(dir, "corpus.db")
	statePath := filepath.Join(dir, "state.db")

	cdb, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cdb.Exec(testcorpus.Schema); err != nil {
		t.Fatal(err)
	}
	if _, err := cdb.Exec(`INSERT INTO works(id,url,title,authors,summary,word_count,kudos,hits) VALUES
		(5,'https://archiveofourown.org/works/5','Incomplete','author',NULL,100,10,10),
		(7,'https://archiveofourown.org/works/7','Complete','author','already stored',100,10,10)`); err != nil {
		t.Fatal(err)
	}
	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(t.Context(), statePath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, corpusPath
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io_Discard{}, nil))
}

// io_Discard keeps the test's logs out of the output without importing io's
// name into every assertion line.
type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }

func TestAutoCrawlFetchesIncompleteAndSaves(t *testing.T) {
	ctx := context.Background()
	s, corpusPath := newAutoCrawlFixture(t)
	mirror, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()

	const url5 = "https://archiveofourown.org/works/5"
	if err := s.EnqueueCrawl(ctx, url5, "reader-requested"); err != nil {
		t.Fatal(err)
	}
	job, ok, err := s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("queued job: ok=%v err=%v", ok, err)
	}

	client := &stubClient{body: autoCrawlPage}
	cr := crawl.New(client)
	processAutoCrawlJob(ctx, s, mirror, crawl.NewSQLSink(mirror), cr, job, discardLog())

	// Queue state first: if the fetch or the write failed, last_error says
	// which, and a NULL summary with no error recorded would be the test's
	// own bug rather than the worker's.
	var doneAt, lastErr sql.NullString
	var attempts int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT done_at, attempts, last_error FROM crawl_queue WHERE url = ?`, url5).
		Scan(&doneAt, &attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	t.Logf("queue row: done=%v attempts=%d err=%v fetches=%d",
		doneAt.Valid, attempts, lastErr.String, client.fetches)
	if !doneAt.Valid {
		t.Fatalf("job not marked done: attempts=%d err=%v", attempts, lastErr.String)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d on success, want 0", attempts)
	}

	// The metadata landed in the mirror...
	var summary string
	if err := mirror.QueryRowContext(ctx,
		`SELECT summary FROM works WHERE id = 5`).Scan(&summary); err != nil {
		t.Fatalf("work 5 after crawl: %v", err)
	}
	if summary == "" {
		t.Error("summary still empty after a successful fetch")
	}
	// ...and the job is done, so nothing refetches it.
	if _, ok, err := s.NextCrawlJob(ctx); err != nil || ok {
		t.Errorf("queue not drained after success: ok=%v err=%v", ok, err)
	}
	if client.fetches != 1 {
		t.Errorf("fetches = %d, want 1", client.fetches)
	}
}

func TestAutoCrawlSkipsWorkWhoseMetadataExists(t *testing.T) {
	ctx := context.Background()
	s, corpusPath := newAutoCrawlFixture(t)
	mirror, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()

	const url7 = "https://archiveofourown.org/works/7"
	if err := s.EnqueueCrawl(ctx, url7, "incomplete"); err != nil {
		t.Fatal(err)
	}
	job, ok, err := s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("queued job: ok=%v err=%v", ok, err)
	}

	// A client that would FAIL the test if it were called: existing
	// metadata must cost zero requests.
	client := &stubClient{err: errors.New("network must not be touched")}
	cr := crawl.New(client)
	processAutoCrawlJob(ctx, s, mirror, crawl.NewSQLSink(mirror), cr, job, discardLog())

	if client.fetches != 0 {
		t.Errorf("fetches = %d, want 0 for a work whose metadata exists", client.fetches)
	}
	if _, ok, err := s.NextCrawlJob(ctx); err != nil || ok {
		t.Errorf("skip did not mark the job done: ok=%v err=%v", ok, err)
	}
}

func TestAutoCrawlFailureIsRecordedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	s, corpusPath := newAutoCrawlFixture(t)
	mirror, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()

	const url5 = "https://archiveofourown.org/works/5"
	if err := s.EnqueueCrawl(ctx, url5, "viewed-incomplete"); err != nil {
		t.Fatal(err)
	}
	job, ok, err := s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("queued job: ok=%v err=%v", ok, err)
	}

	client := &stubClient{err: errors.New("connection refused")}
	cr := crawl.New(client)
	// Production backoff is 10s doubling; the point of this test is the
	// accounting, not the waiting.
	oldRetries, oldBackoff := autoCrawlRetries, autoCrawlBackoff
	autoCrawlRetries, autoCrawlBackoff = 1, time.Millisecond
	defer func() { autoCrawlRetries, autoCrawlBackoff = oldRetries, oldBackoff }()
	processAutoCrawlJob(ctx, s, mirror, crawl.NewSQLSink(mirror), cr, job, discardLog())

	next, ok, err := s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("failed job left the queue entirely: ok=%v err=%v", ok, err)
	}
	if next.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", next.Attempts)
	}
	if next.URL != url5 {
		t.Errorf("next job = %s, want the retried %s", next.URL, url5)
	}
}
