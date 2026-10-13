package main

// runDiscover walks AO3 outward from seeds and finds works the mirror does not
// have.
//
// ## How this differs from `crawl`
//
// `crawl` fetches URLs you already know. This finds the URLs you do not: it
// walks LISTING pages — a tag, a search, a user's bookmarks — which carry about
// twenty works each, and hands the ids it finds to the queue that `serve`'s
// auto-crawler already drains. So the two compose:
//
//	kindred discover --seed "tag:Slow Burn" --enqueue   # find new works
//	kindred serve                                       # fetch their metadata
//
// or, in one go, `kindred crawl --urls <(kindred discover ... -print-urls)`.
//
// ## Why it does not fetch the work pages itself
//
// Because the crawl delay is the binding constraint, not the CPU. AO3 asks for
// 30 seconds between requests and the client reads that from robots.txt. A
// command that walked listings AND fetched each work would be two policies
// sharing one delay, and the second would silently starve the first. The
// queue already exists for the work pages, with its own resume, its own
// retries and its own place in `serve`.
//
// ## Seeds are shorthand, because URLs are not what anyone types
//
// --seed accepts a full URL for the cases shorthand cannot express, and
// otherwise tag:/search:/user:/bookmarks:/collection:/series:. See parseSeed.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
	"git.polarisocial.xyz/kindred/kindred/internal/discover"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// stringList is a flag that may be repeated: --seed a --seed b.
//
// Not comma-separated, because the values are tag names and search queries and
// those contain commas. A tag literally named "Slow Burn, Actually" is a real
// thing on AO3, and a comma-separated flag cannot express it at all.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// seedKinds are the shorthands parseSeed understands.
var seedKinds = []string{"tag", "search", "user", "bookmarks", "collection", "series", "work"}

// parseSeed turns a shorthand seed into an absolute AO3 URL.
//
// A full http(s) URL passes through untouched: the shorthand exists so the
// common cases do not require knowing AO3's URL shapes, not to forbid naming
// one.
//
// The slash replacement is AO3's own convention rather than an escaping
// choice — AO3 rewrites a "/" inside a tag name to "*s*", so the pairing tag
// "Kirk/Spock" lives at /tags/Kirk*s*Spock/works and not at /tags/Kirk%2FSpock.
// Escaping it the obvious way 404s, which is the kind of failure that looks
// like "there are no works for this tag".
func parseSeed(s, base string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty seed")
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s, nil
	}

	kind, rest, found := strings.Cut(s, ":")
	if !found {
		return "", fmt.Errorf("seed %q needs a kind: one of %s, or a full URL",
			s, strings.Join(seedKinds, ", "))
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", fmt.Errorf("seed %q has no value after %q", s, kind)
	}

	// tagPath applies AO3's own tag-name rewriting.
	tagPath := func(name string) string {
		return strings.ReplaceAll(url.PathEscape(strings.ReplaceAll(name, "/", "*s*")), "+", "%20")
	}

	switch kind {
	case "tag":
		return base + "/tags/" + tagPath(rest) + "/works", nil
	case "search":
		// Sorted by kudos: a search that is not sorted by something stable is
		// a different first page on every request, and the walk would gather
		// twenty works per page at random rather than the twenty best.
		q := url.Values{}
		q.Set("work_search[query]", rest)
		q.Set("work_search[sort_column]", "kudos")
		return base + "/works?" + q.Encode(), nil
	case "user":
		return base + "/users/" + url.PathEscape(strings.Trim(rest, "/")) + "/works", nil
	case "bookmarks":
		return base + "/users/" + url.PathEscape(strings.Trim(rest, "/")) + "/bookmarks", nil
	case "collection":
		return base + "/collections/" + url.PathEscape(strings.Trim(rest, "/")) + "/works", nil
	case "series":
		if _, err := strconv.ParseInt(rest, 10, 64); err != nil {
			return "", fmt.Errorf("series seed %q is not a numeric id", rest)
		}
		return base + "/series/" + rest, nil
	case "work":
		if _, err := strconv.ParseInt(rest, 10, 64); err != nil {
			return "", fmt.Errorf("work seed %q is not a numeric id", rest)
		}
		return base + "/works/" + rest, nil
	}
	return "", fmt.Errorf("unknown seed kind %q: one of %s", kind, strings.Join(seedKinds, ", "))
}

