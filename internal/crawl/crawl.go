// Package crawl fetches AO3 pages into the same local mirror kindred already
// reads.
//
// ## Why this package exists at all
//
// The Python sibling (ao3-recommender) grows the mirror by crawling AO3, and
// its crawl is the source of the two facts this whole project is built on: the
// 112,935 works and the 7.75M tag co-occurrence edges. kindred inherited the
// mirror but not the means of growing it, which meant every corpus question
// could only ever be answered about a snapshot someone else collected.
//
// ## The crawl-delay is the design constraint, not an implementation detail
//
// AO3's robots.txt carries `Crawl-delay: 30`, and the Python tool reads the
// FIRST such line regardless of which User-agent block it sits in — so every
// fresh request costs 30s plus jitter. Measured on the sibling: 690 requests
// is about 6 hours. Any design here that assumes it can fan out is wrong, so:
//
//   - one sequential worker by default; concurrency is a flag but defaults to 1
//   - the delay is read from robots.txt, not hardcoded, so a change upstream
//     is obeyed rather than assumed away
//   - 525 and 503 from Cloudflare are treated as TRANSIENT and retried with
//     backoff. Measured: ~50% of raw probes returned the 8,706-byte 525
//     interstitial, with and without a browser UA, and it is not a ban.
//
// ## Resume is the difference between a 6-hour job and a lost afternoon
//
// State is a JSON file naming every completed work. A crash writes nothing
// else: on restart the crawler reloads the file and skips what is done. The
// Python tool learned this the hard way — its checkpoint only saved after the
// seed's BFS completed, so a crash an hour in restarted from zero.
//
// ## This package does NOT decide what to crawl
//
// It fetches URLs and records what came back. Seed expansion, BFS over
// bookmarkers, and depth limits belong to a policy layer above it, because
// those decisions are about taste and this one is about HTTP.
package crawl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BaseURL is AO3's production host.
const BaseURL = "https://archiveofourown.org"

// DefaultCrawlDelay is the fallback when robots.txt cannot be read.
//
// It is 30 seconds, matching what AO3's robots.txt actually says, and it is a
// FALLBACK rather than the default: crawling faster than the site asks is a
// decision this package does not get to make silently, so a failed robots.txt
// read means "be conservative", not "be fast".
const DefaultCrawlDelay = 30 * time.Second

// UserAgent identifies the crawler. It is honest about being a bot, which is
// both required by the crawl-delay agreement and the reason the site is
// willing to answer at all.
const UserAgent = "kindred/4.0 (+https://kindred.polarisocial.xyz; read-only mirror for a recommendation engine)"

// ErrOffline is returned when a network operation is attempted in offline mode.
//
// It is a distinct error rather than a generic failure because offline mode is
// a promise, and a caller needs to tell "you asked me not to use the network"
// apart from "the network failed".
var ErrOffline = errors.New("crawl: offline mode, no network request attempted")

// Client fetches AO3 pages.
//
// The interface exists so tests can exercise the crawler against a local
// httptest server AND against recorded fixtures, with no network and no
// sleeping. A crawler whose only test is "run it against the real site" is a
// crawler nobody can run in CI.
type Client interface {
	// Fetch returns the body for path (absolute URL or a path on BaseURL).
	Fetch(ctx context.Context, rawURL string) ([]byte, error)
	// Delay is the crawl delay to wait between requests.
	Delay() time.Duration
	// Close releases resources.
	Close() error
}

// HTTPClient is the real Client.
type HTTPClient struct {
	base  string
	http  *http.Client
	delay time.Duration
	// lastAt records when the previous request completed, so Delay is measured
	// between requests rather than slept blindly before each one. Sleeping a
	// flat 30s before every request makes a 690-request crawl take 30s longer
	// than necessary for no reason.
	lastAt time.Time
}

// NewHTTPClient builds a client. `base` may be empty for the real site, and is
// set to an httptest server in tests.
func NewHTTPClient(base string) *HTTPClient {
	if base == "" {
		base = BaseURL
	}
	return &HTTPClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{
			Timeout: 60 * time.Second,
		},
		delay: DefaultCrawlDelay,
	}
}

