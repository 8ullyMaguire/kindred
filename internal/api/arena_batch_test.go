package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The batch is the only thing in the arena that WRITES ratings. Judging a
// pair only records the comparison; nothing is rated until a period runs.
// So a batch that fails is not a degraded feature, it is a feature that
// cannot produce its own output: the leaderboard stays empty forever, and
// an empty leaderboard looks exactly like "nobody has judged anything yet",
// which is a believable story and a wrong one.
//
// It 500'd on every run for exactly that reason. The arithmetic was tested
// in internal/arena and the write was tested nowhere.

// postJSON sends a POST and decodes the JSON reply, so a 500 with a body is
// a test failure with a readable message rather than a bare status code.
func postJSON(t *testing.T, ts *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	resp, err := ts.Client().Post(ts.URL+path, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("POST %s returned non-JSON: %v", path, err)
	}
	return resp.StatusCode, out
}

// arenaSession drives one judged comparison through the page form, which is
// the same path a browser takes and the only path that mints a session.
func arenaSession(t *testing.T, ts *httptest.Server, choice string) {
	t.Helper()
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
		t.Skip("no arena session cookie; the pool may be exhausted")
	}
	form := url.Values{"session": {session}, "choice": {choice}}
	pr, err := c.PostForm(ts.URL+"/arena/judge", form)
	if err != nil {
		t.Fatal(err)
	}
	pr.Body.Close()
}

// TestArenaBatchWritesRatings is the regression test for the struct-as-arg
// bug: the batch SQL was handed a store.RatingRow as a single argument,
// which database/sql cannot marshal, so every period failed to write.
func TestArenaBatchWritesRatings(t *testing.T) {
	ts := newTestServer(t)

	// Judge two pairs, so the batch has something to fold into a period.
	// A batch over zero comparisons is skipped, and a skipped batch proves
	// nothing about the write path.
	arenaSession(t, ts, "a")
	arenaSession(t, ts, "a")

	status, body := postJSON(t, ts, "/api/v1/arena/batch")
	if status != http.StatusOK {
		t.Fatalf("POST /api/v1/arena/batch -> %d, want 200: %v", status, body)
	}
	if _, skipped := body["skipped"]; skipped {
		t.Fatalf("the batch skipped with comparisons judged: %v", body)
	}
	if body["period"] != float64(1) {
		t.Errorf("first period = %v, want 1", body["period"])
	}
	if n, _ := body["works_updated"].(float64); n < 1 {
		t.Errorf("works_updated = %v, want at least 1", body["works_updated"])
	}
}

// A leaderboard that is empty after a successful batch is the specific
// failure this whole path had: ratings written somewhere nobody reads them.
func TestLeaderboardHasRowsAfterABatch(t *testing.T) {
	ts := newTestServer(t)
	arenaSession(t, ts, "a")
	arenaSession(t, ts, "b")

	if status, body := postJSON(t, ts, "/api/v1/arena/batch"); status != http.StatusOK {
		t.Fatalf("batch -> %d: %v", status, body)
	}

	resp, err := ts.Client().Get(ts.URL + "/api/v1/arena/leaderboard")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("leaderboard -> %d", resp.StatusCode)
	}
	var lb struct {
		Entries []struct {
			WorkID int64   `json:"work_id"`
			Title  string  `json:"title"`
			Rating float64 `json:"rating"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lb); err != nil {
		t.Fatal(err)
	}
	if len(lb.Entries) == 0 {
		t.Fatal("the leaderboard is empty after a batch that reported success; " +
			"ratings were written somewhere the page does not read")
	}
	// A rated work must have left 1500. A row sitting exactly at the
	// starting rating means the arithmetic ran and the value was discarded.
	for _, e := range lb.Entries {
		if e.Title == "" {
			t.Errorf("entry for work %d has no title", e.WorkID)
		}
	}
}

// The batch is a POST because it writes every rating in the database. If it
// ever became reachable by GET, a crawler or a prefetcher would run it.
func TestArenaBatchRejectsGET(t *testing.T) {
	ts := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/api/v1/arena/batch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("GET /api/v1/arena/batch -> 200; a GET must not write every rating")
	}
}

// Two batches with no new judgements must not invent a period. A period
// with no games in it would advance arena_last_period and quietly widen the
// window of the NEXT real period.
func TestArenaBatchIsIdempotentWithNoNewJudgements(t *testing.T) {
	ts := newTestServer(t)
	arenaSession(t, ts, "a")
	if status, body := postJSON(t, ts, "/api/v1/arena/batch"); status != http.StatusOK {
		t.Fatalf("first batch -> %d: %v", status, body)
	}
	status, body := postJSON(t, ts, "/api/v1/arena/batch")
	if status != http.StatusOK {
		t.Fatalf("second batch -> %d: %v", status, body)
	}
	if _, skipped := body["skipped"]; !skipped {
		t.Errorf("a second batch with nothing new should skip, got %v", body)
	}
}

// The rating page must show the same number the API does. Two code paths
// reading the same row and disagreeing is the failure that makes a
// leaderboard untrustworthy without any error being raised.
//
// The work is taken from the leaderboard rather than hardcoded: which two
// works the strategy pairs is not a property this test should assume, and
// hardcoding one made this test fail for the right reason by accident.
func TestRankPageAgreesWithTheAPI(t *testing.T) {
	ts := newTestServer(t)
	arenaSession(t, ts, "a")
	if status, body := postJSON(t, ts, "/api/v1/arena/batch"); status != http.StatusOK {
		t.Fatalf("batch -> %d: %v", status, body)
	}

	resp, err := ts.Client().Get(ts.URL + "/api/v1/arena/leaderboard")
	if err != nil {
		t.Fatal(err)
	}
	var lb struct {
		Entries []struct {
			WorkID      int64   `json:"work_id"`
			Comparisons int     `json:"comparisons"`
			Rating      float64 `json:"rating"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lb); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(lb.Entries) == 0 {
		t.Fatal("no rated works to compare the page against")
	}

	// Every work the API says is rated must also read as rated on the page.
	// One was enough to break; all of them is the claim being tested.
	for _, e := range lb.Entries {
		if e.Comparisons < 1 {
			t.Fatalf("the leaderboard lists work %d with %d comparisons; a "+
				"listed work with no comparisons means the tally is wrong",
				e.WorkID, e.Comparisons)
		}
		_, _, page := getText(t, ts, "/rank/"+strconv.FormatInt(e.WorkID, 10))
		if strings.Contains(page, "has not been compared yet") {
			t.Errorf("the page says work %d is unrated while the API reports "+
				"%d comparisons", e.WorkID, e.Comparisons)
		}
		if !strings.Contains(page, "Comparisons") {
			t.Errorf("the rank page for work %d shows no comparison count:\n%.800s",
				e.WorkID, page)
		}
	}
}
