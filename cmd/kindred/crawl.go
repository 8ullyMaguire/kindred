package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/config"
	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

// runCrawl grows the mirror from AO3.
//
// This is the one subcommand that touches the network, and it is separated from
// `serve` for the reason the binary is split at all: `serve` is offline by
// construction (SPEC §3.2.1) and a crawler wired into it would put an outbound
// request in the request path.
//
// ## -offline exists to be honest, not to be fast
//
// With -offline, no HTTP request is made at all — not even robots.txt, which
// the sibling tool still fetched and which broke the mode's single promise in
// exactly the Cloudflare-blocked case that prompted the flag. The crawl then
// reports every URL as failed with the reason attached, rather than exiting 0
// having done nothing.
//
// ## The delay is read, not assumed
//
// robots.txt states Crawl-delay: 30. That is read at startup and obeyed, and
// an unreadable robots.txt falls back to the same 30s rather than to 0: a
// failed read is not permission to go faster.
func runCrawl(ctx context.Context, args []string) error {
	fs := newFlagSet("crawl")
	var (
		urlsFile   = fs.String("urls", "", "file of work URLs to fetch, one per line ('-' for stdin)")
		seedsFlag  = fs.String("seeds", "", "comma-separated kind:id or AO3 work URLs to seed from")
		statePath  = fs.String("state", "", "resume file; a crawl interrupted mid-run continues from it")
		offline    = fs.Bool("offline", false, "make no network request at all; report every URL as unfetched")
		workers    = fs.Int("workers", 1, "concurrent fetchers (default 1: the crawl delay is the binding constraint)")
		maxRetries = fs.Int("max-retries", 3, "retries for a transient failure")
		backoff    = fs.Duration("retry-backoff", 2*time.Second, "first retry backoff; doubles per attempt")
		checkpoint = fs.Int("checkpoint-urls", 25, "persist resume state every N fetches")
		baseURL    = fs.String("base-url", "", "override the AO3 base URL (for a fixture server)")
		parseOnly  = fs.Bool("parse-only", false, "fetch and parse, print a summary, write nothing")
		// -1 means "not set, read robots.txt". Zero is a REAL value meaning
		// "do not wait", which a fixture server needs: treating 0 as unset
		// meant `--crawl-delay 0` silently waited the 30 seconds AO3 asks for,
		// and the override was unusable exactly where it was most wanted.
		crawlDelay = fs.Duration("crawl-delay", -1,
			"override the crawl delay; -1 (default) reads robots.txt, 0 means no wait")
	)
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}

	urls, err := collectCrawlURLs(*urlsFile, *seedsFlag)
	if err != nil {
		return err
	}
	if len(urls) == 0 {
		return errors.New("crawl: nothing to do; pass --seeds or --urls")
	}

	var st *crawl.State
	if *statePath != "" {
		st, err = crawl.LoadState(*statePath)
		if err != nil {
			return err
		}
	}

	var client crawl.Client
	if *offline {
		client = crawl.NewOffline("-offline was set")
	} else {
		hc := crawl.NewHTTPClient(*baseURL)
		switch {
		case *crawlDelay >= 0:
			// Explicit, including 0. Reported so a reader can tell a
			// deliberate override from a parsed robots.txt.
			hc.SetDelay(*crawlDelay)
			if *crawlDelay == 0 {
				fmt.Fprintln(os.Stderr,
					"crawl: --crawl-delay 0: no wait between requests (fixture use)")
			} else {
				fmt.Fprintf(os.Stderr, "crawl: --crawl-delay %s\n", *crawlDelay)
			}
		default:
			if d, err := hc.LoadRobotsDelay(ctx); err == nil {
				hc.SetDelay(d)
				fmt.Fprintf(os.Stderr, "crawl: robots.txt says %s between requests\n", d)
			} else {
				// Not fatal, and deliberately conservative: the fallback IS
				// the value robots.txt states. An unreadable robots.txt is not
				// permission to go faster.
				fmt.Fprintf(os.Stderr, "crawl: %v; falling back to %s\n",
					err, crawl.DefaultCrawlDelay)
			}
		}
		client = hc
	}
	defer client.Close()

	// The mirror is opened READ-WRITE here, deliberately and only here. Every
	// other subcommand opens it mode=ro (SPEC §3.2.1) because it is a shared
	// artefact; `crawl` is the one command whose job is to grow it.
	//
	// It was previously opened by nothing at all: Run parsed and counted, and
	// no Sink was ever constructed, so a crawl reported "fetched 2" while the
	// mirror gained zero rows. Measured, and the report said nothing wrong.
	var sink crawl.Sink
	if !*parseOnly {
		mirror, err := openMirrorForWrite(c)
		if err != nil {
			return err
		}
		defer mirror.Close()
		sink = crawl.NewSQLSink(mirror)
	}

	var lastBeat time.Time
	cr := crawl.New(client)
	rep, err := cr.Run(ctx, crawl.Options{
		URLs:           urls,
		State:          st,
		StatePath:      *statePath,
		CheckpointURLs: *checkpoint,
		Workers:        *workers,
		MaxRetries:     *maxRetries,
		RetryBackoff:   *backoff,
		Sink:           sink,
		OnProgress: func(done, total int, lastURL string) {
			// Progress on a timer, not per URL: with a 30s delay a per-URL log
			// prints one line per 30 seconds and looks hung.
			if time.Since(lastBeat) < 30*time.Second {
				return
			}
			lastBeat = time.Now()
			fmt.Fprintf(os.Stderr, "crawl: %d/%d  %s\n", done, total, lastURL)
		},
	})
	if err != nil {
		return err
	}

	fmt.Printf("crawl: requested=%d fetched=%d skipped=%d failed=%d retried=%d in %s\n",
		rep.Requested, rep.Fetched, rep.Skipped, rep.Failed, rep.Retried,
		rep.Elapsed.Round(time.Second))
	for u, e := range rep.FailedURLs {
		fmt.Fprintf(os.Stderr, "crawl: failed %s: %s\n", u, e)
	}
	// A fetched-but-unwritten page is neither a success nor a fetch failure,
	// and reporting only the two other numbers makes a crawl that grew the
	// mirror by nothing look like a clean run.
	if rep.Unwritten > 0 {
		fmt.Printf("crawl: WARNING %d fetched pages were NOT written to the mirror\n",
			rep.Unwritten)
		for u, e := range rep.UnwrittenURLs {
			fmt.Fprintf(os.Stderr, "crawl: unwritten %s: %s\n", u, e)
		}
	}
	if *parseOnly {
		return nil
	}
	return writeCrawlReport(ctx, c, rep)
}

