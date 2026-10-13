// Package discover walks AO3 outward from a seed.
//
// ## Where this sits
//
// internal/crawl fetches a URL and parses what came back. It deliberately does
// not decide what to crawl — its own package doc says seed expansion, BFS and
// depth limits belong to a policy layer above it. This is that layer, and
// until now it did not exist: the mirror could only grow from works somebody
// had already queued, which is why kindred could answer questions about a
// snapshot but never grow its own.
//
// ## Why the frontier is LISTINGS rather than works
//
// This is the whole design. An AO3 listing page (`/tags/<name>/works`,
// `/works?query`, a user's bookmarks) carries roughly twenty works, and one
// fetch of it yields twenty ids. A work page carries one work. At AO3's
// `Crawl-delay: 30` — which the client reads from robots.txt rather than
// assuming — a listing is therefore about twenty times cheaper per work
// discovered, and a walk that fetched work pages to find work pages would spend
// a day to learn what an hour of listings teaches.
//
// So: walk listings, harvest ids, hand the ids to the caller. Fetching the work
// pages themselves is a separate job with a separate budget (cmd/kindred's
// autocrawl already drains a queue of exactly those), because a walk that also
// fetched each work would be two policies sharing one delay.
//
// ## Failing open
//
// A fetch error is recorded and the walk continues. A BFS that stopped at the
// first 503 would discover nothing on a site that returns them under load, and
// the crawl client already retries transients, so anything that reaches here
// has already been given its chances.
package discover

