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

// Every pattern in this block was written from a **live** AO3 page, fetched
// 2026-10-14: `?view_full_work=true` returns 1,036,147 bytes for the work page
// and a bare `/works/<id>` returns a 302 to `/works/<id>/chapters/<cid>` and
// 44,920 bytes there. The chapter page — which is what a crawl of `/works/<id>`
// actually receives, because Go follows the redirect — carries every field
// below, so nothing here needs the 1 MB view.
//
// The revision before this one read `<meta name="works:title">`,
// `<meta name="works:words">`, `<div class="language">`, `<span class="status">`
// and `<li class="freeforms">`. **AO3 sends none of those**, on the work page or
// the chapter page: the meta block carries only viewport/subject/language/
// keywords/format-detection/distribution/description/csrf-*/classification/
// chrome/author, the counts live in `<dt>/<dd>` pairs, and the tag rows are
// `<dd>`s. So every real page was refused as "not a work page", and the tests
// stayed green because their fixture invented the tags this parser was asking
// for. A pattern nothing has ever matched is a claim, not a parser.
var (
	// Title: <h2 class="title heading"> Name </h2>
	workTitleRe = regexp.MustCompile(`(?is)<h2[^>]*class="[^"]*\btitle\b[^"]*"[^>]*>(.*?)</h2>`)

	// Authors: one <a rel="author" href="/users/U/pseuds/P">P</a> per author.
	// The rel attribute is the site's own; the byline also appears in
	// `name="author"` as a comma-joined string, but that collapses the
	// user/pseud pair the author page needs.
	authorNameRe = regexp.MustCompile(`(?is)<a\s+rel="author"[^>]*>(.*?)</a>`)

	// Metrics. AO3 renders each as a definition pair —
	//   <dt class="words">Words:</dt><dd class="words">155,360</dd>
	// — and every class below appears exactly once on the page, so reading the
	// `<dd>` by class is unambiguous. A metric the work lacks (kudos on a new
	// work, bookmarks on one nobody bookmarked) is ABSENT rather than zero,
	// which is why bookmarks keeps the nil-vs-0 distinction in ParsedWork.
	ddClassRe = regexp.MustCompile(`(?is)<dd[^>]*class="([^"]*)"[^>]*>(.*?)</dd>`)

	// Summary: <div class="summary module"> … <blockquote class="userstuff">
	// The blockquote is the content; the div also holds an <h3>Summary:</h3>.
	summaryRe = regexp.MustCompile(`(?is)<div[^>]*class="[^"]*\bsummary\b[^"]*"[^>]*>.*?<blockquote[^>]*>(.*?)</blockquote>`)

	// Tag rows. AO3 wraps each category in its own <dd>, and the plural in the
	// class is the site's classification:
	//   <dd class="fandom tags"><ul class="commas"><li><a class="tag" …>
	// The seven classes are rating, warning, category, fandom, relationship,
	// character, freeform. This is the authoritative tag_type — see
	// ParsedTag's doc comment for why the mirror's column is not.
	tagRowRe  = regexp.MustCompile(`(?is)<dd[^>]*class="([a-z]+)\s+tags?"[^>]*>(.*?)</dd>`)
	tagNameRe = regexp.MustCompile(`(?is)<a[^>]*class="tag"[^>]*>(.*?)</a>`)
)

// tagTypePlural maps the class AO3 uses to the vocabulary this codebase stores,
// which is plural throughout ('freeforms' in work_tags, 'fandoms' in the
// engine's fandom filter, and both spellings accepted by TagTypeIsFandom).
// A singular that is not in the table is passed through unchanged rather than
// guessed at, so an unanticipated row shows up as itself.
var tagTypePlural = map[string]string{
	"rating":       "ratings",
	"warning":      "warnings",
	"category":     "categories",
	"fandom":       "fandoms",
	"relationship": "relationships",
	"character":    "characters",
	"freeform":     "freeforms",
}

// ParseWork extracts a work from an AO3 work page.
//
// It returns an error when the page carries no title heading, because that is
// the signal the fetch did not get a work page at all — a login wall, a 404
// rendered as HTML, the adult-content gate, or a Cloudflare page that slipped
// past IsInterstitial. `<h2 class="title heading">` is the marker because a
// real work page always has exactly one and none of those do. Writing a
// zero-valued work for one of them poisons the mirror with a row that has no id
// and no title, and nothing downstream can tell it apart from a real sparse
// work.
//
// The marker used to be `<meta name="works:title">`, which AO3 does not send —
// see the block comment on the patterns above.
func ParseWork(body []byte, url string) (*ParsedWork, error) {
	w := &ParsedWork{URL: url, Language: "English"}
	w.ID = WorkIDFromURL(url)

	m := workTitleRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("crawl: no title heading; not a work page (%s)", url)
	}
	w.Title = cleanText(string(m[1]))

	// Authors, in page order. A work with several authors has one byline link
	// each; joined with ", " to match the corpus, which stores the byline as
	// text.
	var authors []string
	for _, a := range authorNameRe.FindAllSubmatch(body, -1) {
		if name := cleanText(string(a[1])); name != "" {
			authors = append(authors, name)
		}
	}
	w.Authors = strings.Join(authors, ", ")

	stats := statValues(body)
	// Every value goes through cleanText first: AO3 links some of them
	// (`<dd class="bookmarks"><a href="/works/896/bookmarks">122</a></dd>`), so a
	// raw parseCount on those reads 0 — which is a *real* value for bookmarks and
	// therefore indistinguishable from "nobody bookmarked it".
	w.WordCount = parseCount(cleanText(stats["words"]))
	w.Kudos = parseCount(cleanText(stats["kudos"]))
	w.Hits = parseCount(cleanText(stats["hits"]))
	w.Chapters = cleanText(stats["chapters"])
	// Bookmarks are a POINTER for the same reason the corpus column is NULLable:
	// AO3 omits the row entirely when a work has none, and 0 is a real value
	// with a different meaning.
	if raw, ok := stats["bookmarks"]; ok {
		v := parseCount(cleanText(raw))
		w.Bookmarks = &v
	}
	if lang := cleanText(stats["language"]); lang != "" {
		w.Language = lang
	}
	if upd := cleanText(stats["status"]); upd != "" {
		w.UpdateDate = upd
	}
	w.Complete = chaptersAreComplete(w.Chapters)

	if m := summaryRe.FindSubmatch(body); m != nil {
		w.Summary = cleanText(string(m[1]))
	}

	w.Tags = parseTags(body)
	if len(w.Tags) == 0 {
		return nil, fmt.Errorf("crawl: work page parsed with zero tags (%s)", url)
	}
	for _, t := range w.Tags {
		if t.Type == "ratings" {
			w.Rating = t.Name
			break
		}
	}
	return w, nil
}

