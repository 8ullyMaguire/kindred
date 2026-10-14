package main

import (
	"fmt"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// undoned counts queue rows still waiting, read straight from the table so the
// test cannot pass by agreeing with a helper's arithmetic.
func undoned(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM crawl_queue WHERE done_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count undoned: %v", err)
	}
	return n
}

func enqueue(t *testing.T, s *store.Store, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		u := fmt.Sprintf("https://archiveofourown.org/works/%d", 900+i)
		if err := s.EnqueueCrawl(t.Context(), u, "test"); err != nil {
			t.Fatalf("enqueue %s: %v", u, err)
		}
	}
}

// The bound is what lets a fifteen-minute timer own the drain. Without it a run
// goes until the queue is empty, which at AO3's 30s crawl delay is hours, and
// "the timer runs it" would be a fiction.
func TestABoundedDrainStopsAtItsBoundAndLeavesTheRestQueued(t *testing.T) {
	s, _ := newAutoCrawlFixture(t)
	enqueue(t, s, 5)
	if got := undoned(t, s); got != 5 {
		t.Fatalf("fixture queued %d rows, want 5", got)
	}

	var seen []string
	processed := drainQueue(t.Context(), s, 2, discardLog(), func(job store.CrawlJob) {
		seen = append(seen, job.URL)
		// Finish it the way the real path does, so the row stops being eligible.
		if err := s.FinishCrawlJob(t.Context(), job.URL, ""); err != nil {
			t.Errorf("finish %s: %v", job.URL, err)
		}
	})
	if processed != 2 {
		t.Errorf("processed = %d, want 2", processed)
	}
	if len(seen) != 2 {
		t.Errorf("the processor saw %d jobs, want 2", len(seen))
	}
	// THE POINT: the run stopped at its bound, not at the end of the queue. The
	// next tick picks these up, which is why no state has to be carried between
	// runs.
	if got := undoned(t, s); got != 3 {
		t.Errorf("%d rows still queued, want 3 — a bounded run must leave the rest", got)
	}

	// A second run, with room for all of them, drains what is left.
	second := drainQueue(t.Context(), s, 10, discardLog(), func(job store.CrawlJob) {
		if err := s.FinishCrawlJob(t.Context(), job.URL, ""); err != nil {
			t.Errorf("finish %s: %v", job.URL, err)
		}
	})
	if second != 3 {
		t.Errorf("second run processed = %d, want 3", second)
	}
	if got := undoned(t, s); got != 0 {
		t.Errorf("%d rows still queued after draining, want 0", got)
	}
}

// A queue that is already empty must return immediately rather than waiting for
// the poll, or every timer tick would hold the unit open for 30 seconds doing
// nothing.
func TestABoundedDrainOnAnEmptyQueueReturnsAtOnce(t *testing.T) {
	s, _ := newAutoCrawlFixture(t)
	called := false
	processed := drainQueue(t.Context(), s, 25, discardLog(), func(store.CrawlJob) { called = true })
	if processed != 0 || called {
		t.Errorf("processed = %d, called = %v; want 0 and false", processed, called)
	}
}
