package store

// The auto-crawl queue's contract: what the request path may assume when it
// fires off one INSERT, and what the worker may assume when it drains.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
	_ "modernc.org/sqlite"
)

func TestCrawlQueueLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const workA = "https://archiveofourown.org/works/111"
	const workB = "https://archiveofourown.org/works/222"

	// Enqueue is idempotent: two readers asking for the same missing work
	// queue it once.
	if err := s.EnqueueCrawl(ctx, workA, "reader-requested"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueCrawl(ctx, workA, "reader-requested"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueCrawl(ctx, workB, "incomplete"); err != nil {
		t.Fatal(err)
	}

	// Oldest first, and both pending.
	job, ok, err := s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("first job: ok=%v err=%v", ok, err)
	}
	if job.URL != workA || job.Source != "reader-requested" {
		t.Errorf("first job = %+v, want %s/reader-requested", job, workA)
	}

	// A failure records the attempt and the reason, and the job stays
	// pending: one failure is not an answer.
	if err := s.FinishCrawlJob(ctx, workA, "429 rate limited"); err != nil {
		t.Fatal(err)
	}
	job, ok, err = s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("job after failure: ok=%v err=%v", ok, err)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", job.Attempts)
	}
	var lastErr sql.NullString
	if err := s.DB.QueryRowContext(ctx,
		`SELECT last_error FROM crawl_queue WHERE url = ?`, workA).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if !lastErr.Valid || lastErr.String != "429 rate limited" {
		t.Errorf("last_error = %v, want the failure reason", lastErr.String)
	}

	// Park after the retry cap: a dead URL must stop consuming polls.
	for i := 1; i < 5; i++ {
		if err := s.FinishCrawlJob(ctx, workA, "still failing"); err != nil {
			t.Fatal(err)
		}
	}
	// The queue now offers B, not the parked A.
	job, ok, err = s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("queue after parking A: ok=%v err=%v", ok, err)
	}
	if job.URL != workB {
		t.Errorf("parked A still served first: got %s", job.URL)
	}
	if err := s.FinishCrawlJob(ctx, workB, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.NextCrawlJob(ctx); err != nil || ok {
		t.Errorf("queue not empty with A parked and B done: ok=%v err=%v", ok, err)
	}

	// A fresh request re-arms the parked job -- new evidence that the URL
	// matters -- and clears the failure trail.
	if err := s.EnqueueCrawl(ctx, workA, "reader-requested"); err != nil {
		t.Fatal(err)
	}
	job, ok, err = s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("re-armed job: ok=%v err=%v", ok, err)
	}
	if job.Attempts != 0 {
		t.Errorf("re-armed attempts = %d, want 0", job.Attempts)
	}

	// Done is done: finishing, then asking again, must not refetch work
	// whose metadata exists. This is the skip rule at the queue level.
	if err := s.FinishCrawlJob(ctx, workA, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueCrawl(ctx, workA, "reader-requested"); err != nil {
		t.Fatal(err)
	}
	for {
		job, ok, err = s.NextCrawlJob(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if job.URL == workA {
			t.Fatalf("done work %s re-entered the queue", workA)
		}
		if err := s.FinishCrawlJob(ctx, job.URL, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnqueueIncompleteWorksSelectsOnlyMissingSummaries(t *testing.T) {
	ctx := context.Background()
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
	// One NULL summary, one empty, one complete: only the first two are
	// incomplete, and a complete work must never be queued for refetch.
	if _, err := cdb.Exec(`INSERT INTO works(id,url,title,authors,summary) VALUES
		(1,'https://archiveofourown.org/works/1','A','x',NULL),
		(2,'https://archiveofourown.org/works/2','B','y',''),
		(3,'https://archiveofourown.org/works/3','C','z','done already')`); err != nil {
		t.Fatal(err)
	}
	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, statePath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	n, err := s.EnqueueIncompleteWorks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("queued %d, want 2 (the two without summaries)", n)
	}
	// Second run queues nothing new: the rows exist.
	n2, err := s.EnqueueIncompleteWorks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("second run queued %d, want 0", n2)
	}
}

// K4: a work that was fetched must not be fetched again FOR EVER.
//
// The bug this covers is the quietest kind: `done_at` was written and then only
// ever tested `IS NULL`, so every work was visited exactly once and the mirror
// silently stopped tracking works that were still being written. A refresh pass
// that requeues the wrong rows is just as bad in the other direction -- it
// spends AO3's 30-second crawl budget re-reading what is already current -- so
// the boundary is asserted from both sides.
func TestRequeueStaleRequeuesOnlyWorksFetchedBeforeTheWindow(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const old = "https://archiveofourown.org/works/900"
	const recent = "https://archiveofourown.org/works/901"
	for _, u := range []string{old, recent} {
		if err := s.EnqueueCrawl(ctx, u, "reader-requested"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishCrawlJob(ctx, old, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCrawlJob(ctx, recent, ""); err != nil {
		t.Fatal(err)
	}
	// The two rows need different ages, and FinishCrawlJob stamps "now" for
	// both, so the ages are written directly. This is a fact about the queue's
	// columns, not a shortcut around the code under test -- RequeueStale is
	// what is being measured.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE crawl_queue SET done_at = datetime('now','-40 days') WHERE url = ?`, old); err != nil {
		t.Fatal(err)
	}

	// A failed attempt count that must be RESET on requeue: NextCrawlJob reads
	// `attempts < CrawlMaxAttempts`, so a refreshed row carrying a dead count is
	// unclaimable and the refresh reports success while doing nothing.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE crawl_queue SET attempts = 4 WHERE url = ?`, old); err != nil {
		t.Fatal(err)
	}

	before := packedTime(t, "-30 days")
	if n, err := s.CountStale(ctx, before); err != nil || n != 1 {
		t.Fatalf("CountStale = %d, %v; want 1 (only the 40-day-old row)", n, err)
	}
	n, err := s.RequeueStale(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RequeueStale = %d, want 1", n)
	}

	job, ok, err := s.NextCrawlJob(ctx)
	if err != nil || !ok {
		t.Fatalf("the refreshed work is not claimable: ok=%v err=%v", ok, err)
	}
	if job.URL != old {
		t.Fatalf("next job = %s, want the refreshed %s", job.URL, old)
	}
	if job.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0: a refreshed row must not carry a dead failure count", job.Attempts)
	}

	// The recent row is untouched: still done, still not claimable.
	var doneAt *string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT done_at FROM crawl_queue WHERE url = ?`, recent).Scan(&doneAt); err != nil {
		t.Fatal(err)
	}
	if doneAt == nil {
		t.Fatalf("%s was requeued; the window must protect a work fetched inside it", recent)
	}

	// And running it twice is not an avalanche: the first pass cleared done_at,
	// so the second finds nothing.
	if n, err := s.RequeueStale(ctx, before); err != nil || n != 0 {
		t.Fatalf("second RequeueStale = %d, %v; want 0 (already requeued rows have no done_at)", n, err)
	}
}

// packedTime renders the same 'YYYY-MM-DD HH:MM:SS' form done_at is stored in,
// so a test's window and the store's comparison are the same units.
func packedTime(t *testing.T, ago string) time.Time {
	t.Helper()
	var s string
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT datetime('now', ?)`, ago).Scan(&s); err != nil {
		t.Fatal(err)
	}
	tm, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}
