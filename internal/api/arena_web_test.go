package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// These cover the arena pages. The one that must not regress is the one
// that panicked in production: tagLinksForWork built its second query by
// slicing the first query's placeholder list to the tag count, and a work
// with more than two tags sliced past the end of a two-element slice. It
// compiled, it passed every existing test, and it took the process down on
// the first real request -- because no test had ever asked for the arena.
//
// Every test here goes through newTestServer, whose corpus fixture has the
// same columns as the real mirror. The production bug was a column that
// exists in neither, so a fixture shaped like the real thing is the only
// thing that catches it.

// The arena must render a pair. If the pool is empty the page still has to
// be a page, not a crash -- a panic here kills the listener.
func TestArenaPageRenders(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/arena")
	if status != http.StatusOK {
		t.Fatalf("GET /arena -> %d, want 200\n%.500s", status, body)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
	// An error page is also 200-shaped HTML in places; assert on the
	// actual thing we came for.
	if strings.Contains(body, "Something went wrong") {
		t.Fatalf("GET /arena rendered the error page:\n%.800s", body)
	}
	if !strings.Contains(body, `action="/arena/judge"`) {
		t.Errorf("the arena has no judge form; the page cannot be used:\n%.800s", body)
	}
}

// A work's tags must appear on the card. This is the regression test for
// the slice-bounds panic: the two works in a pair are looked up by a 2-id
// IN clause, and their tags are looked up by a second IN clause of a
// different length. Building one from the other is where it broke.
func TestArenaCardsCarryTagNames(t *testing.T) {
	ts := newTestServer(t)
	_, _, body := getText(t, ts, "/arena")
	if strings.Contains(body, "Something went wrong") {
		t.Fatalf("GET /arena errored:\n%.800s", body)
	}
	// The fixture's tags are named tag1..tag5. Any one of them appearing
	// proves the tag lookup round-tripped, which is the thing that
	// panicked.
	if !strings.Contains(body, "tag1") && !strings.Contains(body, "tag2") &&
		!strings.Contains(body, "tag3") && !strings.Contains(body, "tag5") {
		t.Errorf("no tag names reached the cards, so the tag lookup is not "+
			"round-tripping:\n%.1500s", body)
	}
}

// Judging is a write that only exists as a form POST, so the method guard
// on every other page must not swallow it.
func TestArenaJudgeAcceptsAPost(t *testing.T) {
	ts := newTestServer(t)
	// Take a session from the arena page first, exactly as a browser
	// would, so the form's hidden field is populated by the server.
	c := ts.Client()
	resp, err := c.Get(ts.URL + "/arena")
	if err != nil {
		t.Fatal(err)
	}
	var session string
	for _, ck := range resp.Cookies() {
		if ck.Name == "kindred_arena" {
			session = ck.Value
		}
	}
	resp.Body.Close()
	if session == "" {
		t.Skip("no arena session cookie was set; nothing to judge")
	}

	form := url.Values{"session": {session}, "choice": {"a"}}
	// A client that does not follow redirects, so the test observes the
	// 303 itself. The default client follows it and this test then measures
	// the 200 of the *next* page, which returns 200 whether or not the
	// judgement was ever recorded.
	noFollow := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	pr, err := noFollow.PostForm(ts.URL+"/arena/judge", form)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	// 303 so the browser re-GETs the next pair instead of re-POSTing and
	// recording a second judgement.
	if pr.StatusCode != http.StatusSeeOther {
		t.Errorf("POST /arena/judge -> %d, want 303", pr.StatusCode)
	}
	if got := pr.Header.Get("Location"); got != "/arena" {
		t.Errorf("POST /arena/judge -> Location %q, want /arena", got)
	}
}

// A judgement with no session is a malformed form, not a judgement of
// nobody. Recording it would create a comparison with no owner.
func TestArenaJudgeRejectsAMissingSession(t *testing.T) {
	ts := newTestServer(t)
	pr, err := ts.Client().PostForm(ts.URL+"/arena/judge", url.Values{"choice": {"a"}})
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /arena/judge with no session -> %d, want 400", pr.StatusCode)
	}
}

// choice is the one thing a judgement cannot do without, and it is a closed
// set. Anything else is a corrupt form and must not reach the store.
func TestArenaJudgeRejectsAnUnknownChoice(t *testing.T) {
	ts := newTestServer(t)
	pr, err := ts.Client().PostForm(ts.URL+"/arena/judge",
		url.Values{"session": {"whatever"}, "choice": {"both"}})
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /arena/judge with choice=both -> %d, want 400", pr.StatusCode)
	}
}

func TestLeaderboardPageRenders(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/leaderboard")
	if status != http.StatusOK {
		t.Fatalf("GET /leaderboard -> %d, want 200\n%.500s", status, body)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
	if strings.Contains(body, "Something went wrong") {
		t.Fatalf("GET /leaderboard rendered the error page:\n%.800s", body)
	}
	// An arena with no judgements is a real state on day one, and it must
	// explain itself rather than render an empty table.
	if !strings.Contains(body, "Nothing has been rated yet") {
		t.Errorf("an unrated arena does not explain its emptiness:\n%.1200s", body)
	}
}

func TestRankPageRendersForAKnownWork(t *testing.T) {
	ts := newTestServer(t)
	status, _, body := getText(t, ts, "/rank/1")
	if status != http.StatusOK {
		t.Fatalf("GET /rank/1 -> %d, want 200\n%.500s", status, body)
	}
	if strings.Contains(body, "Something went wrong") {
		t.Fatalf("GET /rank/1 rendered the error page:\n%.800s", body)
	}
	// The rating, not the title, is the point of this page, so a page
	// without a number on it has failed.
	if !strings.Contains(body, "score") {
		t.Errorf("the rank page shows no rating:\n%.1200s", body)
	}
}

// An unrated work carries the starting rating, not a measured one, and the
// page has to say so -- otherwise "1500" is a false claim about a work
// nobody has compared.
func TestRankPageSaysWhenAWorkIsUnrated(t *testing.T) {
	ts := newTestServer(t)
	_, _, body := getText(t, ts, "/rank/1")
	if !strings.Contains(body, "has not been compared yet") {
		t.Errorf("an unrated work does not say it is unrated:\n%.1200s", body)
	}
}

func TestRankPageRejectsANonNumericID(t *testing.T) {
	ts := newTestServer(t)
	status, _, _ := getText(t, ts, "/rank/not-a-number")
	if status != http.StatusNotFound {
		t.Errorf("GET /rank/not-a-number -> %d, want 404", status)
	}
}

func TestMyRankingPageRenders(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/my-ranking")
	if status != http.StatusOK {
		t.Fatalf("GET /my-ranking -> %d, want 200\n%.500s", status, body)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
	if strings.Contains(body, "Something went wrong") {
		t.Fatalf("GET /my-ranking rendered the error page:\n%.800s", body)
	}
	// A brand new session has no preferences. The page must say that
	// rather than showing an empty panel that reads as "no taste".
	if !strings.Contains(body, "Not enough comparisons yet") {
		t.Errorf("a fresh session does not get the thin-data explanation:\n%.1200s", body)
	}
}

// The arena is only reachable if the footer links to it. A page that
// exists and is linked to nowhere is the same as a page that does not.
func TestFooterLinksToTheArena(t *testing.T) {
	ts := newTestServer(t)
	_, _, body := getText(t, ts, "/")
	for _, want := range []string{"/arena", "/leaderboard", "/my-ranking"} {
		if !strings.Contains(body, `href="`+want+`"`) {
			t.Errorf("no footer link to %s; the arena is unreachable from the UI", want)
		}
	}
}
