package crawl

import (
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ParsedWork is what a work page yields.
type ParsedWork struct {
	ID         int64
	Title      string
	Authors    string
	Summary    string
	URL        string
	WordCount  int64
	Kudos      int64
	Hits       int64
	Bookmarks  *int64
	Chapters   string
	Language   string
	Complete   bool
	Rating     string
	UpdateDate string
	Tags       []ParsedTag
}

// ParsedTag is one tag, with the category AO3 gave it.
//
// The category matters and is preserved. AO3's own tag types (Fandom,
// Relationship, Character, Genre, Freeform) are the only authoritative source
// of what a tag IS in this corpus, and the sibling tool measured 3,890,504 of
// 3,891,300 rows as 'freeforms' — so the mirror's tag_type column is NOT the
// site's classification and must not be used to infer one. Whatever this
// parser reads off the page is the site saying so directly.
type ParsedTag struct {
	Name string
	Type string
}

var (
	// The blurb meta block AO3 puts on every work page.
	metaTitleRe  = regexp.MustCompile(`(?is)<meta\s+name="works:title"\s+content="([^"]*)"`)
	metaAuthorRe = regexp.MustCompile(`(?is)<meta\s+name="works:author"\s+content="([^"]*)"`)
	metaWordsRe  = regexp.MustCompile(`(?is)<meta\s+name="works:words"\s+content="([\d,]+)"`)
	metaKudosRe  = regexp.MustCompile(`(?is)<meta\s+name="works:kudos"\s+content="([\d,]+)"`)
	metaHitsRe   = regexp.MustCompile(`(?is)<meta\s+name="works:hits"\s+content="([\d,]+)"`)
	metaBkmkRe   = regexp.MustCompile(`(?is)<meta\s+name="works:bookmarks"\s+content="([\d,]+)"`)
	metaChapRe   = regexp.MustCompile(`(?is)<meta\s+name="works:chapters"\s+content="([^"]*)"`)
	metaLangRe   = regexp.MustCompile(`(?is)<div class="language">\s*([^<]*)`)
	metaStatusRe = regexp.MustCompile(`(?is)<span[^>]*class="status"\s*>([^<]*)</span>`)

	// The blurb block is quoted in prose and can contain any character, so the
	// summary is read from the blockquote rather than a meta tag.
	summaryRe = regexp.MustCompile(`(?is)<div class="summary[^\"]*"[^>]*>\s*<blockquote[^>]*>(.*?)</blockquote>`)

	// Tag rows. AO3 wraps each in <li class="freeforms"> / <li class="relationships">
	// / <li class="characters"> and links the name in <a class="tag">.
	tagRowRe  = regexp.MustCompile(`(?is)<li\s+class="([a-z]+)">(.*?)</li>`)
	tagNameRe = regexp.MustCompile(`(?is)<a\s+class="tag"[^>]*>(.*?)</a>`)
)

// ParseWork extracts a work from an AO3 work page.
//
// It returns an error when the page has no title meta tag, because that is the
// signal that the fetch did not get a work page at all — a login wall, a 404
// rendered as HTML, or a Cloudflare page that slipped past IsInterstitial.
// Writing a zero-valued work for one of those poisons the mirror with a row
// that has no id and no title, and nothing downstream can tell it apart from a
// real sparse work.
func ParseWork(body []byte, url string) (*ParsedWork, error) {
	w := &ParsedWork{URL: url, Language: "English"}

	m := metaTitleRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("crawl: no works:title meta tag; not a work page (%s)", url)
	}
	w.Title = html.UnescapeString(string(m[1]))
	w.ID = WorkIDFromURL(url)

	if m := metaAuthorRe.FindSubmatch(body); m != nil {
		w.Authors = html.UnescapeString(string(m[1]))
	}
	if m := metaWordsRe.FindSubmatch(body); m != nil {
		w.WordCount = parseCount(string(m[1]))
	}
	if m := metaKudosRe.FindSubmatch(body); m != nil {
		w.Kudos = parseCount(string(m[1]))
	}
	if m := metaHitsRe.FindSubmatch(body); m != nil {
		w.Hits = parseCount(string(m[1]))
	}
	// Bookmarks are a POINTER for the same reason the corpus column is NULLable:
	// AO3 omits the meta tag entirely when a work has none, and 0 is a real
	// value with a different meaning.
	if m := metaBkmkRe.FindSubmatch(body); m != nil {
		v := parseCount(string(m[1]))
		w.Bookmarks = &v
	}
	if m := metaChapRe.FindSubmatch(body); m != nil {
		w.Chapters = html.UnescapeString(string(m[1]))
	}
	if m := metaLangRe.FindSubmatch(body); m != nil {
		if l := strings.TrimSpace(html.UnescapeString(string(m[1]))); l != "" {
			w.Language = l
		}
	}
	if m := metaStatusRe.FindSubmatch(body); m != nil {
		st := strings.ToLower(strings.TrimSpace(string(m[1])))
		w.Complete = strings.Contains(st, "completed")
	}
	if m := summaryRe.FindSubmatch(body); m != nil {
		w.Summary = cleanText(string(m[1]))
	}
	w.Tags = parseTags(body)
	if len(w.Tags) == 0 {
		return nil, fmt.Errorf("crawl: work page parsed with zero tags (%s)", url)
	}
	return w, nil
}

