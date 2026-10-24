package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The author route is addressed by NAME, not by an id, because the mirror has
// no author table. These tests pin the three answers the route owes a caller:
// a ranked list, a bad request that says what is missing, and a not-found that
// names the author it could not find.
func TestSimilarAuthors(t *testing.T) {
	ts := newTestServer(t)

	t.Run("fixture author", func(t *testing.T) {
		res, err := http.Get(ts.URL + "/api/v1/authors/similar?name=author1")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		var body struct {
			Author string `json:"author"`
			Items  []struct {
				Name        string  `json:"name"`
				SharedWorks int     `json:"shared_works"`
				Score       float64 `json:"score"`
				Evidence    []struct {
					Reason string `json:"reason"`
				} `json:"evidence"`
			} `json:"items"`
			Returned int `json:"returned"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Author != "author1" {
			t.Fatalf("author = %q, want author1", body.Author)
		}
		if body.Returned != len(body.Items) {
			t.Fatalf("returned = %d, items = %d", body.Returned, len(body.Items))
		}
		// author2 shares tags 1 and 2 with author1; author3 shares tag 2.
		// The seed itself must never appear in its own neighbourhood.
		seen := map[string]bool{}
		for _, it := range body.Items {
			seen[it.Name] = true
			if it.Name == "author1" {
				t.Fatalf("the seed author is in its own similar list: %+v", body.Items)
			}
			if it.Evidence == nil {
				t.Fatalf("%s has no evidence; a bare score is not an answer", it.Name)
			}
		}
		if !seen["author2"] {
			t.Fatalf("items = %+v, want author2 (shares both seed tags)", body.Items)
		}
		if !seen["author3"] {
			t.Fatalf("items = %+v, want author3 (shares tag 2)", body.Items)
		}
	})

	t.Run("missing name", func(t *testing.T) {
		res, err := http.Get(ts.URL + "/api/v1/authors/similar")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("unknown author", func(t *testing.T) {
		res, err := http.Get(ts.URL + "/api/v1/authors/similar?name=zzzznobody")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", res.StatusCode)
		}
	})
}

// `by` names the arm. The default merges co-readership with the tag profile when this
// mirror has an index; without one the response must SAY it answered from tags rather than
// claim an ordering nothing computed. `by=tags` reproduces the pre-#35 order, and an unknown
// arm is a 400 that names the two it accepts.
func TestSimilarAuthorsArms(t *testing.T) {
	ts := newTestServer(t)

	read := func(t *testing.T, q string) (int, map[string]any) {
		t.Helper()
		res, err := http.Get(ts.URL + "/api/v1/authors/similar?name=author1" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, body
	}

	// The fixture server has no interaction table, so there is no co-readership index: the
	// default arm must report itself as the tag profile.
	code, body := read(t, "")
	if code != http.StatusOK {
		t.Fatalf("default status = %d, want 200", code)
	}
	if got := body["ranked_by"]; got != "tag_profile" {
		t.Fatalf("default ranked_by = %v, want tag_profile on a mirror with no co-readership index", got)
	}

	code, body = read(t, "&by=tags")
	if code != http.StatusOK {
		t.Fatalf("by=tags status = %d, want 200", code)
	}
	if got := body["ranked_by"]; got != "tag_profile" {
		t.Fatalf("by=tags ranked_by = %v", got)
	}
	if n, _ := body["returned"].(float64); n == 0 {
		t.Fatal("by=tags returned no authors on a fixture where every author shares tags")
	}

	code, body = read(t, "&by=popularity")
	if code != http.StatusBadRequest {
		t.Fatalf("by=popularity status = %d, want 400", code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "readers") || !strings.Contains(msg, "tags") {
		t.Fatalf("the 400 does not name the accepted arms: %v", body["error"])
	}
}