// resolve turns a crawl target into an absolute URL.
//
// The subtlety that cost a measured 2m37s: an absolute URL used to be used
// verbatim, so `--base-url` could not redirect it. A crawl seeded with
// `https://archiveofourown.org/works/111` therefore went to the REAL AO3 even
// with --base-url pointing at a fixture — which is exactly the sort of thing a
// test must never do, and the reason this is a function with its own tests
// rather than two lines inline.
//
// An explicit override base REWRITES an absolute URL from the default host,
// keeping its path and query. It does NOT rewrite an absolute URL from some
// other host: silently repointing a caller's chosen host is how a mirror ends
// up with the wrong corpus and nobody notices.
func (c *HTTPClient) resolve(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", errors.New("crawl: empty url")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return c.base + "/" + strings.TrimLeft(raw, "/"), nil
	}
	if c.base == "" || c.base == BaseURL {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("crawl: bad url %q: %w", raw, err)
	}
	def, err := url.Parse(BaseURL)
	if err != nil {
		return "", fmt.Errorf("crawl: bad base %q: %w", BaseURL, err)
	}
	if !strings.EqualFold(u.Host, def.Host) {
		// Some other host entirely: the caller named it, so use it.
		return raw, nil
	}
	override, err := url.Parse(c.base)
	if err != nil {
		return "", fmt.Errorf("crawl: bad base %q: %w", c.base, err)
	}
	u.Scheme, u.Host = override.Scheme, override.Host
	return u.String(), nil
}

