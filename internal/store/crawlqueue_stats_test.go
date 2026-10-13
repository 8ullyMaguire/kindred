package store

// CrawlQueueStats is what an operator reads to decide whether the mirror is
// behind, healthy, or stuck on dead links. The distinction is the whole point
// of the type, so the tests are about the boundaries between the three states
// rather than about the happy path.

import (
	"context"
	"testing"
)

func TestCrawlQueueStatsOnAnEmptyQueueIsZeroNotAnError(t *testing.T) {
	// SUM over no rows is NULL in SQL. A fresh mirror is the common case and it
	// has to read as "nothing queued", not as a failed query.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.CrawlQueueStats(ctx, 10)
	if err != nil {
		t.Fatalf("CrawlQueueStats on an empty queue: %v", err)
	}
	if got.Pending != 0 || got.Done != 0 || got.Parked != 0 {
		t.Errorf("counts = %d/%d/%d; want 0/0/0", got.Pending, got.Done, got.Parked)
	}
	if len(got.BySource) != 0 {
		t.Errorf("BySource = %v; want empty", got.BySource)
	}
	if len(got.Recent) != 0 {
		t.Errorf("Recent = %v; want empty", got.Recent)
	}
}

func TestCrawlQueueStatsSeparatesParkedFromPending(t *testing.T) {
	// The distinction that changes what an operator does: pending means wait,
	// parked means these will never be retried until something re-asks. One
	// number cannot tell "behind" from "stuck on dead links".
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	pending := []string{"https://a/works/1", "https://a/works/2"}
	done := "https://a/works/3"
	parked := "https://a/works/4"

	for _, u := range pending {
		if err := s.EnqueueCrawl(ctx, u, "discover"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnqueueCrawl(ctx, done, "reader-requested"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueCrawl(ctx, parked, "incomplete"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCrawlJob(ctx, done, ""); err != nil {
		t.Fatal(err)
	}
	// Exactly the cap: parked, not pending.
	for i := 0; i < CrawlMaxAttempts; i++ {
		if err := s.FinishCrawlJob(ctx, parked, "500 internal server error"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.CrawlQueueStats(ctx, 10)
	if err != nil {
		t.Fatalf("CrawlQueueStats: %v", err)
	}
	if got.Pending != len(pending) {
		t.Errorf("Pending = %d; want %d", got.Pending, len(pending))
	}
	if got.Done != 1 {
		t.Errorf("Done = %d; want 1", got.Done)
	}
	if got.Parked != 1 {
		t.Errorf("Parked = %d; want 1 — attempts == CrawlMaxAttempts is parked", got.Parked)
	}
	if got.BySource["discover"] != 2 {
		t.Errorf("BySource[discover] = %d; want 2 — the done and parked rows are not pending",
			got.BySource["discover"])
	}
	if _, ok := got.BySource["reader-requested"]; ok {
		t.Error("a finished row was counted as pending by source")
	}
}

func TestCrawlQueueStatsPendingBoundaryIsOneBelowTheCap(t *testing.T) {
	// One short of the cap is still pending and still gets retried. If this
	// boundary moves, a job is either retried forever or given up on early,
	// and neither shows up as a failure anywhere else.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const u = "https://a/works/9"
	if err := s.EnqueueCrawl(ctx, u, "discover"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < CrawlMaxAttempts-1; i++ {
		if err := s.FinishCrawlJob(ctx, u, "503"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.CrawlQueueStats(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pending != 1 || got.Parked != 0 {
		t.Errorf("pending/parked = %d/%d after %d failures; want 1/0",
			got.Pending, got.Parked, CrawlMaxAttempts-1)
	}

	// One more failure crosses it.
	if err := s.FinishCrawlJob(ctx, u, "503"); err != nil {
		t.Fatal(err)
	}
	got, err = s.CrawlQueueStats(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pending != 0 || got.Parked != 1 {
		t.Errorf("pending/parked = %d/%d at the cap; want 0/1", got.Pending, got.Parked)
	}
}

func TestCrawlQueueStatsRecentIsCappedAndCarriesTheError(t *testing.T) {
	// The row list is what makes the page useful when something is failing: the
	// reason has to be on the page, not in the log.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const failed = "https://a/works/failed"
	if err := s.EnqueueCrawl(ctx, failed, "reader-requested"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCrawlJob(ctx, failed, "404 not found"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.EnqueueCrawl(ctx, "https://a/works/"+string(rune('a'+i)), "discover"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.CrawlQueueStats(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Recent) != 3 {
		t.Fatalf("len(Recent) = %d; want 3", len(got.Recent))
	}
	// Newest first, with URL breaking the tie (every row here shares an
	// enqueued_at second). A most-recent list in the wrong order is worse than
	// no list: it reads as an older failure having just happened.
	one, err := s.CrawlQueueStats(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Recent) != 1 || one.Recent[0].URL != failed {
		t.Errorf("most recent = %v; want %s", one.Recent, failed)
	}

	all, err := s.CrawlQueueStats(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Recent) != 6 {
		t.Fatalf("len(Recent) = %d; want all 6 rows", len(all.Recent))
	}
	var sawFailed bool
	for _, r := range all.Recent {
		if r.URL == failed {
			sawFailed = true
			if r.LastError != "404 not found" {
				t.Errorf("LastError = %q; want 404 not found", r.LastError)
			}
			if r.Done {
				t.Error("a row finished with an error was reported as done")
			}
			if r.Attempts != 1 {
				t.Errorf("Attempts = %d; want 1", r.Attempts)
			}
		}
	}
	if !sawFailed {
		t.Error("the failing row is missing from Recent")
	}
}

func TestCrawlQueueStatsRecentZeroOmitsRows(t *testing.T) {
	// The API asks for counts without paying for the row list, and a caller
	// that forgot to pass a limit must not get an unbounded read of a queue
	// that can hold hundreds of thousands of rows.
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnqueueCrawl(ctx, "https://a/works/1", "discover"); err != nil {
		t.Fatal(err)
	}

	got, err := s.CrawlQueueStats(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Recent) != 0 {
		t.Errorf("Recent = %v; want empty when recent is 0", got.Recent)
	}
	if got.Pending != 1 {
		t.Errorf("Pending = %d; want 1 — the counts must still be right", got.Pending)
	}

	// A negative cap must mean "no rows", not "no cap". SQLite reads LIMIT -1
	// as unlimited, so a caller passing a negative (an unbounded int from a CLI
	// flag, a -1 sentinel) would silently get the whole queue -- which on a real
	// mirror is hundreds of thousands of rows, and the opposite of what it asked
	// for.
	if neg, err := s.CrawlQueueStats(ctx, -1); err != nil {
		t.Fatal(err)
	} else if len(neg.Recent) != 0 {
		t.Errorf("Recent = %v for recent = -1; want empty (LIMIT -1 is unlimited in SQLite)", neg.Recent)
	}
}