// parseTags reads the tag list with AO3's categories preserved.
func parseTags(body []byte) []ParsedTag {
	var out []ParsedTag
	seen := map[string]bool{}
	for _, row := range tagRowRe.FindAllSubmatch(body, -1) {
		typ := strings.ToLower(strings.TrimSpace(string(row[1])))
		if typ == "" {
			continue
		}
		for _, nm := range tagNameRe.FindAllSubmatch(row[2], -1) {
			name := cleanText(string(nm[1]))
			if name == "" {
				continue
			}
			key := typ + "\x00" + strings.ToLower(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, ParsedTag{Name: name, Type: typ})
		}
	}
	return out
}

// parseCount reads "12,345" as 12345. A non-numeric string yields 0 rather than
// an error, because AO3 renders "-" for a work with no chapters and an absent
// metric should not abort a parse.
func parseCount(s string) int64 {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "-" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// cleanText strips tags and collapses whitespace, for text that came from
// inside an HTML element.
func cleanText(s string) string {
	s = reTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

var reTagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// ---------------------------------------------------------------- crawling

// Options configures a crawl run.
type Options struct {
	// URLs to fetch. Already-expanded; this package does no seed discovery.
	URLs []string
	// State is the resume file. Nil means no resume (every URL is fetched).
	State *State
	// StatePath, when set, is written every CheckpointURLs fetches.
	StatePath string
	// CheckpointURLs is how often to persist state. The sibling tool's crawl
	// only checkpointed at the end of a seed's BFS, which on a 171-bookmarker
	// seed meant the first checkpoint was an hour out.
	CheckpointURLs int
	// Workers is the fetch concurrency. 1 by default because the crawl-delay is
	// the binding constraint and concurrency only buys parallelism the site
	// will not honour.
	Workers int
	// MaxRetries bounds retries of a transient failure.
	MaxRetries int
	// RetryBackoff is the first backoff; it doubles per attempt.
	RetryBackoff time.Duration
	// OnProgress is called after each fetch.
	OnProgress func(done, total int, lastURL string)
	// ProgressEvery throttles OnProgress.
	ProgressEvery time.Duration

	// Sink receives every successfully parsed work.
	//
	// It is nil-safe: with no sink a run parses and counts but writes nothing,
	// which is what --parse-only wants. It has to be nil-safe rather than
	// required because "fetch and check the parse" is a real mode and forcing
	// a sink on it would mean the mode could not be exercised without a
	// database.
	Sink Sink
}

// Report is the outcome of a run.
type Report struct {
	Requested int
	Fetched   int
	Skipped   int
	Failed    int
	Retried   int
	Elapsed   time.Duration
	// FailedURLs pairs a URL with its last error, for the operator. A count
	// alone cannot be acted on: "4 failed" does not say which four.
	FailedURLs map[string]string

	// Unwritten counts pages that were fetched and parsed but NOT persisted.
	//
	// It is a separate counter from Failed because conflating them hides a
	// real and common case: a crawl whose sink is a full disk or a locked
	// database reports "fetched 690, failed 0" and looks like a complete run
	// that grew the mirror by nothing.
	Unwritten int
	// UnwrittenURLs pairs a URL with its write error.
	UnwrittenURLs map[string]string
}

// Crawler runs an Options against a Client.
type Crawler struct {
	client Client
	// sleep is injected so tests do not actually wait 30 seconds per request.
	// With a real client it is a no-op and the client's own delay applies.
	sleep func(ctx context.Context, d time.Duration) error
}

// New returns a crawler over a client.
func New(client Client) *Crawler {
	return &Crawler{client: client, sleep: sleepCtx}
}

// WithSleep replaces the sleep function. Tests use it to run a crawl whose
// client has a long delay without spending that long.
func (c *Crawler) WithSleep(f func(context.Context, time.Duration) error) *Crawler {
	c.sleep = f
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Run fetches every URL in opts, honouring resume and the crawl delay.
func (c *Crawler) Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.CheckpointURLs <= 0 {
		opts.CheckpointURLs = 25
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 3
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = 2 * time.Second
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 5 * time.Second
	}

	start := time.Now()
	rep := &Report{Requested: len(opts.URLs), FailedURLs: map[string]string{}}

	// Resume filter first, so the skipped ones are never scheduled.
	var todo []string
	for _, u := range opts.URLs {
		if opts.State != nil && opts.State.Done(u) {
			rep.Skipped++
			continue
		}
		todo = append(todo, u)
	}
	if len(todo) == 0 {
		return rep, nil
	}

	type result struct {
		url  string
		id   int64
		work *ParsedWork
		err  error
	}
	results := make(chan result, len(todo))

	// A single shared index makes the checkpoint count exact regardless of
	// worker count, which a per-worker counter does not.
	var (
		mu       sync.Mutex
		done     int
		lastBeat time.Time
		lastURL  string
	)

	sem := make(chan struct{}, opts.Workers)
	var wg sync.WaitGroup
	for _, u := range todo {
		wg.Add(1)
		go func(rawURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := c.sleep(ctx, c.client.Delay()); err != nil {
				results <- result{url: rawURL, err: err}
				return
			}
			body, err := c.fetchWithRetry(ctx, rawURL, opts, &mu, &rep.Retried)
			if err != nil {
				if opts.State != nil {
					opts.State.MarkError(rawURL, err)
				}
				results <- result{url: rawURL, err: err}
				return
			}
			w, err := ParseWork(body, rawURL)
			if err != nil {
				if opts.State != nil {
					opts.State.MarkError(rawURL, err)
				}
				results <- result{url: rawURL, err: err}
				return
			}
			if opts.State != nil {
				opts.State.Mark(rawURL, w.ID)
			}
			results <- result{url: rawURL, id: w.ID, work: w}
		}(u)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	lastBeat = time.Now()
	for r := range results {
		done++
		if r.err != nil {
			rep.Failed++
			rep.FailedURLs[r.url] = r.err.Error()
		} else {
			// The write happens HERE, in the single collection goroutine, not
			// in the worker. Workers parse concurrently; the sink owns a
			// transaction per work and a shared transaction from N goroutines
			// is how a mirror ends up with half-written rows after a crash.
			//
			// A sink failure does NOT make the fetch a failure: the page was
			// fetched and parsed, and reporting "fetched" would be a lie only
			// about the write. So it is counted and surfaced separately.
			if opts.Sink != nil && r.work != nil {
				if err := opts.Sink.Save(ctx, r.work); err != nil {
					rep.Unwritten++
					if rep.UnwrittenURLs == nil {
						rep.UnwrittenURLs = map[string]string{}
					}
					rep.UnwrittenURLs[r.url] = err.Error()
				}
			}
			rep.Fetched++
		}
		lastURL = r.url

		mu.Lock()
		shouldProgress := time.Since(lastBeat) >= opts.ProgressEvery
		if shouldProgress {
			lastBeat = time.Now()
		}
		mu.Unlock()
		if shouldProgress && opts.OnProgress != nil {
			opts.OnProgress(done, len(todo), lastURL)
		}
		if opts.State != nil && opts.StatePath != "" && done%opts.CheckpointURLs == 0 {
			if err := opts.State.Save(opts.StatePath); err != nil {
				return rep, err
			}
		}
	}

	// Final save, so a run that ends cleanly does not rely on the last
	// checkpoint landing exactly on a multiple.
	if opts.State != nil && opts.StatePath != "" {
		if err := opts.State.Save(opts.StatePath); err != nil {
			return rep, err
		}
	}
	rep.Elapsed = time.Since(start)
	return rep, nil
}

// fetchWithRetry retries transient failures with exponential backoff.
//
// The retry ladder caps at five minutes per attempt because AO3's 525s
// cluster can outlast anything shorter, and a shorter cap turns a recoverable
// blip into a permanent loss of that work.
func (c *Crawler) fetchWithRetry(ctx context.Context, rawURL string, opts Options, mu *sync.Mutex, retried *int) ([]byte, error) {
	backoff := opts.RetryBackoff
	var lastErr error
	for attempt := 0; attempt <= opts.MaxRetries; attempt++ {
		if attempt > 0 {
			mu.Lock()
			*retried++
			mu.Unlock()
			if err := c.sleep(ctx, backoff); err != nil {
				return nil, err
			}
			backoff *= 2
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
		}
		body, err := c.client.Fetch(ctx, rawURL)
		if err == nil {
			return body, nil
		}
		lastErr = err
		// A refusal is not a failure. Retrying `ErrOffline` three times with
		// backoff turns "you asked me not to use the network" into a 14-second
		// stall per URL, which is how an offline mode ends up looking hung --
		// measured, and it is why this is a distinct branch rather than a
		// fall-through to the retry ladder.
		if errors.Is(err, ErrOffline) {
			return nil, err
		}
		// A 404 is permanent: retrying it three times costs 6 seconds and
		// returns the same 404.
		if strings.Contains(err.Error(), "not found") {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}
