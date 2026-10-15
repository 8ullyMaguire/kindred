package main

// `autocrawl` — drain the crawl queue once, bounded, and exit.
//
// Why this exists as a command rather than only as serve's goroutine:
//
// serve's unit is deliberately sandboxed (`IPAddressDeny=any` with
// `IPAddressAllow=localhost`), which is right for a read-only mirror and fatal
// for a crawler. Measured on the live instance 2026-10-14: 520 of 521 queue
// rows parked after five attempts, every `last_error` an `i/o timeout` on the
// dial. So the queue — the whole auto-crawl feature — was honest and inert in
// production, and would have been even if the parser had worked.
//
// Two ways out, and the choice is deliberate:
//
//  1. Let archiveofourown.org through the web unit's sandbox. That gives up the
//     guarantee the unit's own comment states ("should not be reaching the
//     network"), for a process whose job is serving readers.
//  2. Keep the web unit exactly as it is, and drain the queue from a separate
//     unit that is allowed to dial out.
//
// This is (2). The read-only unit keeps its guarantee, the crawler gets the
// same queue, the same retry cap, the same per-row error recording and the same
// robots.txt delay as everything else, and `--max` bounds a run so a timer can
// own it (the default run at the 30 s crawl delay is about twelve minutes).
//
// The queue is the state, so there is nothing to resume: a run that stops at
// its bound leaves the remaining rows undoned and the next tick continues.

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func runAutoCrawlCmd(ctx context.Context, args []string) error {
	fs := newFlagSet("autocrawl")
	c := bindConfig(fs)
	var (
		max = fs.Int("max", 25, "stop after this many works (0 = drain until the queue is empty)")
		// --base-url is the same fixture-server override `crawl` and
		// `discover` already take, and it is here because this is the command
		// the timer runs: redirecting the base URL is the only way to prove
		// the drain fetches, parses and stores a work on a host AO3 refuses.
		baseURL = fs.String("base-url", "", "override the AO3 base URL (for a fixture server)")
	)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}
	if *max < 0 {
		return fmt.Errorf("autocrawl: --max cannot be negative")
	}

	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return err
	}
	defer s.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *max > 0 {
		log.Info("autocrawl: starting a bounded drain", "max", *max)
	}
	runAutoCrawlMax(ctx, s, c, log, *max, *baseURL)
	return nil
}
