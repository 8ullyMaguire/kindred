package api

// The per-kind algorithm surface over HTTP.
//
// The engine's tests prove each mechanism; these prove the four claims a client
// can check from outside:
//
//   - the registry names a default per kind and a reason per disabled algorithm;
//   - a disabled algorithm is refused by name and the request still answers;
//   - a choice is stored per KIND (choosing for works leaves tags alone);
//   - a served list is logged with the arm that produced it.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAlgoKindsListsEveryKindWithADefault(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/algo/kinds")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var doc struct {
		Kinds []struct {
			Kind  string `json:"kind"`
			Label string `json:"label"`
			Algos []struct {
				ID      string `json:"id"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Default bool   `json:"default"`
			} `json:"algos"`
			Active string `json:"default"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(doc.Kinds) != 6 {
		t.Fatalf("the registry lists %d kinds, want the arena's six", len(doc.Kinds))
	}
	for _, k := range doc.Kinds {
		if k.Active == "" {
			t.Fatalf("%s names no default", k.Kind)
		}
		live, disabled := 0, 0
		for _, a := range k.Algos {
			switch a.Status {
			case "live":
				live++
			case "disabled":
				disabled++
				if a.Reason == "" {
					t.Fatalf("%s/%s is disabled with no reason on the wire", k.Kind, a.ID)
				}
			}
		}
		if live == 0 {
			t.Fatalf("%s offers nothing live", k.Kind)
		}
		if disabled == 0 {
			// Not cosmetic: the whole point of publishing the disabled set is
			// that a reader asking "why not a GNN" finds an answer.
			t.Fatalf("%s lists no disabled algorithms, so the page cannot answer why "+
				"the heavier models are missing", k.Kind)
		}
	}
}

func TestARefusedAlgorithmStillAnswersAndSaysWhy(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/related?kind=ao3_tag&id=1&algo=lightgcn&n=3")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("asking for a disabled algorithm -> %d, want 200 with the reason", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	var doc struct {
		Algo    string `json:"algo"`
		Default string `json:"default"`
		Refused string `json:"refused"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if doc.Algo == "lightgcn" {
		t.Fatal("the disabled algorithm was used")
	}
	if doc.Algo != doc.Default {
		t.Fatalf("fell back to %s, want the default %s", doc.Algo, doc.Default)
	}
	if !strings.Contains(doc.Refused, "not available") || !strings.Contains(doc.Refused, "training pipeline") {
		t.Fatalf("refusal = %q, want it to name the algorithm and the reason", doc.Refused)
	}
}

func TestAChoiceIsStoredPerKind(t *testing.T) {
	ts, st := newTestServerFull(t)
	db := st.DB
	client := jarClient(t, ts)

	form := url.Values{"kind": {"ao3_work"}, "algo": {"collab"}}
	res, err := client.PostForm(ts.URL+"/api/v1/algo/choice", form)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("storing a choice -> %d", res.StatusCode)
	}

	// The choice applies to works…
	var works, tags string
	if err := db.QueryRow(`SELECT algo FROM algo_choices WHERE kind = 'ao3_work'`).Scan(&works); err != nil {
		t.Fatalf("works choice not stored: %v", err)
	}
	if works != "collab" {
		t.Fatalf("works = %q, want collab", works)
	}
	// …and NOT to tags: a choice for one kind must not become a global setting.
	if err := db.QueryRow(`SELECT algo FROM algo_choices WHERE kind = 'ao3_tag'`).Scan(&tags); err == nil {
		t.Fatalf("choosing for works also wrote a tag row (%q)", tags)
	}

	// The other direction, which is the one that catches a handler that ignores
	// ?kind=: a choice stored for tags must be stored AS tags. `popular` is live
	// for both kinds, so the algorithm alone cannot say which row was written.
	res, err = client.PostForm(ts.URL+"/api/v1/algo/choice", url.Values{"kind": {"ao3_tag"}, "algo": {"popular"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var tagAlgo string
	if err := db.QueryRow(`SELECT algo FROM algo_choices WHERE kind = 'ao3_tag'`).Scan(&tagAlgo); err != nil {
		t.Fatalf("a tag choice was not stored under its kind: %v", err)
	}
	if tagAlgo != "popular" {
		t.Fatalf("tag fate = %q, want popular", tagAlgo)
	}
	if err := db.QueryRow(`SELECT algo FROM algo_choices WHERE kind = 'ao3_work'`).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if works != "collab" {
		t.Fatalf("the tag choice overwrote the work choice: works = %q, want collab", works)
	}

	// Clearing returns the reader to the experiment.
	res, err = client.PostForm(ts.URL+"/api/v1/algo/choice", url.Values{"kind": {"ao3_work"}, "clear": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM algo_choices WHERE kind = 'ao3_work'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d work choices survived a clear", n)
	}
}

func TestAServedListIsLoggedWithItsArm(t *testing.T) {
	ts, st := newTestServerFull(t)
	db := st.DB
	client := jarClient(t, ts)
	// The GET itself mints: this route exists to run the experiment, and a list
	// served to an unassigned reader is not in one.
	if _, err := client.Get(ts.URL + "/api/v1/related?kind=ao3_tag&id=1&n=3"); err != nil {
		t.Fatal(err)
	}

	var kind, arm, algoID string
	var seed int64
	if err := db.QueryRow(
		`SELECT kind, algo, arm, seed_id FROM algo_impressions ORDER BY id DESC LIMIT 1`,
	).Scan(&kind, &algoID, &arm, &seed); err != nil {
		t.Fatalf("no impression was recorded: %v", err)
	}
	if kind != "ao3_tag" {
		t.Fatalf("kind = %q", kind)
	}
	if seed != 1 {
		t.Fatalf("seed = %d, want the seed that was asked for", seed)
	}
	// With no choice stored the reader is in the experiment, and the arm says
	// which side: "request"/"choice" would mean the page is recording the wrong
	// reason, which is the difference between an experiment and a survey.
	if arm != "default" && arm != "challenger" {
		t.Fatalf("arm = %q, want default or challenger for a reader with no choice", arm)
	}
	if algoID == "" {
		t.Fatal("the impression does not name an algorithm")
	}
}

func TestARequestedAlgorithmIsLoggedAsARequest(t *testing.T) {
	ts, st := newTestServerFull(t)
	db := st.DB
	client := jarClient(t, ts)
	if _, err := client.Get(ts.URL + "/api/v1/related?kind=ao3_tag&id=1&algo=popular&n=3"); err != nil {
		t.Fatal(err)
	}
	var arm string
	if err := db.QueryRow(`SELECT arm FROM algo_impressions ORDER BY id DESC LIMIT 1`).Scan(&arm); err != nil {
		t.Fatal(err)
	}
	if arm != "request" {
		t.Fatalf("arm = %q, want request: an explicit ?algo= is not an experiment arm", arm)
	}
}

func TestRelatedRefusesABadSeedAndABadKind(t *testing.T) {
	ts := newTestServer(t)
	for _, u := range []string{
		"/api/v1/related?kind=ao3_tag",
		"/api/v1/related?kind=ao3_tag&id=0",
		"/api/v1/related?kind=ao3_tag&id=-4",
		"/api/v1/related?kind=ao3_tag&id=abc",
	} {
		res, err := http.Get(ts.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s -> %d, want 400", u, res.StatusCode)
		}
	}
	// An unknown kind is NOT a 400: ParseKind degrades to works, because the
	// JSON API's callers predate `?kind=`.
	res, err := http.Get(ts.URL + "/api/v1/related?kind=spaceships&id=1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an unknown kind -> %d, want the documented fallback to works", res.StatusCode)
	}
}