// Fetch retrieves a page.
func (c *HTTPClient) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	target, err := c.resolve(rawURL)
	if err != nil {
		return nil, err
	}

	// The delay is owed BEFORE the request, measured from the previous one.
	if !c.lastAt.IsZero() {
		if wait := c.delay - time.Since(c.lastAt); wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("crawl: build request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := c.http.Do(req)
	c.lastAt = time.Now()
	if err != nil {
		return nil, fmt.Errorf("crawl: %s: %w", target, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("crawl: read %s: %w", target, err)
	}
	// AO3 answers a Cloudflare origin error with HTTP 200 and an interstitial
	// body, so the status alone is not enough: the body is checked too. A 525
	// arrives as 525, but the 200-with-interstitial case is the one that
	// silently poisons a mirror with 8KB of HTML parsed as zero works.
	if IsInterstitial(body) {
		return nil, fmt.Errorf("crawl: %s: Cloudflare interstitial (status %d)", target, resp.StatusCode)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("crawl: %s: rate limited (429)", target)
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("crawl: %s: server error %d (transient)", target, resp.StatusCode)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("crawl: %s: not found (%d)", target, resp.StatusCode)
	}
	return body, nil
}

// Delay reports the configured crawl delay.
func (c *HTTPClient) Delay() time.Duration { return c.delay }

// Close is a no-op; http.Client holds no resources needing explicit release.
func (c *HTTPClient) Close() error { return nil }

// SetDelay overrides the crawl delay, used by LoadRobotsDelay and by tests.
func (c *HTTPClient) SetDelay(d time.Duration) { c.delay = d }

var robotsDelayRe = regexp.MustCompile(`(?i)^\s*crawl-delay\s*:\s*(\d+)`)

// LoadRobotsDelay reads the site's stated crawl delay.
//
// A failure to read robots.txt is NOT permission to crawl faster, so an
// unreachable robots.txt yields DefaultCrawlDelay and an error the caller can
// log. Returning 0 here would be the single most damaging bug this package
// could have: it looks like success and means hammering a site that asked not
// to be.
func (c *HTTPClient) LoadRobotsDelay(ctx context.Context) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/robots.txt", nil)
	if err != nil {
		return DefaultCrawlDelay, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return DefaultCrawlDelay, fmt.Errorf("crawl: robots.txt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return DefaultCrawlDelay, fmt.Errorf("crawl: robots.txt: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DefaultCrawlDelay, fmt.Errorf("crawl: robots.txt: %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if m := robotsDelayRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n > 0 {
				return time.Duration(n) * time.Second, nil
			}
		}
	}
	return DefaultCrawlDelay, nil
}

// interstitialMarkers are strings that appear only in Cloudflare's error page.
var interstitialMarkers = []string{
	"cf-error-details",
	"Attention Required! | Cloudflare",
	"cloudflare ray id",
	"Web server is returning an unknown error",
}

// IsInterstitial reports whether a body is a Cloudflare error page rather than
// content.
//
// Exported because the parse step needs the same judgement: a page that is an
// error page must be skipped, and having two implementations of "is this an
// error page" is how they drift.
func IsInterstitial(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// A real AO3 work page is large; the interstitial is ~8.7KB. Size alone is
	// not a test (a small tag page is legitimate), so it is only used to skip
	// obviously-tiny bodies.
	head := body
	if len(head) > 16384 {
		head = head[:16384]
	}
	lower := strings.ToLower(string(head))
	for _, m := range interstitialMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- state

// State is the resume file: which URLs have been fetched, and when.
//
// It is written atomically (temp file + rename) because the whole value of
// resume is that it survives the crash that made you want it, and a half-written
// JSON file is a corrupt one that costs the entire crawl.
type State struct {
	Version   int               `json:"version"`
	StartedAt time.Time         `json:"started_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Fetched   map[string]int64  `json:"fetched"` // URL -> work id, or 0
	Errors    map[string]string `json:"errors"`  // URL -> last error
	Fetches   int               `json:"fetches"` // attempts, including failures

	// mu guards every field above. It is required, not defensive: Mark and
	// MarkError are called from every fetch worker while the collection
	// goroutine calls Done and Save, and Go's map implementation treats a
	// concurrent read and write as a fatal runtime error rather than a lost
	// update. Caught by `go test -race` at Workers > 1 with checkpointing on,
	// which is why the concurrency test and -race both belong in this suite.
	mu   sync.Mutex `json:"-"`
	Note string     `json:"note,omitempty"`
}

// StateVersion is the on-disk format version. Bumped when the shape changes so
// an old file is rejected rather than misread.
const StateVersion = 1

// NewState returns an empty state.
func NewState() *State {
	return &State{
		Version:   StateVersion,
		StartedAt: time.Now().UTC(),
		Fetched:   map[string]int64{},
		Errors:    map[string]string{},
	}
}

// LoadState reads a resume file. A missing file is not an error: it is a first
// run.
func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("crawl: read state %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("crawl: parse state %s: %w", path, err)
	}
	if s.Version != StateVersion {
		return nil, fmt.Errorf("crawl: state %s is version %d, this build writes version %d",
			path, s.Version, StateVersion)
	}
	if s.Fetched == nil {
		s.Fetched = map[string]int64{}
	}
	if s.Errors == nil {
		s.Errors = map[string]string{}
	}
	return &s, nil
}

// Save writes the state atomically.
//
// mu guards Fetched, Errors, Fetches and UpdatedAt. It is unexported and
// therefore not marshalled, which is what keeps a resumed file byte-identical
// in shape to a written one.

// The mutex is held across the MARSHAL, not just across the file write. A
// checkpoint runs in the collection goroutine while N fetch workers are still
// calling Mark and MarkError, and json.Marshal walks the same two maps -- so a
// lock around the write alone still races the encode. This was a real
// `fatal error: concurrent map read and map write` caught by -race, not by
// inspection: it only appeared at Workers > 1 with checkpointing on.
func (s *State) Save(path string) error {
	s.mu.Lock()
	s.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("crawl: encode state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("crawl: write state: %w", err)
	}
	// Rename is atomic on POSIX within a filesystem, which is what makes a
	// crash mid-write survivable: the old file is intact or the new one is
	// complete, never a truncated hybrid.
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("crawl: commit state: %w", err)
	}
	return nil
}

// Done reports whether a URL has already been fetched.
//
// Errors are NOT counted as done. A URL that failed three times and one that
// succeeded look identical in a set-based resume, and re-crawling a success is
// cheap while silently skipping a failure loses the work forever.
func (s *State) Done(rawURL string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Fetched[canonical(rawURL)]
	return ok
}

// Mark records a successful fetch.
//
// Every worker goroutine calls this concurrently, hence the lock. The map
// writes are otherwise unsynchronised and Go's runtime detects the collision as
// a fatal error rather than corrupting quietly, which is the better of the two
// outcomes but still a crash.
func (s *State) Mark(rawURL string, workID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Fetched[canonical(rawURL)] = workID
	delete(s.Errors, canonical(rawURL))
	s.Fetches++
}

// MarkError records a failed fetch, for reporting rather than for resuming.
func (s *State) MarkError(rawURL string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Errors[canonical(rawURL)] = err.Error()
	s.Fetches++
}

// canonical normalises a URL for use as a map key, so `/works/123` and
// `https://archiveofourown.org/works/123` are the same entry. Without this a
// resume file records both and re-fetches half the corpus.
func canonical(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.Host == "" {
		u.Scheme = "https"
		u.Host = "archiveofourown.org"
	}
	u.Fragment = ""
	u.RawQuery = ""
	return strings.TrimRight(u.String(), "/")
}

// WorkIDFromURL extracts a numeric work id from an AO3 work URL.
//
// It returns 0 for anything that is not a work URL rather than an error,
// because a caller walking a list of links is filtering, not validating: a
// /users/ or /tags/ link in the middle of a bookmark page is not a failure, it
// is simply not a work.
func WorkIDFromURL(rawURL string) int64 {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "works" && i+1 < len(parts) {
			if n, err := strconv.ParseInt(parts[i+1], 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}
