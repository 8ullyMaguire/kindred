package main

// `refresh` — put works that were fetched a while ago back on the queue.
//
// K4. The crawl queue recorded `done_at` on every successful fetch and
// NextCrawlJob only ever tested `done_at IS NULL`, so a work was fetched
// exactly ONCE, EVER. A fic that gained forty chapters after its first visit
// kept its first-visit chapter count, and no part of the system could notice:
// the mirror was correct about a past it had stopped updating.
//
// This command is the whole fix, and it is deliberately a command. The mirror
// is a CACHE, not a snapshot — a snapshot that silently refreshes claims a past
// it no longer holds, and a cache that never refreshes is stale by design.
// Making the refresh explicit keeps the crawl budget a number a person decides
// (AO3's robots.txt asks for 30 s between requests, so a "refresh everything"
// pass is a decision about hours, not milliseconds) and keeps the queue's
// meaning stable: nothing requeues itself.
//
// The queue IS the state, so this command does no I/O beyond the database: it
// moves rows, it does not fetch. `crawl` / `autocrawl` then drain them under
// the same delay, the same retry cap and the same error recording as always.

import (
	"context"
	"fmt"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func runRefresh(ctx context.Context, args []string) error {
	fs := newFlagSet("refresh")
	c := bindConfig(fs)
	var (
		olderThan = fs.Duration("older-than", 30*24*time.Hour,
			"requeue works last fetched longer ago than this (default 720h = 30 days)")
		dryRun = fs.Bool("dry-run", false,
			"report how many works would be requeued, change nothing")
	)
	// NOT finishConfig: it requires --corpus, and every other subcommand needs
	// one because it reads the mirror. This one reads `crawl_queue` and nothing
	// else -- the queue lives in kindred.db -- so demanding a corpus path here
	// would force an operator to name a database this command never opens, and
	// on a host where the corpus is not mounted the queue could not be
	// refreshed at all. The flag is still ACCEPTED (bindConfig attaches it) for
	// a caller that passes the usual set; it is simply not required.
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.DB == "" {
		return fmt.Errorf("refresh: --db is required (the kindred database holding crawl_queue)")
	}
	// A zero or negative window would requeue EVERY fetched work, which is
	// "refetch the entire mirror" — a decision large enough that it should be
	// typed as a number, not arrived at by leaving a flag out.
	if *olderThan <= 0 {
		return fmt.Errorf("refresh: --older-than must be positive (got %s); "+
			"use a large value such as 720h to mean 'the whole mirror'", *olderThan)
	}

	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return err
	}
	defer s.Close()

	before := time.Now().Add(-*olderThan)
	if *dryRun {
		n, err := s.CountStale(ctx, before)
		if err != nil {
			return err
		}
		fmt.Printf("refresh: would requeue %d work(s) last fetched before %s (dry run, nothing changed)\n",
			n, before.UTC().Format(time.RFC3339))
		return nil
	}

	n, err := s.RequeueStale(ctx, before)
	if err != nil {
		return err
	}
	fmt.Printf("refresh: requeued %d work(s) last fetched before %s; the next crawl or autocrawl drains them\n",
		n, before.UTC().Format(time.RFC3339))
	return nil
}
