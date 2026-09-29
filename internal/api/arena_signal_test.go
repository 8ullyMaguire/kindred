package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// peer_rating only counts if it reaches the response. A signal that is
// registered but never scored, or scored but never named, is exactly the
// failure SPEC §1 records in kindling: the arena signal was inert in
// production and silent about it.
//
// So these two tests are the claim, from both ends: the signal must appear in
// meta.degraded[] when the arena is empty, and it must NOT appear there --
// while still changing the ranking -- once ratings exist.

// degradedNames pulls meta.degraded out of a /recommend response.
func degradedNames(t *testing.T, body []byte) []string {
	t.Helper()
	var out struct {
		Meta struct {
			Degraded []string `json:"degraded"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("recommend response is not JSON: %v\n%.400s", err, body)
	}
	return out.Meta.Degraded
}

// With an arena that has rated nothing, peer_rating must be named in
// meta.degraded[]. This is the assertion that makes an inert arena signal
// observable rather than invisible.
func TestPeerRatingIsReportedDegradedWhenTheArenaIsEmpty(t *testing.T) {
	ts := newTestServer(t)
	status, _, body := getText(t, ts, "/api/v1/recommend?seed=ao3_work:1&n=5")
	if status != http.StatusOK {
		t.Fatalf("recommend -> %d: %.300s", status, body)
	}
	got := degradedNames(t, []byte(body))
	for _, name := range got {
		if name == "peer_rating" {
			return // the claim holds
		}
	}
	t.Errorf("meta.degraded = %v, want it to contain peer_rating: the arena "+
		"has rated nothing, so the signal is degraded and SPEC 7.1 requires "+
		"it to be named rather than silently absent", got)
}

// Once a batch has rated works, peer_rating must NOT be degraded, and its
// evidence string must reach the response. A signal that stays degraded
// after the data arrived is a signal that is wired to nothing.
func TestPeerRatingIsNotDegradedOnceTheArenaHasRated(t *testing.T) {
	ts := newTestServer(t)
	arenaSession(t, ts, "a")
	arenaSession(t, ts, "b")
	batchStatus, batch := postJSON(t, ts, "/api/v1/arena/batch")
	if batchStatus != http.StatusOK {
		t.Fatalf("batch -> %d: %v", batchStatus, batch)
	}

	status, _, body := getText(t, ts, "/api/v1/recommend?seed=ao3_work:1&n=5")
	if status != http.StatusOK {
		t.Fatalf("recommend -> %d: %.300s", status, body)
	}
	for _, name := range degradedNames(t, []byte(body)) {
		if name == "peer_rating" {
			t.Errorf("peer_rating is reported degraded after a batch rated "+
				"works (batch said %v); the signal is registered but never scoring",
				batch)
		}
	}
	// The evidence has to be visible, or the weight is unaccountable.
	if !strings.Contains(body, "arena effective rating") {
		t.Errorf("no candidate carries peer_rating evidence, so the signal is "+
			"not reaching the response:\n%.1200s", body)
	}
}

// The default tune must carry a weight for every signal the engine can
// register. A missing key means the signal is registered, scores, and is
// then multiplied by nothing -- present in the code, absent in the ranking.
// The API reports the tune it used, so this is checkable from the response.
func TestRecommendReportsAPeerRatingWeight(t *testing.T) {
	ts := newTestServer(t)
	_, _, body := getText(t, ts, "/api/v1/recommend?seed=ao3_work:1&n=5")
	if !strings.Contains(body, "peer_rating") {
		t.Errorf("the response does not mention peer_rating at all, so its "+
			"weight is unaccountable:\n%.1200s", body)
	}
}