// collectCrawlURLs gathers seed and file URLs, deduplicated and in order.
func collectCrawlURLs(urlsFile, seeds string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	for _, s := range strings.Split(seeds, ",") {
		add(normaliseSeedURL(s))
	}

	if urlsFile != "" {
		var f *os.File
		if urlsFile == "-" {
			f = os.Stdin
		} else {
			var err error
			f, err = os.Open(urlsFile)
			if err != nil {
				return nil, fmt.Errorf("crawl: open %s: %w", urlsFile, err)
			}
			defer f.Close()
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			// A pasted bookmark listing is full of junk; a # comment and a bare
			// number are the two forms worth accepting.
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "http") {
				if n := parseBareWorkID(line); n > 0 {
					add(fmt.Sprintf("%s/works/%d", crawl.BaseURL, n))
					continue
				}
			}
			add(line)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("crawl: read %s: %w", urlsFile, err)
		}
	}
	return out, nil
}

// normaliseSeedURL turns "kind:id" or a bare id into a work URL.
func normaliseSeedURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http") {
		return s
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	if n := parseBareWorkID(s); n > 0 {
		return fmt.Sprintf("%s/works/%d", crawl.BaseURL, n)
	}
	return s
}

func parseBareWorkID(s string) int64 {
	s = strings.TrimSpace(s)
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// openMirrorForWrite opens the corpus read-write, for the crawl sink only.
//
// The mode=ro rule everywhere else is not a convention this one overrides for
// convenience: `crawl` is the only command whose purpose is to change the
// mirror, so read-write is the correct mode here and read-only is correct
// everywhere else.
func openMirrorForWrite(c *config.Config) (*sql.DB, error) {
	if c.CorpusDB == "" {
		return nil, errors.New("crawl: no corpus configured; pass --corpus PATH")
	}
	db, err := sql.Open("sqlite", c.CorpusDB)
	if err != nil {
		return nil, fmt.Errorf("crawl: open mirror %s: %w", c.CorpusDB, err)
	}
	return db, nil
}

// writeCrawlReport persists the run summary into the state database, so a
// scheduled crawl leaves a record beyond its stderr.
//
// It is deliberately best-effort: a crawl whose works are written must not be
// reported as a failure because a summary line could not be appended.
func writeCrawlReport(ctx context.Context, c *config.Config, rep *crawl.Report) error {
	if c.DB == "" {
		return nil
	}
	db, err := sql.Open("sqlite", c.DB)
	if err != nil {
		return nil //nolint:nilerr // best-effort by design; see the doc comment
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS crawl_runs(
			at TEXT NOT NULL, requested INTEGER NOT NULL, fetched INTEGER NOT NULL,
			skipped INTEGER NOT NULL, failed INTEGER NOT NULL, elapsed_s REAL NOT NULL)`); err != nil {
		return nil //nolint:nilerr // best-effort by design
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO crawl_runs(at,requested,fetched,skipped,failed,elapsed_s)
		 VALUES(?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), rep.Requested, rep.Fetched,
		rep.Skipped, rep.Failed, rep.Elapsed.Seconds()); err != nil {
		return nil //nolint:nilerr // best-effort by design
	}
	return nil
}
