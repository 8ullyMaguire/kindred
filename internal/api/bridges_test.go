package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The shared API fixture's tags are not fandom-shaped, so the endpoint must
// say WHY there are no bridges rather than answer an unexplained empty list.
func TestBridgesExplainsWhyThereAreNone(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/works/1/bridges?limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		WorkID int64  `json:"work_id"`
		Items  []any  `json:"items"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.WorkID != 1 {
		t.Fatalf("work_id = %d, want 1", body.WorkID)
	}
	if len(body.Items) != 0 {
		t.Fatalf("items = %v, want none", body.Items)
	}
	if !strings.Contains(body.Note, "no fandom-shaped tag") {
		t.Fatalf("note = %q, want it to name the fandom-shape rule", body.Note)
	}
}
