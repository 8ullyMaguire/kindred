package api

// Taste export and import (#36). The route's promises: 401 without a session, the vector with
// NAMES and evidence on the way out, merge by default and replace only when asked, and a
// refused import that names the tag it could not resolve.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// tasteSession is a session cookie the routes will hash into an owner key. The store hashes
// whatever identifier arrives, so a test does not need the arena to mint one -- and using the
// arena here would put a pair-selection between the test and its subject.
func tasteSession(t *testing.T, _ string) *http.Cookie {
	t.Helper()
	return &http.Cookie{Name: "kindred_arena", Value: "taste-test-session-36"}
}

func TestTasteRequiresASession(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/taste")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET status = %d, want 401", res.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/taste",
		strings.NewReader(`{"tags":[{"name":"angst","weight":0.5}]}`))
	if err != nil {
		t.Fatal(err)
	}
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("PUT status = %d, want 401: an import must never write to an unauthenticated key", res2.StatusCode)
	}
}

// The round trip: import two tags, read them back with their evidence, merge a third, then
// replace the vector and watch the count fall. Every assertion is on the SERVER's numbers, not
// on the response echo of its own input.
func TestTasteImportRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	cookie := tasteSession(t, ts.URL)

	put := func(body string, query string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/taste"+query,
			bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	get := func() (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/taste", nil)
		req.AddCookie(cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	code, body := put(`{"tags":[{"name":"tag1","weight":0.6,"n":4},{"name":"tag2","weight":-0.2,"n":2}]}`, "")
	if code != http.StatusOK {
		t.Fatalf("import status = %d (%v)", code, body)
	}
	if got, _ := body["imported"].(float64); got != 2 {
		t.Fatalf("imported = %v, want 2", body["imported"])
	}

	code, got := get()
	if code != http.StatusOK {
		t.Fatalf("export status = %d", code)
	}
	if n, _ := got["count"].(float64); n != 2 {
		t.Fatalf("count = %v, want 2 (%v)", got["count"], got)
	}
	tags, _ := got["tags"].([]any)
	first, _ := tags[0].(map[string]any)
	if first["name"] != "tag1" {
		t.Fatalf("first tag = %v, want slow burn (the strongest by |weight|)", first["name"])
	}
	if w, _ := first["weight"].(float64); w != 0.6 {
		t.Fatalf("weight = %v, want 0.6", first["weight"])
	}
	if n, _ := first["n"].(float64); n != 4 {
		t.Fatalf("n = %v, want 4: the evidence count has to survive the round trip", first["n"])
	}

	// MERGE: a third tag joins, the first two stay.
	code, body = put(`{"tags":[{"name":"tag3","weight":0.3}]}`, "")
	if code != http.StatusOK {
		t.Fatalf("merge status = %d (%v)", code, body)
	}
	if n, _ := body["after"].(float64); n != 3 {
		t.Fatalf("after merge = %v, want 3", body["after"])
	}
	if note, _ := body["note"].(string); !strings.Contains(note, "merged") {
		t.Fatalf("the merge is not named in the response: %v", body["note"])
	}

	// REPLACE: one tag, and the other two are gone.
	code, body = put(`{"tags":[{"name":"tag3","weight":0.9,"n":7}]}`, "?replace=1")
	if code != http.StatusOK {
		t.Fatalf("replace status = %d (%v)", code, body)
	}
	if n, _ := body["after"].(float64); n != 1 {
		t.Fatalf("after replace = %v, want 1", body["after"])
	}
	if n, _ := body["replaced"].(float64); n != 3 {
		t.Fatalf("replaced = %v, want 3: the caller is told what the import overwrote", body["replaced"])
	}
	if _, got := get(); func() bool { n, _ := got["count"].(float64); return n == 1 }() == false {
		t.Fatalf("the vector did not shrink to one tag: %v", got)
	}
}

func TestTasteImportRefusesBadVectors(t *testing.T) {
	ts := newTestServer(t)
	cookie := tasteSession(t, ts.URL)
	cases := []struct {
		name string
		body string
		want int
		why  string
	}{
		{"empty", `{"tags":[]}`, http.StatusBadRequest, "empty"},
		{"unknown field", `{"weights":[]}`, http.StatusBadRequest, "unknown field"},
		{"no weight", `{"tags":[{"name":"tag1"}]}`, http.StatusUnprocessableEntity, "weight is required"},
		{"weight too big", `{"tags":[{"name":"tag1","weight":2}]}`, http.StatusUnprocessableEntity, "outside [-1, 1]"},
		{"unknown tag", `{"tags":[{"name":"a tag this mirror has never held","weight":0.1}]}`, http.StatusUnprocessableEntity, "no tag named"},
		{"no name or id", `{"tags":[{"weight":0.1}]}`, http.StatusUnprocessableEntity, "needs a name or an id"},
		{"duplicate", `{"tags":[{"name":"tag1","weight":0.1},{"name":"tag1","weight":0.2}]}`, http.StatusUnprocessableEntity, "appears twice"},
		{"n out of range", `{"tags":[{"name":"tag1","weight":0.1,"n":0}]}`, http.StatusUnprocessableEntity, "outside [1, 1000]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/taste",
				bytes.NewReader([]byte(c.body)))
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(cookie)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != c.want {
				t.Fatalf("status = %d, want %d (%s)", res.StatusCode, c.want, c.why)
			}
			var body map[string]any
			_ = json.NewDecoder(res.Body).Decode(&body)
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, c.why) {
				t.Fatalf("the refusal does not explain itself (%q): %v", c.why, body["error"])
			}
		})
	}

	// Nothing was written by any of the refusals.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/taste", nil)
	req.AddCookie(cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got map[string]any
	_ = json.NewDecoder(res.Body).Decode(&got)
	if n, _ := got["count"].(float64); n != 0 {
		t.Fatalf("a refused import wrote %v tags", n)
	}
	if note, _ := got["note"].(string); !strings.Contains(note, "no taste") {
		t.Fatalf("an empty vector does not explain itself: %v", got["note"])
	}
}