// parseKinds validates --kinds against the classifier's vocabulary, so a typo
// fails here rather than producing a walk that quietly follows nothing.
func parseKinds(s string) ([]string, error) {
	valid := map[string]bool{
		"tag": true, "search": true, "user-works": true, "user-bookmarks": true,
		"collection": true, "series": true,
	}
	var out []string
	for _, k := range strings.Split(s, ",") {
		k = strings.TrimSpace(k)
		if k == "" || k == "none" {
			continue
		}
		if !valid[k] {
			names := make([]string, 0, len(valid))
			for n := range valid {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("unknown kind %q: one of %s", k, strings.Join(names, ", "))
		}
		out = append(out, k)
	}
	return out, nil
}

func runDiscover(ctx context.Context, args []string) error {
	fs := newFlagSet("discover")
	var seeds stringList
	fs.Var(&seeds, "seed", "where to start; repeatable. tag:<name>, search:<query>, user:<name>, "+
		"bookmarks:<name>, collection:<name>, series:<id>, work:<id>, or a full URL")
	var (
		depth    = fs.Int("depth", 1, "listing hops from a seed; 0 fetches the seeds and expands nothing")
		maxPages = fs.Int("pages", 40, "cap on listing pages fetched")
		maxWorks = fs.Int("works", 500, "stop once this many distinct works are found (0 = no cap)")
		kinds    = fs.String("kinds", "tag", "comma-separated listing kinds to expand: "+
			"tag, search, user-works, user-bookmarks, collection, series, or none")
		paginate   = fs.Bool("paginate", true, "follow rel=next within a listing")
		enqueue    = fs.Bool("enqueue", false, "queue the discovered works so serve's auto-crawler fetches their metadata")
		printURLs  = fs.Bool("print-urls", false, "print discovered work URLs, one per line")
		statePath  = fs.String("state", "", "resume file; a re-run skips what it already discovered")
		offline    = fs.Bool("offline", false, "make no network request at all")
		baseURL    = fs.String("base-url", "", "override the AO3 base URL (for a fixture server)")
		crawlDelay = fs.Duration("crawl-delay", -1, "override the crawl delay; -1 reads robots.txt, 0 means no wait")
	)
	c := bindConfig(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// finishConfig is NOT used here, and the reason is the point of the
	// command: it demands --corpus, but discover's job is to find works the
	// mirror does not have. Requiring a mirror would demand the very thing this
	// command exists to fill, and would make it unusable in the case it is for.
	// The database is needed only to queue.
	if *enqueue && c.CorpusDB == "" {
		return errors.New("--enqueue needs --corpus (the mirror the queued works will be written into)")
	}
	if len(seeds) == 0 {
		return errors.New("no --seed given; try --seed \"tag:Slow Burn\"")
	}
	base := *baseURL
	if base == "" {
		base = crawl.BaseURL
	}
	base = strings.TrimRight(base, "/")

	seedURLs := make([]string, 0, len(seeds))
	for _, s := range seeds {
		u, err := parseSeed(s, base)
		if err != nil {
			return err
		}
		seedURLs = append(seedURLs, u)
	}
	kindList, err := parseKinds(*kinds)
	if err != nil {
		return err
	}

	// Resume. A walk of a popular tag is hours of wall-clock against a 30-second
	// delay, so a re-run must not start over. The state file is a plain URL
	// list, one per line, appended as pages are fetched — the same shape the
	// crawl package uses, because the failure it prevents is the same one.
	state, closeState, err := openDiscoverState(*statePath)
	if err != nil {
		return err
	}
	defer closeState()

	var qs *store.Store
	if *enqueue {
		qs, err = store.Open(ctx, c.DB, c.CorpusDB)
		if err != nil {
			return fmt.Errorf("opening the state database to queue discovered works: %w", err)
		}
		defer qs.Close()
	}

	var client crawl.Client
	if *offline {
		client = crawl.NewOffline("--offline: no network request will be made")
	} else {
		hc := crawl.NewHTTPClient(base)
		delay, derr := hc.LoadRobotsDelay(ctx)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "discover: robots.txt unreadable, using the conservative %s (%v)\n", delay, derr)
		}
		if *crawlDelay >= 0 {
			delay = *crawlDelay
		}
		hc.SetDelay(delay)
		client = hc
	}
	defer client.Close()

	crawler := crawl.New(client)

	// Every discovered work is reported and optionally queued as it is found,
	// not at the end: a walk that dies an hour in must not lose the hour.
	var queued, alreadyKnown int
	onWork := func(id int64) error {
		if err := state.mark(base + "/works/" + strconv.FormatInt(id, 10)); err != nil {
			return err
		}
		if *printURLs {
			fmt.Println(base + "/works/" + strconv.FormatInt(id, 10))
		}
		if qs == nil {
			return nil
		}
		if err := qs.EnqueueCrawl(ctx, base+"/works/"+strconv.FormatInt(id, 10), "discover"); err != nil {
			return err
		}
		queued++
		return nil
	}

	res, err := discover.Walk(ctx, crawler, discover.Options{
		Seeds:       seedURLs,
		MaxDepth:    *depth,
		MaxPages:    *maxPages,
		MaxWorks:    *maxWorks,
		Paginate:    *paginate,
		FollowKinds: kindList,
		OnWork:      onWork,
	})
	if err != nil {
		return err
	}

	fmt.Printf("discover: %d page(s) fetched, %d work(s) discovered, stopped: %s\n",
		res.Pages, len(res.Works), res.Stopped)
	if qs != nil {
		fmt.Printf("discover: %d work(s) queued for metadata fetch\n", queued)
	}
	if alreadyKnown > 0 {
		fmt.Printf("discover: %d already queued\n", alreadyKnown)
	}
	if len(res.Failed) > 0 {
		fmt.Printf("discover: %d page(s) failed:\n", len(res.Failed)+res.Truncated)
		for _, u := range sortedKeys(res.Failed) {
			fmt.Printf("  %s: %s\n", u, res.Failed[u])
		}
		if res.Truncated > 0 {
			fmt.Printf("  …and %d more not shown\n", res.Truncated)
		}
	}
	if len(res.Works) == 0 {
		// A walk that found nothing has not succeeded, whatever the reason, and
		// saying so is the difference between "no new works" and "this command
		// did not work". Exiting 0 here would let a script treat an offline run
		// or a mistyped seed as a clean result — the failure mode crawl.go
		// already names for its own offline mode.
		switch {
		case res.Pages == 0:
			return fmt.Errorf("no page could be fetched (%d failed): the seed, the network, or --offline", len(res.Failed)+res.Truncated)
		default:
			return fmt.Errorf("%d page(s) fetched but no works found; is the seed a listing rather than a work?", res.Pages)
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// discoverState is the resume file: URLs already fetched or discovered.
type discoverState struct {
	path string
	f    *os.File
	seen map[string]bool
}

func openDiscoverState(path string) (*discoverState, func(), error) {
	st := &discoverState{seen: map[string]bool{}}
	if path == "" {
		return st, func() {}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			st.seen[line] = true
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	st.path, st.f = path, f
	return st, func() {
		if f != nil {
			_ = f.Close()
		}
	}, nil
}

func (s *discoverState) mark(u string) error {
	if s.f == nil || s.seen[u] {
		return nil
	}
	s.seen[u] = true
	_, err := fmt.Fprintln(s.f, u)
	return err
}
