package api

// Work-pair similarity (#78) over HTTP.
//
// The engine's own tests cover the arithmetic. These cover the contract a
// client depends on: the pair is symmetric, a typo is named rather than scored
// zero, and a work is not a pair with itself.
//
// The fixture is the shared one: works 1 and 2 carry tags 1 and 2, so they share
// two tags; work 4 carries only tag 5 and shares nothing.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

type similarityBody struct {
	A struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	} `json:"a"`
	B struct {
		ID int64 `json:"id"`
	} `json:"b"`
	SharedTags []struct {
		ID   int64   `json:"id"`
		Name string  `json:"name"`
		IDF  float64 `json:"idf"`
	} `json:"shared_tags"`
	TagOverlap  float64  `json:"tag_overlap"`
	EmbedCosine float64  `json:"embed_cosine"`
	Score       float64  `json:"score"`
	Missing     []string `json:"missing"`
}

func fetchSimilarity(t *testing.T, url string, out any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("decode: %v\n%s", err, body)
		}
	}
	return res.StatusCode
}

func TestWorkSimilarityReportsBothSidesAndTheSharedTags(t *testing.T) {
	ts := newTestServer(t)
	var body similarityBody
	if code := fetchSimilarity(t, ts.URL+"/api/v1/works/1/similarity/2", &body); code != http.StatusOK {
		t.Fatalf("GET pair -> %d, want 200", code)
	}
	if body.A.ID != 1 || body.B.ID != 2 {
		t.Fatalf("the report names works %d and %d, want 1 and 2", body.A.ID, body.B.ID)
	}
	if len(body.SharedTags) != 2 {
		t.Fatalf("the fixture pair shares %d tags, want 2: every assertion below is "+
			"vacuous if the pair shares nothing", len(body.SharedTags))
	}
	for _, st := range body.SharedTags {
		if st.Name == "" {
			t.Fatalf("shared tag %d has no name; the list is the explanation", st.ID)
		}
	}
	if body.Score <= 0 || body.Score > 1 {
		t.Fatalf("score %.4f is outside (0,1] for a pair that shares tags", body.Score)
	}
	// This fixture has no embeddings written, so the axis must be NAMED rather
	// than reported as a zero similarity.
	if !containsStr(body.Missing, "embed_cosine") {
		t.Fatalf("missing = %v, want embed_cosine named on a mirror with no embeddings",
			body.Missing)
	}
}

// TestAPairWithNothingInCommonScoresZero: the other side of the fixture, so the
// shared-tag case above cannot pass on an implementation that scores everything
// alike.
func TestAPairWithNothingInCommonScoresZero(t *testing.T) {
	ts := newTestServer(t)
	var body similarityBody
	if code := fetchSimilarity(t, ts.URL+"/api/v1/works/1/similarity/4", &body); code != http.StatusOK {
		t.Fatalf("GET unrelated pair -> %d", code)
	}
	if len(body.SharedTags) != 0 {
		t.Fatalf("work 1 and work 4 share %d tags", len(body.SharedTags))
	}
	if body.TagOverlap != 0 || body.Score != 0 {
		t.Fatalf("overlap %.4f score %.4f for a pair that shares no tag; the report "+
			"must not imply a resemblance", body.TagOverlap, body.Score)
	}
}

func TestWorkSimilarityIsSymmetric(t *testing.T) {
	ts := newTestServer(t)
	var ab, ba similarityBody
	if code := fetchSimilarity(t, ts.URL+"/api/v1/works/1/similarity/2", &ab); code != http.StatusOK {
		t.Fatalf("(1,2) -> %d", code)
	}
	if code := fetchSimilarity(t, ts.URL+"/api/v1/works/2/similarity/1", &ba); code != http.StatusOK {
		t.Fatalf("(2,1) -> %d", code)
	}
	if ab.TagOverlap != ba.TagOverlap || ab.Score != ba.Score {
		t.Fatalf("(1,2) scores %.6f/%.6f and (2,1) scores %.6f/%.6f: the order of the "+
			"arguments changed the answer", ab.TagOverlap, ab.Score, ba.TagOverlap, ba.Score)
	}
}

func TestWorkSimilarityNamesTheBadId(t *testing.T) {
	ts := newTestServer(t)
	cases := []struct {
		url  string
		want int
		why  string
	}{
		{"/api/v1/works/1/similarity/999999", http.StatusNotFound,
			"an unknown second work must not return a 0.0 report, which reads as " +
				"\"nothing in common\""},
		{"/api/v1/works/999999/similarity/1", http.StatusNotFound, "an unknown first work"},
		{"/api/v1/works/1/similarity/1", http.StatusBadRequest, "a work is not a pair with itself"},
		{"/api/v1/works/1/similarity/abc", http.StatusBadRequest, "a non-numeric id"},
	}
	for _, c := range cases {
		if code := fetchSimilarity(t, ts.URL+c.url, nil); code != c.want {
			t.Fatalf("%s -> %d, want %d (%s)", c.url, code, c.want, c.why)
		}
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
