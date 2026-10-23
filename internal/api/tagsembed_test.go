package api

// The reader-facing half of #11: nearest tags in EMBEDDING space.
//
// WHY THIS IS SEPARATE FROM /api/v1/tags/{id}/similar's DEFAULT ANSWER:
//
// The co-occurrence answer is a statement about shared works — it can only
// name tags that appeared beside this one, and a tag on three works has almost
// no neighbours of that kind. The embedding answer is a statement about
// position, and it exists to reach the tags co-occurrence cannot name. A test
// that asserted "the endpoint returns tags" would pass against either, so this
// file asserts the two DIFFER and that the difference is the point.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
)

// putEmbeddings writes tag vectors into a running test server's state DB.
func putEmbeddings(t *testing.T, s *sql.DB, dim int, vecs map[int64][]float32) {
	t.Helper()
	for id, v := range vecs {
		if len(v) != dim {
			t.Fatalf("tag %d has %d coords, want %d", id, len(v), dim)
		}
		blob := make([]byte, dim*4)
		for i, x := range v {
			binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(x))
		}
		if _, err := s.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO embeddings(kind, entity_id, dim, vec) VALUES('tag',?,?,?)`,
			id, dim, blob); err != nil {
			t.Fatalf("insert embedding for tag %d: %v", id, err)
		}
	}
}

// TestSimilarTagsSpaceEmbedAnswersFromVectors proves the route reads the
// matrix, names the tags, and orders by similarity.
func TestSimilarTagsSpaceEmbedAnswersFromVectors(t *testing.T) {
	ts, s := newTestServerFull(t)
	// The fixture has tags 1..5. Tag 1 and tag 2 point nearly the same way;
	// tag 5 points the other way, so the answer has an order the test can
	// falsify.
	putEmbeddings(t, s.DB, 2, map[int64][]float32{
		1: {1, 0},
		2: {0.98, 0.2},
		3: {0.5, 0.866},
		4: {-1, 0},
		5: {-0.9, 0.1},
	})

	var body struct {
		Space     string `json:"space"`
		Tag       string `json:"tag"`
		Returned  int    `json:"returned"`
		Requested int    `json:"requested"`
		Similar   []struct {
			ID         int     `json:"id"`
			Name       string  `json:"name"`
			Similarity float64 `json:"similarity"`
		} `json:"similar"`
	}
	res, err := http.Get(ts.URL + "/api/v1/tags/1/similar?space=embed&n=3")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("space=embed -> %d, want 200", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Space != "embed" {
		t.Fatalf("space = %q; a caller must be able to tell which space answered", body.Space)
	}
	if body.Returned != len(body.Similar) || body.Returned == 0 {
		t.Fatalf("returned = %d with %d rows: an absence assertion needs its subject counted",
			body.Returned, len(body.Similar))
	}
	// The query tag is excluded: "tag 1 is most similar to tag 1" is not an
	// answer anyone asked for, and it would silently occupy a slot.
	for _, row := range body.Similar {
		if row.ID == 1 {
			t.Fatalf("the query tag (1) is in its own neighbourhood")
		}
		if row.Name == "" {
			t.Fatalf("tag %d came back with no name; the id is not actionable", row.ID)
		}
	}
	// Nearest first: tag 2 is nearly parallel to tag 1, tags 4/5 point away.
	if body.Similar[0].ID != 2 {
		t.Fatalf("first neighbour is tag %d (%.3f), want tag 2: the order is by similarity",
			body.Similar[0].ID, body.Similar[0].Similarity)
	}
	if body.Similar[0].Similarity <= body.Similar[len(body.Similar)-1].Similarity {
		t.Fatalf("similarities are not descending: %+v", body.Similar)
	}
}

// TestSimilarTagsSpaceDistinguishesTheTwoSpaces is the row's actual claim.
//
// The fixture has NO co-occurrence graph (newTestServerFull does not build
// one), so the comparison is made where it can be made honestly: the test
// proves from the CORPUS that the graph could not have named tag 5 for tag 2
// — no work carries both — and then proves the embedding answer does name it.
// Asserting "the two endpoints differ" against a 503 would be asserting that
// one of them is broken.
func TestSimilarTagsSpaceDistinguishesTheTwoSpaces(t *testing.T) {
	ts, s := newTestServerFull(t)
	putEmbeddings(t, s.DB, 2, map[int64][]float32{
		1: {0, 1},
		2: {1, 0},
		3: {0.9, 0.1},
		4: {0, -1},
		5: {0.95, 0.05},
	})

	// The premise, asserted rather than assumed: tag 2 and tag 5 share no
	// work in this corpus, so co-occurrence has nothing to say about them.
	var shared int
	if err := s.Corpus.QueryRow(
		`SELECT COUNT(*) FROM work_tags a JOIN work_tags b ON a.work_id = b.work_id
		 WHERE a.tag_id = 2 AND b.tag_id = 5`).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared != 0 {
		t.Fatalf("tags 2 and 5 share %d works in the fixture; the embedding "+
			"neighbourhood would then prove nothing the graph could not also say", shared)
	}

	em, err := http.Get(ts.URL + "/api/v1/tags/2/similar?space=embed&n=10")
	if err != nil {
		t.Fatal(err)
	}
	defer em.Body.Close()
	var emBody struct {
		Similar []struct {
			ID int `json:"id"`
		} `json:"similar"`
	}
	if err := json.NewDecoder(em.Body).Decode(&emBody); err != nil {
		t.Fatal(err)
	}
	if len(emBody.Similar) == 0 {
		t.Fatal("the embedding neighbourhood is empty: an absence assertion here " +
			"would pass for the wrong reason")
	}
	var embedHas5 bool
	for _, row := range emBody.Similar {
		if row.ID == 5 {
			embedHas5 = true
		}
	}
	if !embedHas5 {
		t.Fatalf("tag 5 is 0.998 cosine from tag 2, shares no work with it, and is "+
			"not in its embedding neighbourhood (%+v)", emBody.Similar)
	}
}

// TestSimilarTagsEmbedSaysSoWhenThereAreNoVectors: a deployment that never ran
// `kindred embed` must be told apart from one where nothing is near.
func TestSimilarTagsEmbedSaysSoWhenThereAreNoVectors(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/tags/2/similar?space=embed")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no embeddings -> %d, want 503 with the reason; an empty 200 "+
			"reads as \"nothing is near this tag\"", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["hint"] == nil || body["hint"] == "" {
		t.Fatalf("503 without a hint: %v", body)
	}
}

// TestSimilarTagsUnknownSpaceIsNamed: a typo must not silently fall back to a
// different answer than the caller asked for.
func TestSimilarTagsUnknownSpaceIsNamed(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/tags/2/similar?space=cosine")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown space -> %d, want 400", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	msg, _ := body["error"].(string)
	if msg == "" || !strings.Contains(msg, "space") {
		t.Fatalf("the error does not name the parameter: %v", body)
	}
}

// TestSimilarTagsMinSimIsValidated: a similarity is 0..1, and a bad one is a
// 400 rather than a silently empty list.
func TestSimilarTagsMinSimIsValidated(t *testing.T) {
	ts, s := newTestServerFull(t)
	putEmbeddings(t, s.DB, 2, map[int64][]float32{1: {1, 0}, 2: {0.8, 0.6}})
	res, err := http.Get(ts.URL + "/api/v1/tags/1/similar?space=embed&min_sim=7")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("min_sim=7 -> %d, want 400: %s", res.StatusCode, res.Status)
	}
	// And a valid one filters. The fixture's tag 2 is cosine 0.8 from tag 1,
	// so a 0.95 floor must exclude it — a floor that is above the fixture's
	// real similarity, not one tuned to the answer.
	ok, err := http.Get(ts.URL + "/api/v1/tags/1/similar?space=embed&min_sim=0.95")
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Body.Close()
	var body struct {
		Returned int `json:"returned"`
	}
	if err := json.NewDecoder(ok.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Returned != 0 {
		t.Fatalf("min_sim=0.95 returned %d rows, and the only other tag is cosine "+
			"0.8 away: the floor is not being applied", body.Returned)
	}
}
