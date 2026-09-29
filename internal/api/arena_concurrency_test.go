package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// The batch replaces the engine's arena-ratings map while requests read it.
// Unsynchronised, that is a concurrent map read and write: a fatal runtime
// error, not a subtly wrong answer.
//
// Getting a test to actually observe that race took two attempts, and both
// failures are the same trap as the rest of this work:
//
//  1. Judge twice, batch three times. Batches 2 and 3 SKIP -- nothing new
//     to rate -- so they return before touching the map. Reader and writer
//     never met, and the test passed with the mutex removed.
//  2. Interleave judge-then-batch 12 times. Measured: 12 judgements, ONE
//     real batch, 11 skipped. The watermark is a wall-clock time and the
//     loop runs inside one second, so `judged_at > watermark` excludes
//     everything judged after the first batch. One swap, no observation.
//
// So the interleaving was right and the TRIGGER was wrong. A test that hopes
// a race gets scheduled is a test that may not run. The two tests below
// assert the property directly through the HTTP surface, which does not
// depend on scheduling at all; the concurrency test is then only holding the
// door for the race detector.

// peerRatingIsDegraded asks the recommender whether its arena signal is
// contributing. It is the observable consequence of the engine holding (or
// not holding) ratings, so it asserts the real claim rather than an internal.
func peerRatingIsDegraded(t *testing.T, ts *httptest.Server) bool {
	t.Helper()
	status, _, body := getText(t, ts, "/api/v1/recommend?seed=ao3_work:1&n=5")
	if status != http.StatusOK {
		t.Fatalf("recommend -> %d: %.300s", status, body)
	}
	for _, name := range degradedNames(t, []byte(body)) {
		if name == "peer_rating" {
			return true
		}
	}
	return false
}

// A batch that rated something must stop the signal reporting itself
// degraded. If it does not, the batch updated the database and changed
// nothing a reader sees -- the quiet failure the reload exists to prevent.
func TestArenaRatingsReachTheSignalAfterABatch(t *testing.T) {
	ts := newTestServer(t)
	arenaSession(t, ts, "a")
	arenaSession(t, ts, "a")

	if !peerRatingIsDegraded(t, ts) {
		t.Fatal("peer_rating is not degraded before any batch; the fixture " +
			"should start with an arena that has rated nothing")
	}

	batchStatus, batch := postJSON(t, ts, "/api/v1/arena/batch")
	if batchStatus != http.StatusOK {
		t.Fatalf("batch -> %d: %v", batchStatus, batch)
	}
	if _, skipped := batch["skipped"]; skipped {
		t.Fatalf("the batch skipped: %v", batch)
	}

	if peerRatingIsDegraded(t, ts) {
		t.Error("peer_rating is still degraded after a batch rated works; the " +
			"engine's copy was not replaced, so the batch changed the database " +
			"and nothing a reader sees")
	}
}

// A skipped batch must leave the engine alone. Reloading on a skip would
// reload identical data -- harmless, but the reason to check is that an
// unconditional reload is the obvious version to write.
func TestSkippedBatchLeavesTheSignalAlone(t *testing.T) {
	ts := newTestServer(t)
	arenaSession(t, ts, "a")
	if status, b := postJSON(t, ts, "/api/v1/arena/batch"); status != http.StatusOK {
		t.Fatalf("first batch -> %d: %v", status, b)
	}
	if peerRatingIsDegraded(t, ts) {
		t.Fatal("the first batch did not reach the signal")
	}

	batchStatus, body := postJSON(t, ts, "/api/v1/arena/batch")
	if batchStatus != http.StatusOK {
		t.Fatalf("second batch -> %d: %v", batchStatus, body)
	}
	if _, skipped := body["skipped"]; !skipped {
		t.Fatalf("a second batch with nothing new should skip, got %v", body)
	}
	if peerRatingIsDegraded(t, ts) {
		t.Error("a skipped batch left peer_rating degraded; something cleared " +
			"the engine's ratings without a period to replace them")
	}
}

// Keeps the race detector honest during real traffic against real batches.
// It does not claim to force the interleaving -- see the note above -- it
// fails if the process dies, which is the symptom of an unsynchronised swap.
func TestConcurrentBatchAndRecommend(t *testing.T) {
	ts := newTestServer(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := ts.Client()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := c.Get(ts.URL + "/api/v1/recommend?seed=ao3_work:1&n=5")
				if err != nil {
					return
				}
				resp.Body.Close()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		c := ts.Client()
		for i := 0; i < 12; i++ {
			resp, err := c.Get(ts.URL + "/arena")
			if err != nil {
				return
			}
			var session string
			for _, ck := range resp.Cookies() {
				if ck.Name == "kindred_arena" {
					session = ck.Value
				}
			}
			resp.Body.Close()
			if session == "" {
				return
			}
			pr, err := c.PostForm(ts.URL+"/arena/judge",
				url.Values{"session": {session}, "choice": {"a"}})
			if err != nil {
				return
			}
			pr.Body.Close()
			// Long enough to cross a second boundary, so the next batch is
			// not skipped and really does swap the map.
			time.Sleep(1100 * time.Millisecond)
			br, err := c.Post(ts.URL+"/api/v1/arena/batch", "application/json", nil)
			if err != nil {
				return
			}
			br.Body.Close()
		}
	}()

	time.Sleep(4 * time.Second)
	close(stop)
	wg.Wait()

	status, _, body := getText(t, ts, "/api/v1/recommend?seed=ao3_work:1&n=5")
	if status != http.StatusOK {
		t.Fatalf("recommend after concurrent batches -> %d: %.300s", status, body)
	}
}
