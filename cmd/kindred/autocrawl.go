package main

// serve's auto-crawler: the mirror grows while it is used.
//
// SPEC's original rule was that `serve` makes no outbound request at all,
// and the rule survives in its real form here: the REQUEST PATH never
// touches the network. One goroutine does, on a queue, at the crawl delay
// robots.txt states (30s, and a failed robots read keeps 30s rather than
// dropping to zero -- see crawl.LoadRobotsDelay). --no-crawl turns the
// goroutine off for an operator who wants the original guarantee literally.
//
// The queue is state.db's crawl_queue, which is also the checkpoint: one row
// per work, done_at per work, restart continues from the undoned rows only.
// Existing metadata is skipped without a fetch -- checked against the corpus
// before any request is made, because "skipping metadata that already
// exists" is the whole point of the check being first.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/config"
	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// autoCrawlRetries and autoCrawlBackoff bound a single job's retries. They
// are variables rather than constants so the failure-path test can run in
// milliseconds instead of sleeping the production backoff; production uses
// the values below (two retries, 10s doubling) because a transient AO3
// error deserves a second and third look, not a parked job.
var (
	autoCrawlRetries = 2
	autoCrawlBackoff = 10 * time.Second
)

// autoCrawlPoll is how often the worker checks the queue for new work when
// it was empty. Reader requests arrive at any moment; thirty seconds of
// latency before a just-queued work starts fetching is invisible next to
// the crawl delay itself.
const autoCrawlPoll = 30 * time.Second

// workURLRe extracts a work id from a queue URL. The queue only ever holds
// work URLs (the two producers build them from ids), so a non-matching URL
// is a bug upstream and is treated as an unknown id rather than guessed at.
var workURLRe = regexp.MustCompile(`/works/(\d{1,12})`)

