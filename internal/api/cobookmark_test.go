package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// The endpoint must return the works this work's readers also kept, in
// shared-reader order, with the count as evidence. The fixture seeds readers
// 1 and 2 on works 1 and 2, and readers 2 and 3 on works 1 and 3, so both 2
// and 3 have two shared readers and the seed must not appear.
func TestCoBookmarkedEndpoint(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/works/1/co-bookmarked?limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		WorkID   int64            `json:"work_id"`
		Items    []rank.Candidate `json:"items"`
		Returned int              `json:"returned"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.WorkID != 1 {
		t.Fatalf("work_id = %d, want 1", body.WorkID)
	}
	if body.Returned != len(body.Items) || body.Returned < 2 {
		t.Fatalf("returned = %d with %d items, want at least 2 and the two to agree",
			body.Returned, len(body.Items))
	}
	ids := map[int64]bool{}
	for _, it := range body.Items {
		ids[it.ID] = true
		if it.ID == 1 {
			t.Fatal("the seed itself was returned")
		}
		if len(it.Evidence) == 0 || it.Evidence[0].Signal != "co_bookmark" {
			t.Fatalf("item %d carries no co_bookmark evidence: %+v", it.ID, it.Evidence)
		}
	}
	if !ids[2] || !ids[3] {
		t.Fatalf("items = %v, want works 2 and 3", ids)
	}
}

func TestCoBookmarkedEndpointRejectsANonNumericWorkID(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/works/notanid/co-bookmarked")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a non-numeric id is the caller's mistake", res.StatusCode)
	}
}
