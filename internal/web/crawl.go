package web

// The crawl page: what the mirror is still missing, and what it has given up on.
//
// This is the only page in the UI that describes ABSENT data. Every other page
// reports what the mirror holds, so discovery that has stopped looks exactly
// like a healthy mirror with nothing new to say -- a failure that is invisible
// by construction, because the symptom of "we stopped finding works" is a page
// that renders perfectly and says nothing about it.
//
// ## Why the page cannot start a walk
//
// A walk of a popular tag is hours of wall-clock against AO3's 30-second crawl
// delay. There is no honest way to run that inside an HTTP request, and a
// request that kicked off a background walk would leak a job whose progress no
// page could then show. So the page REPORTS the backlog and the operator starts
// a walk with:
//
//	kindred discover --seed "tag:Slow Burn" --enqueue
//
// which queues the works it finds and lets the crawler this page is watching
// fetch them. The page is the feedback loop for that command, not a substitute
// for it.

import (
	"context"
	"net/http"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// QueueStatter is the auto-crawler's backlog. An interface, and optional on
// Deps, because a read-only deployment has no queue at all -- passing nil is
// how the same code serves both, and the page then says so in words rather
// than 503-ing with a JSON blob.
type QueueStatter interface {
	CrawlQueueStats(ctx context.Context, recent int) (store.CrawlQueueStats, error)
}

// QueueSource is one contributor to the pending backlog.
type QueueSource struct {
	Source string
	Count  int
	// Why explains who puts rows here in this source's name, because
	// "reader-requested" and "discover" are the deployment's vocabulary and
	// not something a reader can be expected to decode.
	Why string
}

// QueueRow is one queued work.
type QueueRow struct {
	URL        string
	Source     string
	Attempts   int
	LastError  string
	EnqueuedAt string
	Done       bool
	// Parked marks a row past the retry cap, which will not be retried until
	// something asks for it again.
	Parked bool
	// Why is what the source means in words. The recent table showed the raw
	// name ("reader-requested") while the sources table above it explained the
	// very same value — one page, two vocabularies, and the unexplained one was
	// the table a reader looks at when something is failing.
	Why string
}

// CrawlPage is the /crawl view.
type CrawlPage struct {
	Base
	NoQueue bool
	Pending int
	Done    int
	Parked  int
	Total   int
	Sources []QueueSource
	Recent  []QueueRow
	// Cap is the number of failures after which a job is parked. Shown,
	// because "parked" is meaningless without it.
	Cap int
}

// crawlSourceWhy names what each source means, for the two the deployment
// actually writes.
func crawlSourceWhy(source string) string {
	switch source {
	case "discover":
		return "found by a discovery walk"
	case "reader-requested":
		return "a reader asked for this work"
	case "incomplete":
		return "already in the mirror, missing metadata"
	}
	return "queued by " + source
}

func (d Deps) renderCrawl(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := CrawlPage{
		Base: d.base("Crawl queue", "What the mirror is still missing"),
		Cap:  store.CrawlMaxAttempts,
	}
	if d.Queue == nil {
		page.NoQueue = true
		d.page(w, "crawl.html", page, http.StatusOK)
		return
	}

	st, err := d.Queue.CrawlQueueStats(ctx, 50)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	page.Pending, page.Done, page.Parked = st.Pending, st.Done, st.Parked
	page.Total = st.Pending + st.Done + st.Parked

	// Sorted, because a map iterates in a different order every render and a
	// breakdown that reshuffles itself reads as data changing when it is not.
	page.Sources = make([]QueueSource, 0, len(st.BySource))
	for src, n := range st.BySource {
		page.Sources = append(page.Sources, QueueSource{Source: src, Count: n, Why: crawlSourceWhy(src)})
	}
	sort.Slice(page.Sources, func(i, j int) bool {
		if page.Sources[i].Count != page.Sources[j].Count {
			return page.Sources[i].Count > page.Sources[j].Count
		}
		return page.Sources[i].Source < page.Sources[j].Source
	})

	page.Recent = make([]QueueRow, 0, len(st.Recent))
	for _, row := range st.Recent {
		page.Recent = append(page.Recent, QueueRow{
			URL:        row.URL,
			Source:     row.Source,
			Attempts:   row.Attempts,
			LastError:  row.LastError,
			EnqueuedAt: row.EnqueuedAt,
			Done:       row.Done,
			Parked:     !row.Done && row.Attempts >= store.CrawlMaxAttempts,
			Why:        crawlSourceWhy(row.Source),
		})
	}

	d.page(w, "crawl.html", page, http.StatusOK)
}
