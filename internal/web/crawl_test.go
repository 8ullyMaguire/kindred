package web

// /crawl is the only page describing data the mirror does NOT have, so its
// tests are about the states that page exists to distinguish: nothing queued,
// work in progress, and given-up-on. A page that renders all three identically
// would pass a smoke test and fail its purpose.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// storeQueue adapts the real store to the page's interface, so these tests
// exercise the same SQL the deployment runs rather than a canned struct.
func newCrawlDeps(t *testing.T) (Deps, *store.Store) {
	t.Helper()
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return Deps{Version: "test", Queue: st}, st
}

func getCrawl(t *testing.T, d Deps) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/crawl", nil))
	return rec.Code, rec.Body.String()
}

func TestCrawlPageSaysTheMirrorIsNotGrowingWhenNothingIsQueued(t *testing.T) {
	// The failure this page exists for: discovery has stopped, everything looks
	// healthy, and nothing anywhere says "nothing new is coming". Three zeroes
	// do not say it either -- a reader has to be told.
	d, _ := newCrawlDeps(t)
	code, body := getCrawl(t, d)
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200", code)
	}
	for _, want := range []string{"Nothing is queued", "mirror is not growing", "kindred discover"} {
		if !strings.Contains(flat(body), want) {
			t.Errorf("body does not contain %q", want)
		}
	}
}

// flat collapses the template's indentation and newlines, so an assertion is
// about wording rather than about where the renderer wrapped a line.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestCrawlPageSeparatesWaitingFromGivenUpOn(t *testing.T) {
	// "behind" and "stuck on dead links" call for opposite responses, so the
	// page must not render them as the same number.
	ctx := context.Background()
	d, st := newCrawlDeps(t)

	// One waiting, one that failed past the cap.
	if err := st.EnqueueCrawl(ctx, "https://a/works/waiting", "discover"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueCrawl(ctx, "https://a/works/stuck", "reader-requested"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < store.CrawlMaxAttempts; i++ {
		if err := st.FinishCrawlJob(ctx, "https://a/works/stuck", "503 service unavailable"); err != nil {
			t.Fatal(err)
		}
	}

	_, body := getCrawl(t, d)
	page := flat(body)
	for _, want := range []string{
		"waiting", "given up on",
		"1 work failed 5 times",        // the parked row, counted as parked
		"https://a/works/stuck",        // and listed
		"503 service unavailable",      // with its reason
		"a discovery walk",             // explained in the sources table, and
		"a reader asked for this work", // in the recent table too, which used to
		// show the bare name "reader-requested" while the table above it
		// explained the very same value -- one page, two vocabularies.
	} {
		if !strings.Contains(page, want) {
			t.Errorf("body does not contain %q", want)
		}
	}
	// The two states are reported separately. Conflating them is the failure
	// this test is named for, so assert the separation rather than the absence
	// of any particular sentence.
	if !strings.Contains(page, "1 work waiting to be fetched") {
		t.Error("the waiting count is not reported as 1")
	}
	if strings.Contains(page, "2 works waiting") {
		t.Error("the parked row was counted as waiting")
	}
}

func TestCrawlPageShowsTheReasonARowIsFailing(t *testing.T) {
	// Without the reason on the page, "given up on" is a dead end: 404 (gone
	// forever) and 503 (retry later) render identically.
	ctx := context.Background()
	d, st := newCrawlDeps(t)
	if err := st.EnqueueCrawl(ctx, "https://a/works/gone", "discover"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishCrawlJob(ctx, "https://a/works/gone", "404 not found"); err != nil {
		t.Fatal(err)
	}
	_, body := getCrawl(t, d)
	if !strings.Contains(body, "404 not found") {
		t.Error("the failure reason is not on the page")
	}
}

func TestCrawlPageSaysSoWhenThereIsNoQueue(t *testing.T) {
	// 200 with an explanation, not an error page: on a read-only deployment the
	// absence is the configuration, and a reader who lands here deserves to
	// know why rather than to see a broken table.
	code, body := getCrawl(t, Deps{Version: "test"})
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200", code)
	}
	if !strings.Contains(body, "no crawl queue") {
		t.Errorf("body does not explain the missing queue:\n%s", body)
	}
}

func TestCrawlPageDoesNotLeakAQueueErrorAsAPanic(t *testing.T) {
	// The page must survive the store failing, the state database being empty,
	// and anything else a real deployment throws at it.
	ctx := context.Background()
	d, st := newCrawlDeps(t)
	if err := st.EnqueueCrawl(ctx, "https://a/works/1", "discover"); err != nil {
		t.Fatal(err)
	}
	// Close the store out from under the page: every query now fails.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("GET /crawl panicked on a failing store: %v", r)
			}
		}()
		d.route(rec, httptest.NewRequest(http.MethodGet, "/crawl", nil))
	}()
	if rec.Code == http.StatusOK {
		t.Error("a failing store rendered a 200; a page that cannot read the queue must not claim it is empty")
	}
}
