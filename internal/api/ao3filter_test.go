package api

// This file exists because the 100-idea list scored several "already in the
// API, just add the UI" items very high, and checking each one against the
// server found the claim false in a specific, dangerous way:
//
//   - `sort=words` is wired in the query but NO test proves it, because every
//     existing fixture writes word_count as a CONSTANT (5000 for all four
//     works in newTestServer) or, in the e2e fixture, perfectly correlated
//     with hits. A sort that cannot change the order cannot fail.
//   - `complete=`, `words=under:N`, `rating=`, `lang=` are accepted by the
//     query string and IGNORED, returning byte-identical rows. That is the
//     accepted-and-did-nothing shape this repo has now found five times, and
//     it is the most dangerous form here because a UI that renders a
//     "complete only" checkbox would look like it works.
//
// The rule these tests follow, learned from the near-miss where ?max=1 and an
// unfiltered query both returned exactly 50 rows: a filter is proven by
// comparing ID SETS, not counts. Identical counts can coincide; identical sets
// prove inertness.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// newDiscriminatingServer builds a corpus where every sortable and filterable
// column VARIES and is INDEPENDENT of every other.
//
// This is the point of the fixture. The shared newTestServer cannot answer a
// question about word_count because all four of its works have word_count
// 5000, so ORDER BY word_count DESC and ORDER BY hits DESC return the same
// list for reasons that have nothing to do with the code under test.
func newDiscriminatingServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	corpusPath := filepath.Join(dir, "corpus.db")

	{
		f, err := sql.Open("sqlite", corpusPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Exec(corpusSchema); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	s, err := store.Open(context.Background(), dbPath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()

	// 6 works. Every column is built so that no two sorts can agree by
	// accident:
	//
	//	id | words | hits  | kudos | complete | rating                | language
	//	 1 |  1000 | 60000 |    10 |        1 | General Audiences     | English
	//	 2 |  2000 | 50000 |    20 |        0 | Teen And Up Audiences | Spanish
	//	 3 |  3000 | 40000 |    30 |        1 | Mature                | English
	//	 4 |  4000 | 30000 |    40 |        0 | Explicit              | French
	//	 5 |  5000 | 20000 |    50 |        1 | General Audiences     | Spanish
	//	 6 |  6000 | 10000 |    60 |        0 | Teen And Up Audiences | English
	//
	// words ascend with id; hits, kudos descend. So the top id differs for
	// every sort, and a fixture that made them agree would make this test
	// vacuous.
	//
	// The ratings are the FULL names the real mirror stores, measured
	// across all 112,935 rows -- not the single letters AO3's query
	// language uses. This fixture originally stored "G"/"T"/"M"/"E",
	// which is why `rating=G` passed here and returned NOTHING against
	// the real corpus: the letters the fixture invented were the letters
	// the mirror never stored. A fixture whose vocabulary differs from the
	// product's hides exactly this class of bug.
	type row struct {
		id                 int
		words, hits, kudos int
		complete           int
		rating, language   string
	}
	const (
		gen  = "General Audiences"
		teen = "Teen And Up Audiences"
		mat  = "Mature"
		exp  = "Explicit"
	)
	rows := []row{
		{1, 1000, 60000, 10, 1, gen, "English"},
		{2, 2000, 50000, 20, 0, teen, "Spanish"},
		{3, 3000, 40000, 30, 1, mat, "English"},
		{4, 4000, 30000, 40, 0, exp, "French"},
		{5, 5000, 20000, 50, 1, gen, "Spanish"},
		{6, 6000, 10000, 60, 0, teen, "English"},
	}
	for _, r := range rows {
		if _, err := seed.Exec(
			`INSERT INTO works(id,url,title,word_count,hits,kudos,bookmarks,
			   update_date,first_seen,complete,rating,language)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.id, fmt.Sprintf("https://example.com/%d", r.id),
			fmt.Sprintf("Work %d", r.id), r.words, r.hits, r.kudos, 0,
			"2026-06-01", "2026-06-01", r.complete, r.rating, r.language); err != nil {
			t.Fatal(err)
		}
		// One tag per work, so the graph has nodes without making any
		// tag co-occur, which would pollute nothing here but keeps the
		// fixture minimal and deterministic.
		if _, err := seed.Exec(
			`INSERT INTO tags(id,name) VALUES(?,?)`, r.id, fmt.Sprintf("tag%d", r.id)); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Exec(
			`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,?,'freeforms')`,
			r.id, r.id); err != nil {
			t.Fatal(err)
		}
	}

	eng := &engine.Engine{
		Store:    s,
		Corpus:   corpus.NewAO3(s.Corpus),
		PoolSize: 50,
		TopN:     10,
		Lite:     true,
	}
	srv := &Server{
		Engine:    eng,
		Store:     s,
		Version:   "test",
		StartedAt: time.Now().Add(-time.Minute),
		Lite:      true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// workIDs returns the ordered ID list from /api/v1/ao3/works.
func workIDs(t *testing.T, ts *httptest.Server, query string) []int64 {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/v1/ao3/works?" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/ao3/works?%s = %d", query, resp.StatusCode)
	}
	var body struct {
		Works []struct {
			ID int64 `json:"id"`
		} `json:"works"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	out := make([]int64, len(body.Works))
	for i, w := range body.Works {
		out[i] = w.ID
	}
	return out
}

func int64sEq(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSortWordsIsActuallyWired pins idea #5's premise, which the list scored
// at 80 on the grounds that "the API supports all four sorts".
//
// The sort is wired in the handler, so this test is the thing that keeps it
// wired: it asserts the ORDER, not merely that the parameter is accepted.
func TestSortWordsIsActuallyWired(t *testing.T) {
	ts := newDiscriminatingServer(t)

	byWords := workIDs(t, ts, "limit=10&sort=words")
	if len(byWords) == 0 {
		t.Fatal("the fixture returned no works")
	}
	// words ascend with id in the fixture, so DESC is 6,5,4,3,2,1.
	want := []int64{6, 5, 4, 3, 2, 1}
	if !int64sEq(byWords, want) {
		t.Errorf("sort=words returned %v, want %v (descending word_count)", byWords, want)
	}

	// And it must genuinely differ from the other sorts. If every sort
	// returned the same order, the first assertion above could pass for the
	// wrong reason.
	byHits := workIDs(t, ts, "limit=10&sort=hits")
	if int64sEq(byWords, byHits) {
		t.Error("sort=words and sort=hits returned the same order; the fixture " +
			"is degenerate and this test proves nothing")
	}
}

// TestSortKudosAndDateAreWired pins the other two sorts the list relies on.
func TestSortKudosAndDateAreWired(t *testing.T) {
	ts := newDiscriminatingServer(t)

	// kudos descend with id, so DESC is 6..1.
	if got, want := workIDs(t, ts, "limit=10&sort=kudos"),
		[]int64{6, 5, 4, 3, 2, 1}; !int64sEq(got, want) {
		t.Errorf("sort=kudos returned %v, want %v", got, want)
	}

	// All six works share an update_date, so date order cannot discriminate.
	// What it CAN prove is that an unrecognised sort does not silently
	// become date: the handler's default is hits, and `sort=bogus` must
	// fall back to that default rather than to whatever the switch happens
	// to do last. This is the accepted-and-did-nothing guard.
	def := workIDs(t, ts, "limit=10")
	bogus := workIDs(t, ts, "limit=10&sort=bogus")
	if !int64sEq(def, bogus) {
		t.Errorf("sort=bogus returned %v but the default returned %v; an "+
			"unknown sort must fall back to the documented default", bogus, def)
	}
}

// TestWorksFiltersActuallyFilter is the gate for the four parameters the 100-
// idea list scored at 63-80 on the claim that "it's already in the API, just
// add the UI".
//
// It was not in the API. All four were accepted in the query string and
// ignored, returning byte-identical rows, which is the accepted-and-did-
// nothing shape this repo has now found five times -- and the most dangerous
// instance yet, because a "complete only" checkbox rendered on top of it
// would look like it worked.
//
// The fixture is the one from newDiscriminatingServer, where every filtered
// column varies and no two columns agree, so a filter that returns the wrong
// subset cannot be mistaken for one that returned nothing to exclude.
//
// Each case compares ID SETS, never counts. Identical counts can coincide;
// identical sets prove inertness. That rule comes from a near-miss in another
// project where ?max=1 and an unfiltered query both returned exactly 50 rows
// and the filter looked broken when it was not.
func TestWorksFiltersActuallyFilter(t *testing.T) {
	ts := newDiscriminatingServer(t)

	// Fixture (from newDiscriminatingServer):
	//   id | words | complete | rating | language
	//    1 |  1000 |    yes    |   G    | English
	//    2 |  2000 |    no     |   T    | Spanish
	//    3 |  3000 |    yes    |   M    | English
	//    4 |  4000 |    no     |   E    | French
	//    5 |  5000 |    yes    |   G    | Spanish
	//    6 |  6000 |    no     |   T    | English
	cases := []struct {
		name  string
		query string
		want  []int64
	}{
		{"complete=true", "complete=true", []int64{1, 3, 5}},
		{"complete=false", "complete=false", []int64{2, 4, 6}},
		{"words=under:3000", "words=under:3000", []int64{1, 2}},
		{"words=over:5000", "words=over:5000", []int64{6}},
		// The < and > spellings are the AO3 forms and must work too.
		{"words=>5000", "words=>5000", []int64{6}},
		{"words=<2000", "words=<2000", []int64{1}},
		// AO3's letters, which the API translates.
		{"rating=G", "rating=G", []int64{1, 5}},
		{"rating=G,T", "rating=G,T", []int64{1, 2, 5, 6}},
		{"rating=E", "rating=E", []int64{4}},
		// The mirror's own spelling, which must work too -- both are what
		// readers type, and a client that reads `rating` out of this API
		// and sends it back gets the same works.
		{"rating=General Audiences", "rating=General+Audiences", []int64{1, 5}},
		{"rating=Mature", "rating=Mature", []int64{3}},
		{"lang=English", "lang=English", []int64{1, 3, 6}},
		{"lang=Spanish", "lang=Spanish", []int64{2, 5}},
		// Two filters at once: the single-WHERE-clause version this
		// replaced could not express this at all.
		{"complete=true + rating=G", "complete=true&rating=G", []int64{1, 5}},
		{"complete=true + words=over:2000", "complete=true&words=over:2000", []int64{3, 5}},
		{"rating=T + lang=Spanish", "rating=T&lang=Spanish", []int64{2}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := workIDs(t, ts, "limit=100&"+c.query)
			if !int64sEq(got, c.want) {
				t.Errorf("%s returned %v, want %v\n"+
					"  (an unfiltered list is %v -- if that is what you got, the "+
					"parameter is being ignored)",
					c.query, got, c.want, workIDs(t, ts, "limit=100"))
			}
		})
	}
}

// TestFixtureCanDistinguishFilteredFromUnfiltered is what stops
// TestWorksFiltersActuallyFilter passing for the wrong reason: if the fixture
// had no incomplete works, `complete=true` returning everything would be
// indistinguishable from filtering.
func TestFixtureCanDistinguishFilteredFromUnfiltered(t *testing.T) {
	ts := newDiscriminatingServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/ao3/works?limit=100")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Works []struct {
			ID        int64 `json:"id"`
			Complete  any   `json:"complete"`
			WordCount int64 `json:"word_count"`
		} `json:"works"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var complete, incomplete, under3k int
	for _, w := range body.Works {
		if w.Complete == true {
			complete++
		} else {
			incomplete++
		}
		if w.WordCount < 3000 {
			under3k++
		}
	}
	if complete == 0 || incomplete == 0 {
		t.Fatalf("fixture cannot distinguish complete from incomplete: "+
			"%d complete, %d incomplete; complete=true filtering is unfalsifiable",
			complete, incomplete)
	}
	if under3k == 0 || under3k == len(body.Works) {
		t.Fatalf("fixture has %d of %d works under 3000 words; words=under:3000 "+
			"either cannot filter or cannot exclude", under3k, len(body.Works))
	}
}

// TestMalformedFilterValuesAreRejected pins the other half of the change.
//
// A filter value that cannot be understood is a 400, never a silently
// dropped parameter. `words=lots` returning every work is indistinguishable
// from `words=under:10000` returning every work when the reader meant
// something by it -- and the first is a bug while the second is a legitimate
// empty result.
func TestMalformedFilterValuesAreRejected(t *testing.T) {
	ts := newDiscriminatingServer(t)
	bad := []string{
		"complete=maybe",
		"words=lots",
		"words=under:",
		"words=under:abc",
		"words=under:-5",
		"rating=,,",
	}
	for _, q := range bad {
		t.Run(q, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/api/v1/ao3/works?limit=10&" + q)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				var body map[string]any
				json.NewDecoder(resp.Body).Decode(&body)
				t.Errorf("?%s = %d, want 400; a filter value that cannot be "+
					"parsed must not be ignored (body: %v)", q,
					resp.StatusCode, body["error"])
			}
		})
	}
}

// TestFiltersAppliedIsEchoed pins the honesty requirement: a client must be
// able to tell "your filter matched nothing" from "your filter was ignored".
// Before this change both returned every work with no indication either way.
func TestFiltersAppliedIsEchoed(t *testing.T) {
	ts := newDiscriminatingServer(t)

	get := func(query string) map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/v1/ao3/works?limit=10&" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	body := get("complete=true&words=over:5000")
	applied, ok := body["filters_applied"].(map[string]any)
	if !ok {
		t.Fatalf("filters_applied is %T (%v), want an object; a client "+
			"cannot tell an ignored filter from an empty result without it",
			body["filters_applied"], body["filters_applied"])
	}
	if applied["complete"] != "true" || applied["words"] != "over:5000" {
		t.Errorf("filters_applied = %v, want complete=true and words=over:5000", applied)
	}

	// Unfiltered is explicitly empty, so "no filters" and "a server too old to
	// report them" are different facts.
	body = get("")
	applied, ok = body["filters_applied"].(map[string]any)
	if !ok {
		t.Fatalf("unfiltered response has no filters_applied object, want {}")
	}
	if len(applied) != 0 {
		t.Errorf("unfiltered response reported filters %v, want none", applied)
	}
}

// TestAFilterMatchingNothingIsAnEmptyListNotAnError is the distinction the
// whole change rests on: a real filter that matches nothing must return an
// empty list with 200, so the reader sees "no works match" rather than a
// fault. This is what a UI can render honestly.
func TestAFilterMatchingNothingIsAnEmptyListNotAnError(t *testing.T) {
	ts := newDiscriminatingServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/ao3/works?limit=10&words=under:1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a filter matching nothing = %d, want 200 with an empty list",
			resp.StatusCode)
	}
	var body struct {
		Works []json.RawMessage `json:"works"`
		Count int               `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 0 || len(body.Works) != 0 {
		t.Errorf("words=under:1 returned %d works, want 0", len(body.Works))
	}
}

// TestSortDefaultsToHits pins the documented default, which the handler's
// `order := "w.hits DESC"` is the only statement of.
func TestSortDefaultsToHits(t *testing.T) {
	ts := newDiscriminatingServer(t)
	def := workIDs(t, ts, "limit=10")
	hits := workIDs(t, ts, "limit=10&sort=hits")
	if !int64sEq(def, hits) {
		t.Errorf("the default order %v is not sort=hits (%v); the handler's "+
			"documented default and its behaviour disagree", def, hits)
	}
}

// TestWorksListIsStablyOrderedAcrossRequests guards the "identical set" trap
// from the other side: two unfiltered calls must return the SAME order, so a
// filter test comparing sets is comparing like with like.
func TestWorksListIsStablyOrderedAcrossRequests(t *testing.T) {
	ts := newDiscriminatingServer(t)
	a := workIDs(t, ts, "limit=100")
	b := workIDs(t, ts, "limit=100")
	if !int64sEq(a, b) {
		t.Errorf("two unfiltered requests returned different orders:\n  %v\n  %v",
			a, b)
	}
}
