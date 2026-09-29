package api

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The resume path is the one a normal user hits and a test never did.
//
// /arena presents a pair, and the pair is recorded at PRESENTATION. If the
// user walks away and comes back, the next request finds an unjudged row for
// their session and resumes it -- which means reading presented_at. That
// column is TEXT (datetime('now') has no type in SQLite), and scanning it
// into a bare time.Time is a runtime Scan error, not a compile error:
//
//   unsupported Scan, storing driver.Value type string into type *time.Time
//
// So every returning visitor got the error page on /arena and no test failed,
// because every test judged the pair immediately and never came back. A test
// that only completes the happy path cannot see a bug in the path taken when
// the happy path is abandoned.
//
// presented_at is column index 7 in that SELECT, which is the index the
// production error named.

// jarClient returns a client that persists cookies, so repeated requests
// share one arena session the way a browser's do.
func jarClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	j, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: j}
}

// getArena fetches /arena with a cookie-persisting client and returns the
// body, the session value, and whether a pair was rendered.
func getArena(t *testing.T, c *http.Client, ts *httptest.Server) (body, session string, paired bool) {
	t.Helper()
	resp, err := c.Get(ts.URL + "/arena")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// The jar is the authority: it holds the session even when this
	// particular response did not re-set the cookie.
	if u := c.Jar.Cookies(resp.Request.URL); u != nil {
		for _, ck := range u {
			if ck.Name == "kindred_arena" {
				session = ck.Value
			}
		}
	}
	if session == "" {
		for _, ck := range resp.Cookies() {
			if ck.Name == "kindred_arena" {
				session = ck.Value
			}
		}
	}
	return string(raw), session, strings.Contains(string(raw), `class="arena-card"`)
}

// TestArenaResumesAnUnjudgedPair is the regression test: present a pair, do
// NOT judge it, come back. The page must render, not error.
func TestArenaResumesAnUnjudgedPair(t *testing.T) {
	ts := newTestServer(t)
	c := jarClient(t, ts)

	first, _, paired := getArena(t, c, ts)
	if !paired {
		t.Skip("the fixture pool produced no pair; nothing to resume")
	}
	firstPair := pairWorks(first)

	// Abandon it. The next visit is the one that used to fail.
	second, _, _ := getArena(t, c, ts)
	if strings.Contains(second, "Something went wrong") {
		t.Fatalf("resuming an unjudged pair rendered the error page:\n%.900s", second)
	}
	if !strings.Contains(second, `class="arena-card"`) {
		t.Fatalf("the resume path rendered no pair:\n%.900s", second)
	}
	// The unjudged presentation is still owed, so the same pair must come
	// back rather than a fresh one stacked on the queue.
	if got := pairWorks(second); got != firstPair {
		t.Errorf("resumed pair %q, want the presented pair %q", got, firstPair)
	}
}

// TestArenaHandlesSeveralUnjudgedRows covers what several open tabs leave
// behind: more than one outstanding presentation for one session.
func TestArenaHandlesSeveralUnjudgedRows(t *testing.T) {
	ts := newTestServer(t)
	c := jarClient(t, ts)

	for i := 0; i < 4; i++ {
		body, _, _ := getArena(t, c, ts)
		if strings.Contains(body, "Something went wrong") {
			t.Fatalf("visit %d rendered the error page:\n%.900s", i+1, body)
		}
	}
}

// Judging has to work with an unjudged row outstanding, because the resume
// path and the judge path touch the same row.
func TestJudgeStillWorksWithAnUnjudgedRowPresent(t *testing.T) {
	ts := newTestServer(t)
	c := jarClient(t, ts)

	if _, _, paired := getArena(t, c, ts); !paired {
		t.Skip("no pair presented")
	}
	_, session, _ := getArena(t, c, ts)
	if session == "" {
		t.Fatal("no arena session cookie")
	}

	// A client that does NOT follow redirects, so this observes the 303
	// itself. The default client follows it and this then measures the 200
	// of the next page, which is 200 whether or not anything was recorded.
	noFollow := *c
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	pr, err := noFollow.PostForm(ts.URL+"/arena/judge", url.Values{"session": {session}, "choice": {"a"}})
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusSeeOther {
		t.Errorf("POST /arena/judge with an unjudged row present -> %d, want 303", pr.StatusCode)
	}
}

// pairWorks extracts the two work links from a rendered pair, as a stable
// string for comparison. Order is not asserted: the store may resume either
// side first, and what matters is that it is the SAME pair.
func pairWorks(body string) string {
	var out []string
	for _, part := range strings.Split(body, `href="/work/`) {
		if i := strings.IndexByte(part, '"'); i > 0 {
			out = append(out, part[:i])
		}
	}
	return strings.Join(out, ",")
}
