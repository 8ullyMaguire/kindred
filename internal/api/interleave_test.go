package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// The interleaving surface: one list, both arms, and a read mark as the vote.
//
// What can silently be wrong here is not the merge (rank.TeamDraft has its own
// tests) but the ATTRIBUTION: a list recorded against no session, an arm
// credited that never showed the work, or a vote that lands where no standings
// query reads it. So these tests drive the whole path -- list, read mark,
// standings -- through the HTTP surface with a cookie jar, which is the only
// way a session survives from one request to the next.

type interleaveBody struct {
	Items []struct {
		ID  int64  `json:"id"`
		Arm string `json:"arm"`
	} `json:"items"`
	Exposure map[string]int `json:"exposure"`
	N        int            `json:"n"`
	Seed     string         `json:"seed"`
}

// tuneFixtureStore defines a second algorithm whose weights differ from the
// built-in ones, so the two arms do not produce identical lists and the merge
// is exercised rather than trivially concatenated.
func tuneFixtureStore(t *testing.T, s *store.Store) string {
	t.Helper()
	if err := s.SaveTune(context.Background(), "recency_only", map[string]float64{"recency": 1}); err != nil {
		t.Fatalf("SaveTune: %v", err)
	}
	return "recency_only"
}

func TestInterleaveMergesBothArmsAndRecordsTheImpressions(t *testing.T) {
	ts, s := newTestServerFull(t)
	other := tuneFixtureStore(t, s)
	// jarClient, NOT ts.Client(): httptest's client has no cookie jar, so
	// every request would mint a new session and the impressions would be
	// recorded against an owner no later request can name.
	c := jarClient(t, ts)

	res, err := c.Get(ts.URL + "/api/v1/interleave?seed=ao3_work:1&a=default&b=" + other + "&n=4")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body interleaveBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	// The fixture corpus is small, so n=4 is a ceiling rather than a
	// promise: the seed is excluded and a pool of three works cannot fill
	// four slots.
	if len(body.Items) == 0 || len(body.Items) > 4 {
		t.Fatalf("%d items, want 1..4", len(body.Items))
	}
	var slots int
	for _, n := range body.Exposure {
		slots += n
	}
	if slots != len(body.Items) {
		t.Fatalf("exposure %v sums to %d, but the list has %d items", body.Exposure, slots, len(body.Items))
	}
	// Both arms contributed. One arm taking every slot is the failure mode
	// that makes a credit rate a measure of the merge.
	if body.Exposure["default"] == 0 || body.Exposure[other] == 0 {
		t.Fatalf("exposure %v, want slots from both arms", body.Exposure)
	}
	// No work appears twice: a duplicate would be two credits for one work.
	seen := map[int64]bool{}
	for _, it := range body.Items {
		if seen[it.ID] {
			t.Fatalf("work %d appears twice in the merged list", it.ID)
		}
		seen[it.ID] = true
	}
}

// The vote: a read mark on a merged-list item credits the arm that showed it,
// once, and the standings report it.
func TestReadMarkCreditsTheArmThatShowedTheWork(t *testing.T) {
	ts, s := newTestServerFull(t)
	other := tuneFixtureStore(t, s)
	c := jarClient(t, ts)

	res, err := c.Get(ts.URL + "/api/v1/interleave?seed=ao3_work:1&a=default&b=" + other + "&n=4")
	if err != nil {
		t.Fatal(err)
	}
	var body interleaveBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(body.Items) == 0 {
		t.Fatal("the merged list is empty; nothing can be credited")
	}
	target, arm := body.Items[0].ID, body.Items[0].Arm

	// The read form, the same one a browser submits.
	form := url.Values{"work_id": {strconv.FormatInt(target, 10)}, "action": {"read"}}
	post, err := c.PostForm(ts.URL+"/seen", form)
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusSeeOther && post.StatusCode != http.StatusFound && post.StatusCode != http.StatusOK {
		t.Fatalf("read mark status = %d", post.StatusCode)
	}

	standings := getStandings(t, c, ts)
	var found bool
	for _, a := range standings.Arms {
		if a.Arm != arm {
			continue
		}
		found = true
		if a.Credits != 1 {
			t.Fatalf("arm %q has %d credits, want 1 (work %d was read)", arm, a.Credits, target)
		}
		// Exactly the slots the merged list showed from that arm: the
		// standings denominator and the response's own exposure must be the
		// same number, or the rate printed on the page is over a different
		// population than the list it describes.
		if want := body.Exposure[arm]; a.Impressions != want {
			t.Fatalf("arm %q has %d impressions, want %d (its slots in the merged list)",
				arm, a.Impressions, want)
		}
	}
	if !found {
		t.Fatalf("the credited arm %q is absent from the standings %+v", arm, standings.Arms)
	}

	// A second read mark on the same impression is a revisit, not a vote.
	if post, err := c.PostForm(ts.URL+"/seen", form); err != nil {
		t.Fatal(err)
	} else {
		post.Body.Close()
	}
	for _, a := range getStandings(t, c, ts).Arms {
		if a.Arm == arm && a.Credits != 1 {
			t.Fatalf("arm %q has %d credits after a second read mark, want 1", arm, a.Credits)
		}
	}
}

type standingsBody struct {
	Arms []struct {
		Arm         string  `json:"arm"`
		Impressions int     `json:"impressions"`
		Credits     int     `json:"credits"`
		CreditRate  float64 `json:"credit_rate"`
	} `json:"arms"`
}

func getStandings(t *testing.T, c *http.Client, ts *httptest.Server) standingsBody {
	t.Helper()
	res, err := c.Get(ts.URL + "/api/v1/interleave/standings")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("standings status = %d, want 200", res.StatusCode)
	}
	var body standingsBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// An odd n is refused BY NAME. Rounding it would hand the first arm a slot and
// then report the resulting difference as a preference.
func TestInterleaveRefusesAnOddSlotCount(t *testing.T) {
	ts, s := newTestServerFull(t)
	other := tuneFixtureStore(t, s)
	res, err := jarClient(t, ts).Get(ts.URL + "/api/v1/interleave?seed=ao3_work:1&a=default&b=" + other + "&n=5")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d for n=5, want 400", res.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error, "even") {
		t.Fatalf("error %q does not name the even-slot rule", body.Error)
	}
}

func TestInterleaveRefusesSelfComparison(t *testing.T) {
	ts := newTestServer(t)
	res, err := jarClient(t, ts).Get(ts.URL + "/api/v1/interleave?seed=ao3_work:1&a=default&b=default&n=4")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an arm against itself", res.StatusCode)
	}
}
