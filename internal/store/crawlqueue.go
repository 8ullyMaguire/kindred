package store

// The auto-crawl queue: serve grows the mirror in the background, and this
// is the contract between the request path (which only ever says "a reader
// wanted this") and the worker (which fetches politely, later, off the hot
// path).
//
// The request path does no network I/O. That is not an optimisation -- it is
// the same rule that keeps `serve` offline in SPEC's original terms, kept
// honestly: an INSERT here, an outbound request only in the worker.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// crawlMaxAttempts is how many times a job fails before it is parked.
// Five is generous for a per-work fetch with its own retries underneath
// (the crawler already retries transient failures), and small enough that a
// permanently deleted AO3 work does not come back on every poll.
const crawlMaxAttempts = 5

// CrawlJob is one queued work URL.
type CrawlJob struct {
	URL      string
	Source   string
	Attempts int
}

// EnqueueCrawl queues a work URL for background crawling.
//
// Conflict semantics carry the skip rule: an existing DONE row is never
// re-armed, because done means the metadata exists and refetching what we
// have is the waste this queue exists to avoid. An existing row past the
// retry cap IS re-armed, because a fresh request is new evidence that the
// URL matters -- a reader clicking the link again is exactly when a second
// look at a failed fetch is worth the one request.
func (s *Store) EnqueueCrawl(ctx context.Context, url, source string) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	const stmt = `INSERT INTO crawl_queue(url, source) VALUES(?, ?)
		ON CONFLICT(url) DO UPDATE SET
			attempts = CASE
				WHEN crawl_queue.done_at IS NULL AND crawl_queue.attempts >= ? THEN 0
				ELSE crawl_queue.attempts END,
			last_error = CASE
				WHEN crawl_queue.done_at IS NULL AND crawl_queue.attempts >= ? THEN NULL
				ELSE crawl_queue.last_error END`
	if _, err := s.DB.ExecContext(ctx, stmt, url, source,
		crawlMaxAttempts, crawlMaxAttempts); err != nil {
		return fmt.Errorf("enqueue crawl %s: %w", url, err)
	}
	return nil
}

