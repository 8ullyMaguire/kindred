package api

// The request path's only crawler duty: say "a reader wanted this" with one
// INSERT, then serve the page. The network half is tested in cmd/kindred's
// autocrawl tests; this proves the hot path does its part -- and that the
// hot path does ONLY that part.

import (
	"context"
	"net/http"
	"testing"
)

// TestUnknownWorkQueuesBackgroundCrawl: /work/<missing> 404s AND queues the
// work for the background crawler. The 404 is the honest answer today; the
// queue is why it stops being one.
func TestUnknownWorkQueuesBackgroundCrawl(t *testing.T) {
	ts, s := newTestServerWithStore(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/work/999999")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	job, ok, err := s.NextCrawlJob(context.Background())
	if err != nil || !ok {
		t.Fatalf("no job queued after a 404: ok=%v err=%v", ok, err)
	}
	wantURL := "https://archiveofourown.org/works/999999"
	if job.URL != wantURL {
		t.Errorf("queued %q, want %q", job.URL, wantURL)
	}
	if job.Source != "reader-requested" {
		t.Errorf("source = %q, want reader-requested", job.Source)
	}

	// A second hit on the same 404 must not duplicate the row.
	resp2, err := http.Get(ts.URL + "/work/999999")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	job2, ok, err := s.NextCrawlJob(context.Background())
	if err != nil || !ok {
		t.Fatalf("queue lost its job: ok=%v err=%v", ok, err)
	}
	if job2.URL != job.URL {
		t.Errorf("second hit queued a different row: %q", job2.URL)
	}
	if _, ok, err := s.NextCrawlJob(context.Background()); err != nil || ok {
		// Finish the first, then nothing else should be pending: exactly
		// one row for one URL, no matter how many times it was requested.
		if err := s.FinishCrawlJob(context.Background(), job.URL, ""); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.NextCrawlJob(context.Background()); err != nil || ok {
			t.Errorf("extra crawl rows pending: ok=%v err=%v", ok, err)
		}
	}
}