// statValues reads the page's definition pairs into class → inner HTML.
//
// First occurrence wins: the work's stats block is the first thing on the page
// that uses these classes, and the chapter list below it repeats some of them
// for its own per-chapter counts.
func statValues(body []byte) map[string]string {
	out := map[string]string{}
	for _, m := range ddClassRe.FindAllSubmatch(body, -1) {
		cls := strings.TrimSpace(string(m[1]))
		if _, seen := out[cls]; seen {
			continue
		}
		out[cls] = string(m[2])
	}
	return out
}

// chaptersAreComplete reads AO3's chapter fraction: "55/?" is a work in
// progress and "1/1" is complete. Both halves must be numbers and equal, so an
// unknown total ("?/?") is reported as NOT complete — the safe direction, since
// a mirror that marks a work in progress complete stops re-checking it.
func chaptersAreComplete(fraction string) bool {
	parts := strings.Split(strings.TrimSpace(fraction), "/")
	if len(parts) != 2 {
		return false
	}
	posted, errPosted := strconv.Atoi(strings.TrimSpace(parts[0]))
	total, errTotal := strconv.Atoi(strings.TrimSpace(parts[1]))
	return errPosted == nil && errTotal == nil && posted == total
}

// parseTags reads the tag list with AO3's categories preserved.
//
// The class singular is mapped to this codebase's plural vocabulary (see
// tagTypePlural); an unmapped singular is kept as-is rather than dropped, so a
// category AO3 adds later surfaces as itself instead of vanishing.
func parseTags(body []byte) []ParsedTag {
	var out []ParsedTag
	seen := map[string]bool{}
	for _, row := range tagRowRe.FindAllSubmatch(body, -1) {
		typ := strings.ToLower(strings.TrimSpace(string(row[1])))
		if typ == "" {
			continue
		}
		if plural, ok := tagTypePlural[typ]; ok {
			typ = plural
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

	// OnWork, when set, is called with every successfully parsed work.
	//
	// It exists because `--parse-only` has always promised "fetch and parse,
	// print a summary" and had no way to print one: the report carries counts,
	// so an operator asking "does this parser work on a real page?" got
	// `fetched=1` and nothing about the work fetched. That question is the
	// reason the flag exists, and it went unanswered while the parser was
	// rejecting every real AO3 page.
	//
	// Called from the fetch goroutine, so an implementation that touches shared
	// state must synchronise.
	OnWork func(*ParsedWork)

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

// Defaults for a run that leaves a knob unset. They are constants so Run and
// FetchOne cannot drift apart: a BFS that retried differently from a plain
// crawl would be two politeness policies pointed at one host.
const (
	DefaultMaxRetries    = 3
	DefaultRetryBackoff  = 2 * time.Second
	DefaultWorkers       = 1
	DefaultCheckpoint    = 25
	DefaultProgressEvery = 5 * time.Second
)

// FetchOne fetches a single URL under the crawler's delay and retry policy.
//
// It exists for callers that cannot name their URLs up front — a BFS discovers
// the next URL by fetching the current one, so it cannot hand Run a list. The
// alternative was for the BFS to call client.Fetch directly, which would have
// skipped both the crawl delay and the transient-error retry, and would have
// looked correct in every test that used a fake client.
func (c *Crawler) FetchOne(ctx context.Context, rawURL string) ([]byte, error) {
	if err := c.sleep(ctx, c.client.Delay()); err != nil {
		return nil, err
	}
	var (
		mu      sync.Mutex
		retried int
	)
	return c.fetchWithRetry(ctx, rawURL, Options{
		MaxRetries:   DefaultMaxRetries,
		RetryBackoff: DefaultRetryBackoff,
	}, &mu, &retried)
}

// Run fetches every URL in opts, honouring resume and the crawl delay.
func (c *Crawler) Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.CheckpointURLs <= 0 {
		opts.CheckpointURLs = DefaultCheckpoint
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = DefaultMaxRetries
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = DefaultRetryBackoff
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = DefaultProgressEvery
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
			if opts.OnWork != nil {
				opts.OnWork(w)
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
