package store

// The walk's checkpoint: which listing pages a discovery walk has read.
//
// ## Why this is a table and not a resume file
//
// internal/crawl.State already has a JSON resume file for batch crawls, and it
// is the wrong shape here for the reason the crawl_queue schema gives for
// choosing per-row state over a batch checkpoint: a JSON set of every URL ever
// walked grows without bound and is O(history) to test membership against,
// while SQLite gives O(log n) on an index that already exists and lets the count
// be queried for a status page. It also keeps one home for "what has been
// walked" instead of two files that can disagree.
//
// ## Why the skip is a row, not a timestamp
//
// The same reason crawl_queue.done_at is a boolean rather than a window:
// "have we read this page" is answerable exactly, and a recency comparison
// answers a different question (is the page still fresh?) that a caller who
// wanted the exact answer would then have to work around. fetched_at is stored
// for the operator and for a future explicit --older-than pass; it is not the
// filter.

import (
	"context"
	"fmt"
	"strings"
)

// DiscoveryPages returns every listing page already walked, as a set for the
// walk to test membership against.
//
// A set rather than a query per page: a walk reads tens of pages and the table
// is small, so one read at the start replaces N round trips with one. The point
// at which the table is too big for that is the point at which this wants a
// different design, not a different query.
func (s *Store) DiscoveryPages(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT url FROM discovery_pages`)
	if err != nil {
		return nil, fmt.Errorf("read discovery pages: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("scan a discovery page: %w", err)
		}
		out[u] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate discovery pages: %w", err)
	}
	return out, nil
}

// MarkDiscoveryPage records that a listing page has been walked.
//
// On conflict it UPDATES works_found and fetched_at, unlike crawl_queue's
// upsert which deliberately refuses to touch done_at. The difference is what a
// repeat means: a re-crawled work has nothing new to say, but a re-walked page
// does -- the tag gained works, or the earlier walk stopped at MaxWorks -- and
// the count is exactly what a later pass compares against. Leaving the old
// count would be a number that looks like a measurement and is not one.
func (s *Store) MarkDiscoveryPage(ctx context.Context, url string, worksFound int) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	if worksFound < 0 {
		worksFound = 0
	}
	const stmt = `INSERT INTO discovery_pages(url, works_found) VALUES(?, ?)
		ON CONFLICT(url) DO UPDATE SET
			fetched_at  = datetime('now'),
			works_found = excluded.works_found`
	if _, err := s.DB.ExecContext(ctx, stmt, url, worksFound); err != nil {
		return fmt.Errorf("mark discovery page %s: %w", url, err)
	}
	return nil
}

// ForgetDiscoveryPages clears the checkpoint so the next walk re-reads every
// page. Returns how many rows were cleared.
//
// This escape hatch is what makes a default-on checkpoint safe rather than a
// trap. A walk that remembers a page forever is wrong for a busy tag, whose
// page 1 holds different works every week: the first walk would find 20 works,
// mark the page, and the second walk would skip the only page that changed.
// Resume-on-by-default needs this to be reachable, and --fresh is the operator's
// answer to it.
func (s *Store) ForgetDiscoveryPages(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM discovery_pages`)
	if err != nil {
		return 0, fmt.Errorf("clear discovery pages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear discovery pages: %w", err)
	}
	return n, nil
}

// DiscoveryStats is the size and recency of the walk checkpoint.
type DiscoveryStats struct {
	Pages       int    // distinct listing pages walked
	WorksFound  int    // sum of the per-page counts
	LastFetched string // most recent fetched_at, empty when nothing is recorded
}

// DiscoveryStats reports the walk checkpoint, for the CLI summary and /crawl.
func (s *Store) DiscoveryStats(ctx context.Context) (DiscoveryStats, error) {
	var out DiscoveryStats
	// COALESCE on the aggregates and not on COUNT: SUM and MAX over zero rows
	// return NULL, but COUNT returns 0. A fresh mirror is the common case, and
	// scanning that NULL into a string or an int is an error on exactly the
	// state a new deployment hits first.
	const stmt = `SELECT
		COUNT(*),
		COALESCE(SUM(works_found), 0),
		COALESCE(MAX(fetched_at), '')
		FROM discovery_pages`
	if err := s.DB.QueryRowContext(ctx, stmt).
		Scan(&out.Pages, &out.WorksFound, &out.LastFetched); err != nil {
		return out, fmt.Errorf("discovery stats: %w", err)
	}
	return out, nil
}
