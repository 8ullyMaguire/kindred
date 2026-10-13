package web

// /mirror exists because the nav used to point readers at /stats and /healthz,
// both of which are JSON. The tests below are mostly about the page NOT
// claiming things it cannot know: every count here has an "unknown" spelling
// that must be distinct from a zero, because "0 tags" is a claim about the
// corpus and "cannot tell you" is not.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func renderMirrorPage(t *testing.T, d Deps) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/mirror", nil))
	return rec.Code, rec.Body.String()
}

// errQueue is a queue whose read fails. distinct from a queue with nothing in it.
type errQueue struct{}

func (errQueue) CrawlQueueStats(context.Context, int) (store.CrawlQueueStats, error) {
	return store.CrawlQueueStats{}, errors.New("state database is locked")
}

func TestMirrorPageRendersTheNavMarkerAndTheCounts(t *testing.T) {
	d, _ := newCrawlDeps(t)
	code, body := renderMirrorPage(t, d)
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200", code)
	}
	// The nav marker, on the RIGHT link: /mirror is the page that replaced the
	// two JSON links, so it is the one that must show as current.
	if !strings.Contains(body, `<a href="/mirror" class="is-current" aria-current="page">This mirror</a>`) {
		t.Error("the mirror page does not mark its own nav entry")
	}
	// And the backlog is reported, not silently absent. This is the section
	// that used to sit inside the corpus branch and vanish with it.
	if !strings.Contains(flat(body), "Nothing is queued") {
		t.Errorf("an empty queue is not reported")
	}
}

func TestMirrorPageDistinguishesNoQueueFromAQueueThatFailed(t *testing.T) {
	// The sibling-disagreement shape, in three states that all end up as "no
	// numbers on the page":
	//
	//   no queue at all      -> the deployment cannot queue, and that is fine
	//   a queue that errored -> the backlog is UNKNOWN, which is the state a
	//                           stalled mirror hides in
	//   an empty queue       -> genuinely nothing waiting
	//
	// Collapsing the middle into either neighbour prints a reassuring sentence
	// for a deployment that has just lost the ability to say whether it is
	// falling behind.
	noQueue, _ := newCrawlDeps(t)
	noQueue.Queue = nil
	_, bodyNoQueue := renderMirrorPage(t, noQueue)

	_, bodyErr := renderMirrorPage(t, Deps{Version: "test", Queue: errQueue{}})

	empty, _ := newCrawlDeps(t)
	_, bodyEmpty := renderMirrorPage(t, empty)

	if bodyNoQueue == bodyErr || bodyErr == bodyEmpty || bodyNoQueue == bodyEmpty {
		t.Fatal("two of the three backlog states render identically; they are different facts")
	}
	if !strings.Contains(flat(bodyErr), "could not be read") {
		t.Errorf("a failed queue read does not say so: %s", flat(bodyErr)[:200])
	}
	if !strings.Contains(flat(bodyNoQueue), "no crawl queue") {
		t.Errorf("a deployment with no queue does not say so: %s", flat(bodyNoQueue)[:200])
	}
	// The failed read must NOT be described as an empty queue.
	if strings.Contains(flat(bodyErr), "Nothing is queued") {
		t.Error("a failed queue read is reported as an empty queue")
	}
}

func TestMirrorPageSaysWhenThereIsNoCorpusRatherThanPrintingZeroes(t *testing.T) {
	// Deps.Engine is nil here, which is the corpus-less configuration. The page
	// must not render "0 works" -- that is a claim about the mirror, and this
	// deployment has no mirror to make it about.
	d, _ := newCrawlDeps(t)
	_, body := renderMirrorPage(t, d)

	if !strings.Contains(flat(body), "no corpus handle") {
		t.Errorf("a corpus-less deployment does not say so")
	}
	// Scoped to the counts table: the page must not print a labelled zero for
	// a corpus it cannot read. (The shared tagline had the same bug and is
	// guarded too -- see TestTheTaglineDoesNotClaimZeroWorksWithoutACorpus.)
	if strings.Contains(body, `<span class="stat-label">works</span>`) ||
		strings.Contains(body, `<span class="stat-label">tags</span>`) {
		t.Error("the page printed a labelled zero count for a corpus it cannot read")
	}
	// But the backlog still has to be stated, corpus or no corpus. This Deps
	// HAS a queue, so the section must render its own verdict rather than
	// being swallowed by the corpus branch.
	if !strings.Contains(flat(body), "Nothing is queued") {
		t.Error("a corpus-less page lost its backlog section as well")
	}
}

func TestMirrorPagePointsAtTheAPIForTheFactsItLeavesOut(t *testing.T) {
	// The page is a front for the API, not a second source of the same facts.
	// The memory and embedding numbers live there; a reader who wants them must
	// be able to find them from here.
	d, _ := newCrawlDeps(t)
	_, body := renderMirrorPage(t, d)

	for _, want := range []string{"/api/v1/stats", "/healthz"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not link %s", want)
		}
	}
}

func TestTheNavNoLongerOffersJSONAsPages(t *testing.T) {
	// This is the defect /mirror was built to fix, so it gets a test: /stats and
	// /healthz return application/json, and a nav link is a promise of a page.
	for _, href := range navHrefs(t) {
		if href == "/stats" || href == "/healthz" {
			t.Errorf("the nav links %s, which returns JSON, as if it were a page", href)
		}
	}
}

func TestTheHomePageTitleDoesNotRepeatTheSiteName(t *testing.T) {
	// The layout appends " · kindred", and the home page's own title IS
	// "kindred", so the page every visitor lands on announced itself twice.
	d, _ := newCrawlDeps(t)
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if strings.Contains(body, "kindred &middot; kindred") {
		t.Error(`the home page title is "kindred · kindred"`)
	}
	if !strings.Contains(body, "<title>kindred</title>") {
		t.Errorf("the home page has no site title; got %q",
			body[strings.Index(body, "<title>"):strings.Index(body, "</title>")+8])
	}
	// And it has an h1: it had none, which is both an a11y landmark gap and a
	// page with nothing to scan.
	if !strings.Contains(flat(body), "<h1>Find something to read</h1>") {
		t.Error("the home page has no heading")
	}
}

func TestTheTaglineDoesNotClaimZeroWorksWithoutACorpus(t *testing.T) {
	// The shared header's sentence about the mirror printed its corpus count
	// unconditionally, so every page of a corpus-less deployment announced "a
	// local mirror of 0 works". A count that was never taken must not be
	// printed as one.
	d, _ := newCrawlDeps(t)
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if strings.Contains(flat(rec.Body.String()), "mirror of 0 works") {
		t.Error(`the tagline claims "a local mirror of 0 works" with no corpus to count`)
	}
}

func TestSearchHasAHeadingOfItsOwn(t *testing.T) {
	// Different words from the home page's: "Find something to read" above a
	// filled-in result list would be an instruction the reader has already
	// followed. This deployment has no corpus, so the query 503s -- which is
	// still a page with a heading, and the heading is what is being checked.
	d, _ := newCrawlDeps(t)
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/search?q=nonexistent", nil))
	body := flat(rec.Body.String())
	if strings.Contains(body, "<h1>Find something to read</h1>") {
		t.Error("/search reused the home page's heading")
	}
}