// EnqueueIncompleteWorks queues up to limit works whose stored metadata is
// missing a summary -- the "gets better over time" input. It reads through
// the corpus handle (read-only, separate database) and writes through the
// state handle; the two never share a connection, which is the same split
// every other corpus read on this type makes.
//
// Returns how many rows were newly queued.
func (s *Store) EnqueueIncompleteWorks(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	rows, err := s.Corpus.QueryContext(ctx,
		`SELECT url FROM works
		 WHERE url IS NOT NULL AND url <> ''
		   AND (summary IS NULL OR summary = '')
		 ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return 0, fmt.Errorf("select incomplete works: %w", err)
	}
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan incomplete work: %w", err)
		}
		urls = append(urls, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scan incomplete works: %w", err)
	}

	queued := 0
	for _, u := range urls {
		const stmt = `INSERT INTO crawl_queue(url, source) VALUES(?, 'incomplete')
			ON CONFLICT(url) DO NOTHING`
		res, err := s.DB.ExecContext(ctx, stmt, u)
		if err != nil {
			return queued, fmt.Errorf("queue incomplete work %s: %w", u, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			queued++
		}
	}
	return queued, nil
}

// NextCrawlJob returns the oldest undoned, unparked job.
//
// Order is enqueued_at then url: deterministic, and oldest-first means a
// burst of reader requests is served in request order rather than in
// whatever order an index happens to return.
func (s *Store) NextCrawlJob(ctx context.Context) (CrawlJob, bool, error) {
	const stmt = `SELECT url, source, attempts FROM crawl_queue
		WHERE done_at IS NULL AND attempts < ?
		ORDER BY enqueued_at, url LIMIT 1`
	var j CrawlJob
	err := s.DB.QueryRowContext(ctx, stmt, crawlMaxAttempts).
		Scan(&j.URL, &j.Source, &j.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return CrawlJob{}, false, nil
	}
	if err != nil {
		return CrawlJob{}, false, fmt.Errorf("next crawl job: %w", err)
	}
	return j, true, nil
}

// FinishCrawlJob records a job outcome. An empty errMsg marks the work done
// (its metadata is now in the mirror, or was already); any other string
// counts an attempt and keeps the reason, so the next failure can be
// diagnosed without a log dive.
func (s *Store) FinishCrawlJob(ctx context.Context, url, errMsg string) error {
	if errMsg == "" {
		const stmt = `UPDATE crawl_queue
			SET done_at = datetime('now'), last_error = NULL
			WHERE url = ? AND done_at IS NULL`
		if _, err := s.DB.ExecContext(ctx, stmt, url); err != nil {
			return fmt.Errorf("finish crawl job: %w", err)
		}
		return nil
	}
	const stmt = `UPDATE crawl_queue
		SET attempts = attempts + 1, last_error = ?
		WHERE url = ? AND done_at IS NULL`
	if _, err := s.DB.ExecContext(ctx, stmt, errMsg, url); err != nil {
		return fmt.Errorf("fail crawl job: %w", err)
	}
	return nil
}

// CrawlRow is one queued work URL, for a status page.
type CrawlRow struct {
	URL        string
	Source     string
	Attempts   int
	LastError  string
	EnqueuedAt string
	Done       bool
}

// CrawlQueueStats is the auto-crawler's backlog: what the mirror is waiting to
// fetch and what it has given up on.
//
// Three counts rather than one, because they mean different things to whoever
// is looking. Pending is work in progress, done is the checkpoint, and parked
// is jobs that failed crawlMaxAttempts times and will NOT be retried until
// something asks for them again (EnqueueCrawl re-arms exactly those). A page
// showing one number could not tell "the crawler is behind" from "the crawler
// is fine and these are dead links" -- and those call for opposite responses.
type CrawlQueueStats struct {
	Pending  int
	Done     int
	Parked   int
	BySource map[string]int // pending rows, by who asked for them
	Recent   []CrawlRow     // most recently enqueued first
}

// CrawlQueueStats reports the backlog. recent caps the row list; 0 omits it.
func (s *Store) CrawlQueueStats(ctx context.Context, recent int) (CrawlQueueStats, error) {
	out := CrawlQueueStats{BySource: map[string]int{}}

	// COALESCE because SUM over an empty table is NULL, not 0: a fresh mirror
	// is the common case here and it must read as "0 pending", not as an error.
	const totals = `SELECT
		COALESCE(SUM(CASE WHEN done_at IS NULL AND attempts < ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN done_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN done_at IS NULL AND attempts >= ? THEN 1 ELSE 0 END), 0)
		FROM crawl_queue`
	if err := s.DB.QueryRowContext(ctx, totals, crawlMaxAttempts, crawlMaxAttempts).
		Scan(&out.Pending, &out.Done, &out.Parked); err != nil {
		return out, fmt.Errorf("crawl queue totals: %w", err)
	}

	srcRows, err := s.DB.QueryContext(ctx, `SELECT source, COUNT(*) FROM crawl_queue
		WHERE done_at IS NULL AND attempts < ?
		GROUP BY source ORDER BY COUNT(*) DESC, source ASC`, crawlMaxAttempts)
	if err != nil {
		return out, fmt.Errorf("crawl queue sources: %w", err)
	}
	defer srcRows.Close()
	for srcRows.Next() {
		var src string
		var n int
		if err := srcRows.Scan(&src, &n); err != nil {
			return out, fmt.Errorf("scanning a queue source: %w", err)
		}
		out.BySource[src] = n
	}
	if err := srcRows.Err(); err != nil {
		return out, fmt.Errorf("iterating queue sources: %w", err)
	}

	if recent > 0 {
		// enqueued_at has one-second resolution, so a bulk enqueue shares a
		// timestamp across hundreds of rows; URL breaks the tie and makes the
		// list stable between two reads of an unchanged queue.
		rows, err := s.DB.QueryContext(ctx, `SELECT url, source, attempts,
			COALESCE(last_error, ''), enqueued_at, done_at IS NOT NULL
			FROM crawl_queue ORDER BY enqueued_at DESC, url DESC LIMIT ?`, recent)
		if err != nil {
			return out, fmt.Errorf("crawl queue rows: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r CrawlRow
			if err := rows.Scan(&r.URL, &r.Source, &r.Attempts, &r.LastError,
				&r.EnqueuedAt, &r.Done); err != nil {
				return out, fmt.Errorf("scanning a queue row: %w", err)
			}
			out.Recent = append(out.Recent, r)
		}
		if err := rows.Err(); err != nil {
			return out, fmt.Errorf("iterating queue rows: %w", err)
		}
	}
	return out, nil
}
