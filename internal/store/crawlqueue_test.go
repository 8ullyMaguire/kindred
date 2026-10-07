package store

// The auto-crawl queue's contract: what the request path may assume when it
// fires off one INSERT, and what the worker may assume when it drains.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

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
