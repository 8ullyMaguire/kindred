package api

// Reading paths (#40) over HTTP.
//
// The engine's tests prove the walk; these prove the contract. The shared api
// fixture has no embeddings written, which is the honest state of a fresh
// mirror — so the contract here is "say that, and say it in words", not "return
// ten rows".

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestPathWithoutEmbeddingsSaysSo(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/works/1/path?steps=5")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("path -> %d, want 200 with an explanation", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	var doc struct {
		Seed      struct{ ID int64 } `json:"seed"`
		Steps     []any             `json:"steps"`
		Requested int               `json:"requested"`
		Returned  int               `json:"returned"`
		Note      string            `json:"note"`
		Missing   []string          `json:"missing"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if doc.Seed.ID != 1 {
		t.Fatalf("seed id = %d", doc.Seed.ID)
	}
	if len(doc.Steps) != 0 || doc.Returned != 0 {
		t.Fatalf("a fixture with no embeddings produced %d steps", len(doc.Steps))
	}
	if doc.Requested != 5 {
		t.Fatalf("requested = %d, want the 5 the caller asked for: an empty answer "+
			"still has to say what it was asked for", doc.Requested)
	}
	if !containsStr(doc.Missing, "embed_cosine") {
		t.Fatalf("missing = %v, want embed_cosine named", doc.Missing)
	}
	if doc.Note == "" {
		t.Fatal("no note: a client gets an empty list and no reason for it")
	}
}

func TestPathNamesABadSeedAndABadStepCount(t *testing.T) {
	ts := newTestServer(t)
	for _, c := range []struct {
		url  string
		want int
	}{
		{"/api/v1/works/999999/path", http.StatusNotFound},
		{"/api/v1/works/1/path?steps=0", http.StatusBadRequest},
		{"/api/v1/works/1/path?steps=abc", http.StatusBadRequest},
	} {
		res, err := http.Get(ts.URL + c.url)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Fatalf("%s -> %d, want %d", c.url, res.StatusCode, c.want)
		}
	}
}
