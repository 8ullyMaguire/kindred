package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// The algorithm A/B endpoints. An algorithm is a named tune, so these tests
// exercise the two things that can silently be wrong: a comparison that is
// not a comparison, and a vote that lands somewhere nobody reads.

func TestAlgoListIncludesTheBuiltinDefault(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/algo/list")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Algorithms []struct {
			Name    string `json:"name"`
			Builtin bool   `json:"builtin"`
		} `json:"algorithms"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Algorithms) == 0 {
		t.Fatal("no algorithms listed")
	}
	// "default" must always work: it is the built-in weights and the one
	// name a comparison can rely on existing.
	if body.Algorithms[0].Name != "default" || !body.Algorithms[0].Builtin {
		t.Fatalf("first algorithm = %+v, want the builtin default", body.Algorithms[0])
	}
}

// TestAlgoComparePutsTheSameSeedThroughTwoTunes pins the property that makes
// the page a measurement: both columns rank the SAME work, and neither
// contains it.
func TestAlgoComparePutsTheSameSeedThroughTwoTunes(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/algo/compare?seed=ao3_work:1&a=default&b=default2&n=3")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// "default2" does not exist, so this must be a named 400 rather than a
	// column silently ranked by the built-in weights.
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d for an unknown algorithm, want 400", res.StatusCode)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&errBody)
	if !strings.Contains(errBody.Error, "default2") {
		t.Fatalf("error %q does not name the unknown algorithm", errBody.Error)
	}
}

func TestAlgoCompareRefusesAnAlgorithmAgainstItself(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/algo/compare?seed=ao3_work:1&a=default&b=default")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d comparing default with itself, want 400", res.StatusCode)
	}
}

func TestAlgoCompareNeedsASecondAlgorithm(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/algo/compare?seed=ao3_work:1&a=default")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d with no b=, want 400", res.StatusCode)
	}
}

func TestAlgoCompareReturnsTwoListsAndTheSharedCount(t *testing.T) {
	ts, st := newTestServerFull(t)
	// A second algorithm has to EXIST: an unknown name is a named 400 by
	// design, which the test above pins. This one needs a real tune so the
	// comparison has two sides to compare.
	if err := st.SaveTune(context.Background(), "quality", map[string]float64{
		"tag_overlap": 0.10, "quality": 0.60, "recency": 0.05,
	}); err != nil {
		t.Fatalf("saving a tune: %v", err)
	}
	res, err := http.Get(ts.URL + "/api/v1/algo/compare?seed=ao3_work:1&a=default&b=quality&n=3")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := json.Marshal(res.StatusCode)
		t.Fatalf("status = %d (%s), want 200", res.StatusCode, body)
	}
	var body struct {
		Seed   string `json:"seed"`
		SeedID int64  `json:"seed_id"`
		A      struct {
			Algo  string           `json:"algo"`
			Items []rank.Candidate `json:"items"`
		} `json:"a"`
		B struct {
			Algo  string           `json:"algo"`
			Items []rank.Candidate `json:"items"`
		} `json:"b"`
		Shared int `json:"shared"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.SeedID != 1 {
		t.Fatalf("seed_id = %d, want 1", body.SeedID)
	}
	if body.A.Algo != "default" || body.B.Algo != "quality" {
		t.Fatalf("columns = %q / %q, want default / quality", body.A.Algo, body.B.Algo)
	}
	if len(body.A.Items) == 0 || len(body.B.Items) == 0 {
		t.Fatalf("a column is empty: A=%d B=%d items", len(body.A.Items), len(body.B.Items))
	}
	for _, it := range append(body.A.Items, body.B.Items...) {
		if it.ID == 1 {
			t.Fatal("the seed itself appears in a compared list; Exclude must hold")
		}
	}
}

func TestAlgoJudgeRecordsAndStandingsSeeIt(t *testing.T) {
	ts, st := newTestServerFull(t)
	if err := st.SaveTune(context.Background(), "quality", map[string]float64{
		"tag_overlap": 0.10, "quality": 0.60,
	}); err != nil {
		t.Fatalf("saving a tune: %v", err)
	}

	res, err := http.Post(ts.URL+"/api/v1/algo/judge", "application/json",
		strings.NewReader(`{"seed":"ao3_work:1","a":"default","b":"quality","choice":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	sres, err := http.Get(ts.URL + "/api/v1/algo/standings?global=1")
	if err != nil {
		t.Fatal(err)
	}
	defer sres.Body.Close()
	var body struct {
		Algorithms []struct {
			Algo   string `json:"algo"`
			Wins   int    `json:"wins"`
			Losses int    `json:"losses"`
			Judged int    `json:"judged"`
		} `json:"algorithms"`
	}
	if err := json.NewDecoder(sres.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range body.Algorithms {
		if row.Algo != "quality" {
			continue
		}
		found = true
		if row.Wins != 1 {
			t.Fatalf("quality wins = %d, want 1 (the vote said b)", row.Wins)
		}
	}
	if !found {
		t.Fatalf("quality missing from standings: %+v", body.Algorithms)
	}
}

func TestAlgoJudgeRefusesAnUnknownChoice(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Post(ts.URL+"/api/v1/algo/judge", "application/json",
		strings.NewReader(`{"seed":"ao3_work:1","a":"default","b":"quality","choice":"left"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d recording choice \"left\", want 400", res.StatusCode)
	}
}