import (
	"context"
	"fmt"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

// Fetcher fetches one URL under a politeness policy the caller chose.
//
// It is the crawl.Crawler's FetchOne, and taking the interface rather than the
// concrete type is what lets a walk be tested with no network at all.
type Fetcher interface {
	FetchOne(ctx context.Context, rawURL string) ([]byte, error)
}

// Options bounds a walk.
type Options struct {
	// Seeds are absolute URLs to start from. They are fetched whatever their
	// ListingKind, because an operator naming a seed has already decided it is
	// worth a fetch.
	Seeds []string

	// MaxDepth is the deepest frontier level fetched. Seeds are depth 0, so
	// MaxDepth 0 fetches the seeds and expands nothing, and 1 adds one hop.
	MaxDepth int

	// MaxPages caps fetches over the whole walk. 0 means no cap, which is only
	// safe with a MaxDepth that terminates.
	MaxPages int

	// MaxWorks stops the walk once this many distinct works are found. It is
	// the bound that matters in practice: a single tag listing paginates to
	// thousands of works, and an operator asking for "a few hundred new works"
	// should not have to reason about depth and page counts to get them.
	MaxWorks int

	// Paginate follows rel="next" within a listing. Pagination stays at the
	// same depth — page 2 of a tag is the same distance from the seed as page
	// 1 — so a paginated walk costs pages, not depth.
	Paginate bool

	// FollowKinds names the crawl.ListingKind values worth expanding, e.g.
	// "tag", "user-bookmarks", "search". Empty means the seeds are fetched and
	// nothing beyond them is queued, which is the honest default: following
	// every kind turns one seed into a walk of the whole site.
	FollowKinds []string

	// OnWork is called once per newly discovered work id, in discovery order,
	// before the walk continues. Returning an error stops the walk: a caller
	// persisting ids as it goes must not have them silently dropped.
	OnWork func(id int64) error

	// OnPage is called after each successful fetch, for progress.
	OnPage func(pages, works int, url string)

	// Fetched reports whether a listing page has already been walked. A page it
	// answers true for is never fetched AND never expanded: the walk that read
	// it already queued its links, and re-reading it is the cost this exists to
	// avoid -- a 40-page tag is ~20 minutes against AO3's 30 s crawl delay,
	// repaid in full on every re-run.
	//
	// The callback, not a []string or a file path, because the walk must not
	// care where the answer comes from: a caller with a database passes a
	// lookup, and a test passes a map.
	Fetched func(url string) bool

	// OnFetch records that a listing page has been walked, with how many
	// distinct works it carried.
	//
	// Called ONLY after a fetch that succeeded and was processed to the end. A
	// failed page must stay unrecorded so the next run retries it, and a page
	// the walk abandoned mid-way (MaxWorks) must stay unrecorded too: marking
	// it would make the next run skip the works it never reached, which trades
	// 30 seconds of re-fetching for works lost permanently.
	OnFetch func(url string, works int)
}

// Result is what a walk found and why it stopped.
type Result struct {
	Pages int
	// Skipped counts listing pages the resume checkpoint already had. It is a
	// separate number from Pages rather than folded into it: "fetched 0 pages"
	// with 40 skipped is a finished walk, and "fetched 0 pages" with 0 skipped
	// is a failure -- collapsing them is how a completed tag looks like a
	// broken command.
	Skipped int
	Works   []int64
	// Failed maps a URL to its fetch error. Bounded: a six-hour walk against a
	// flaky site must not accumulate one string per request.
	Failed map[string]string
	// Truncated counts failures not recorded because the cap was reached, so
	// the report cannot imply the failure list is complete when it is not.
	Truncated int
	// Stopped names the reason the walk ended.
	Stopped string
}

// maxRecordedFailures bounds Result.Failed.
const maxRecordedFailures = 50

// Stop reasons.
const (
	StopDrained  = "the queue drained"
	StopMaxPages = "reached MaxPages"
	StopMaxWorks = "reached MaxWorks"
	StopCanceled = "context cancelled"
)

type frontierItem struct {
	url   string
	depth int
}

// Walk breadth-first from the seeds, returning the work ids it found.
//
// It never returns a non-nil error for a fetch failure; see the package doc.
// The only errors are a cancelled context and an error from OnWork.
func Walk(ctx context.Context, f Fetcher, o Options) (*Result, error) {
	if f == nil {
		return nil, fmt.Errorf("discover: no fetcher")
	}
	res := &Result{Failed: map[string]string{}, Stopped: StopDrained}

	kindSet := make(map[string]bool, len(o.FollowKinds))
	for _, k := range o.FollowKinds {
		kindSet[k] = true
	}

	// Two sets, for two different questions. `visited` asks "have I fetched
	// this page", which keeps a pagination cycle from looping forever;
	// `seenWork` asks "have I already reported this work", which is what stops
	// a work linked from six listings from being six discoveries.
	visited := make(map[string]bool)
	seenWork := make(map[int64]bool)

	queue := make([]frontierItem, 0, len(o.Seeds))
	for _, s := range o.Seeds {
		if s != "" {
			queue = append(queue, frontierItem{url: s, depth: 0})
		}
	}

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			res.Stopped = StopCanceled
			return res, nil
		}
		if o.MaxPages > 0 && res.Pages >= o.MaxPages {
			res.Stopped = StopMaxPages
			return res, nil
		}

		item := queue[0]
		queue = queue[1:]
		if visited[item.url] {
			continue
		}
		visited[item.url] = true

		// Resume BEFORE the fetch, which is the only place it can save
		// anything. Marked visited so a page reachable from two listings is
		// not reconsidered, and not expanded: the walk that read it already
		// queued its links.
		if o.Fetched != nil && o.Fetched(item.url) {
			res.Skipped++
			continue
		}

		body, err := f.FetchOne(ctx, item.url)
		if err != nil {
			recordFailure(res, item.url, err)
			continue
		}
		res.Pages++
		if o.OnPage != nil {
			o.OnPage(res.Pages, len(res.Works), item.url)
		}

		// The page's DISTINCT work count, for the checkpoint. Distinct by
		// construction: crawl.ExtractWorkIDs already dedupes while matching, so
		// the count cannot be inflated by a listing that repeats a row. That is
		// a dependency on the crawler's contract, not an accident -- a second
		// dedupe here would be code no test could distinguish from its absence,
		// and TestWalkCountsEachWorkOnAPageOnce is what fails if the contract
		// changes.
		ids := crawl.ExtractWorkIDs(body)
		perPage := len(ids)
		for _, id := range ids {
			if seenWork[id] {
				continue
			}
			seenWork[id] = true
			res.Works = append(res.Works, id)
			if o.OnWork != nil {
				if err := o.OnWork(id); err != nil {
					return res, err
				}
			}
			if o.MaxWorks > 0 && len(res.Works) >= o.MaxWorks {
				res.Stopped = StopMaxWorks
				// Deliberately returns WITHOUT calling OnFetch: this page was
				// abandoned part-way, and recording it would make the next run
				// skip the works it never reached.
				return res, nil
			}
		}
		if o.OnFetch != nil {
			o.OnFetch(item.url, perPage)
		}

		if o.Paginate {
			// NOT depth-gated. Page 2 of a listing is the same distance from
			// the seed as page 1, so the depth limit does not apply to it, and
			// gating it would mean "expand this one tag" also required letting
			// a hop of tag expansion in. MaxPages is what bounds pagination.
			if next := crawl.NextPageURL(body, item.url); next != "" {
				queue = append(queue, frontierItem{url: next, depth: item.depth})
			}
		}

		// Expansion IS depth-gated, and the gate is checked before anything is
		// queued: a seed at the limit costs exactly one fetch and queues
		// nothing.
		if item.depth >= o.MaxDepth || len(kindSet) == 0 {
			continue
		}
		for _, href := range crawl.ExtractHrefs(body, item.url) {
			kind := crawl.ListingKind(href)
			if kind == "" || !kindSet[kind] {
				continue
			}
			queue = append(queue, frontierItem{url: href, depth: item.depth + 1})
		}
	}

	if err := ctx.Err(); err != nil {
		res.Stopped = StopCanceled
	}
	return res, nil
}

func recordFailure(res *Result, url string, err error) {
	if len(res.Failed) >= maxRecordedFailures {
		res.Truncated++
		return
	}
	// The error text can be long and multi-line; a report is read as a table.
	res.Failed[url] = oneLine(err.Error())
}

func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