// runAutoCrawl drains the crawl queue until ctx is cancelled.
//
// Every failure here degrades to "try later", never to a crash: this is a
// background improvement loop, and a crawler that takes serve down because
// a database was locked would be worse than no crawler.
func runAutoCrawl(ctx context.Context, s *store.Store, c *config.Config, log *slog.Logger) {
	if s == nil {
		return
	}

	// The mirror is opened read-write HERE and only here on the serve path,
	// for the same reason the `crawl` subcommand is the only other writer:
	// growing the mirror is the one job that requires it.
	mirror, err := openMirrorForWrite(c)
	if err != nil {
		log.Warn("auto-crawl disabled: cannot open the mirror for writing",
			"err", err, "hint", "pass --corpus PATH")
		return
	}
	defer mirror.Close()

	client := crawl.NewHTTPClient("")
	// The delay is read from robots.txt, not assumed; the error is logged
	// and the returned delay used regardless, because LoadRobotsDelay's
	// contract is that failure yields the conservative default.
	delay, derr := client.LoadRobotsDelay(ctx)
	client.SetDelay(delay)
	if derr != nil {
		log.Warn("auto-crawl: robots.txt unreadable; using the conservative delay",
			"delay", delay, "err", derr)
	} else {
		log.Info("auto-crawl: started", "delay", delay, "poll", autoCrawlPoll)
	}
	defer client.Close()

	// The startup input: works the mirror already holds without a summary.
	// Bounded per start so a very incomplete mirror cannot spend its first
	// hour queueing instead of fetching; the worker drains and the next
	// start queues more if any remain.
	if n, err := s.EnqueueIncompleteWorks(ctx, 500); err != nil {
		log.Warn("auto-crawl: could not queue incomplete works", "err", err)
	} else if n > 0 {
		log.Info("auto-crawl: queued incomplete works", "queued", n)
	}

	cr := crawl.New(client)
	sink := crawl.NewSQLSink(mirror)
	ticker := time.NewTicker(autoCrawlPoll)
	defer ticker.Stop()

	for {
		job, ok, err := s.NextCrawlJob(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("auto-crawl: queue read failed", "err", err)
		}
		if ok {
			processAutoCrawlJob(ctx, s, mirror, sink, cr, job, log)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// processAutoCrawlJob fetches one queued work (or skips it honestly) and
// records the outcome. Returns nothing: every path is logged, none panics,
// and the queue row's state is the durable record.
func processAutoCrawlJob(
	ctx context.Context,
	s *store.Store,
	mirror *sql.DB,
	sink crawl.Sink,
	cr *crawl.Crawler,
	job store.CrawlJob,
	log *slog.Logger,
) {
	if ctx.Err() != nil {
		return
	}

	// Skip rule FIRST, before any request: if the mirror already holds
	// this work with a summary, there is nothing to gather and the fetch
	// would be pure cost. The row is marked done, so a completed work
	// never re-enters the queue from this source either.
	id := int64(0)
	if m := workURLRe.FindStringSubmatch(job.URL); m != nil {
		id, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if id > 0 && mirror != nil {
		var summary sql.NullString
		err := mirror.QueryRowContext(ctx,
			`SELECT summary FROM works WHERE id = ?`, id).Scan(&summary)
		if err == nil && summary.Valid && summary.String != "" {
			if ferr := s.FinishCrawlJob(ctx, job.URL, ""); ferr != nil {
				log.Warn("auto-crawl: could not mark job done", "url", job.URL, "err", ferr)
			}
			log.Debug("auto-crawl: skipped, metadata exists", "work", id, "source", job.Source)
			return
		}
		if err != nil && err != sql.ErrNoRows {
			// A locked or unreadable mirror must not be papered over as
			// "not present": fail the attempt with the reason and let the
			// retry cap park it if the problem persists.
			if ferr := s.FinishCrawlJob(ctx, job.URL, "mirror check: "+err.Error()); ferr != nil {
				log.Warn("auto-crawl: could not record failure", "url", job.URL, "err", ferr)
			}
			return
		}
	}

	// One URL per Run: retries, the crawl delay and the sink transaction
	// all come from the same code the batch crawl uses, so serve's crawler
	// cannot drift from the tool that built the mirror in the first place.
	rep, err := cr.Run(ctx, crawl.Options{
		URLs:           []string{job.URL},
		Sink:           sink,
		MaxRetries:     autoCrawlRetries,
		RetryBackoff:   autoCrawlBackoff,
		Workers:        1,
		CheckpointURLs: 1,
	})
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; the row stays undoned and resumes later
		}
		if ferr := s.FinishCrawlJob(ctx, job.URL, err.Error()); ferr != nil {
			log.Warn("auto-crawl: could not record failure", "url", job.URL, "err", ferr)
		}
		return
	}

	switch {
	case rep.Unwritten > 0:
		msg := rep.UnwrittenURLs[job.URL]
		if ferr := s.FinishCrawlJob(ctx, job.URL, "write: "+msg); ferr != nil {
			log.Warn("auto-crawl: could not record write failure", "url", job.URL, "err", ferr)
		}
		log.Warn("auto-crawl: fetched but not written", "url", job.URL, "err", msg)
	case rep.Failed > 0:
		msg := rep.FailedURLs[job.URL]
		if ferr := s.FinishCrawlJob(ctx, job.URL, msg); ferr != nil {
			log.Warn("auto-crawl: could not record failure", "url", job.URL, "err", ferr)
		}
		log.Warn("auto-crawl: fetch failed", "url", job.URL, "attempt", job.Attempts+1, "err", msg)
	case rep.Fetched > 0:
		if ferr := s.FinishCrawlJob(ctx, job.URL, ""); ferr != nil {
			log.Warn("auto-crawl: could not mark job done", "url", job.URL, "err", ferr)
		}
		log.Info("auto-crawl: mirrored a work",
			"url", job.URL, "source", job.Source, "attempt", job.Attempts+1)
	default:
		// Requested but neither fetched nor failed nor unwritten should be
		// unreachable; record it rather than silently completing, because
		// a row that vanishes without a fetch is the one outcome that
		// cannot be diagnosed later.
		if ferr := s.FinishCrawlJob(ctx, job.URL,
			fmt.Sprintf("no outcome: %+v", rep)); ferr != nil {
			log.Warn("auto-crawl: could not record odd outcome", "url", job.URL, "err", ferr)
		}
	}
}
