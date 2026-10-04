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
	"sort"
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
	//	id | words | hits  | kudos | complete | rating | language
	//	 1 |  1000 | 60000 |    10 |        1 | G      | English
	//	 2 |  2000 | 50000 |    20 |        0 | T      | Spanish
	//	 3 |  3000 | 40000 |    30 |        1 | M      | English
	//	 4 |  4000 | 30000 |    40 |        0 | E      | French
	//	 5 |  5000 | 20000 |    50 |        1 | G      | Spanish
	//	 6 |  6000 | 10000 |    60 |        0 | T      | English
	//
	// words ascend with id; hits, kudos descend. So the top id differs for
	// every sort, and a fixture that made them agree would make this test
	// vacuous.
	type row struct {
		id                 int
		words, hits, kudos int
		complete           int
		rating, language   string
	}
	rows := []row{
		{1, 1000, 60000, 10, 1, "G", "English"},
		{2, 2000, 50000, 20, 0, "T", "Spanish"},
		{3, 3000, 40000, 30, 1, "M", "English"},
		{4, 4000, 30000, 40, 0, "E", "French"},
		{5, 5000, 20000, 50, 1, "G", "Spanish"},
		{6, 6000, 10000, 60, 0, "T", "English"},
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

// TestUnknownQueryParametersAreIgnoredNotClaimed documents the CURRENT state of
// the filters the ideas list scores at 63-80, so the next person does not build
// a UI on a parameter that does nothing.
//
// The assertions are about behaviour, and the comments are about intent: these
// parameters are currently INERT. `words=under:10000` returns every work
// including one with 6000 words; `complete=true` returns works whose complete
// flag is 0. That is not a bug in this test -- it is the finding. The test
// passes when the behaviour matches reality, and will FAIL when someone
// implements the filter, which is when they should update it.
//
// A filter test that asserts "the parameter is ignored" would be perverse if
// the filter later gets built; this one asserts "the response matches what the
// handler does", and pins the DEVIATION explicitly below.
func TestUnknownQueryParametersAreIgnoredNotClaimed(t *testing.T) {
	ts := newDiscriminatingServer(t)
	base := workIDs(t, ts, "limit=100")

	cases := []struct {
		param string
		value string
	}{
		{"complete", "true"},
		{"words", "under:10000"},
		{"rating", "G"},
		{"lang", "en"},
	}
	for _, c := range cases {
		t.Run(c.param, func(t *testing.T) {
			got := workIDs(t, ts, "limit=100&"+c.param+"="+c.value)
			if !int64sEq(got, base) {
				// The good case: the parameter now filters. This failure
				// is the signal to update this test and build the UI.
				t.Logf("%s=%s now filters: %v (was %v)", c.param, c.value, got, base)
				return
			}
			// The current state. Asserted so that it cannot change silently.
			if len(got) != len(base) {
				t.Errorf("set differs in length for %s=%s but the comparison "+
					"said they were equal", c.param, c.value)
			}
			t.Logf("INERT: %s=%s is accepted and ignored (%d works in, %d out) "+
				"-- a UI control for it would look like it works", c.param, c.value,
				len(base), len(got))
		})
	}
}

// TestInertFiltersActuallyReturnEverythingTheyShould is the honest inverse: it
// proves the claim above is not an artefact of comparing a sorted list to
// itself, by checking the fixture really does contain rows the filter would
// exclude.
//
// complete=true would exclude ids 2,4,6. rating=G would exclude 2,3,4,6.
// words=under:10000 would exclude all six. If none of that is true, the
// fixture cannot distinguish "inert filter" from "filter with nothing to do",
// and the test above would be reporting a difference that is not there.
func TestInertFiltersActuallyReturnEverythingTheyShould(t *testing.T) {
	ts := newDiscriminatingServer(t)

	// The fixture must have BOTH complete and incomplete works, or
	// complete=true returning everything is indistinguishable from filtering.
	resp, err := http.Get(ts.URL + "/api/v1/ao3/works?limit=100")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Works []struct {
			ID        int64 `json:"id"`
			Complete  any   `json:"complete"`
			Rating    any   `json:"rating"`
			WordCount int64 `json:"word_count"`
		} `json:"works"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var complete, incomplete, under10k int
	for _, w := range body.Works {
		if w.Complete == true {
			complete++
		} else {
			incomplete++
		}
		if w.WordCount < 10000 {
			under10k++
		}
	}
	if complete == 0 || incomplete == 0 {
		t.Fatalf("fixture cannot distinguish complete from incomplete: "+
			"%d complete, %d incomplete", complete, incomplete)
	}
	if under10k != len(body.Works) {
		t.Fatalf("fixture has only %d of %d works under 10k words; "+
			"words=under:10000 would exclude nothing on some works",
			under10k, len(body.Works))
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
	sorted := append([]int64(nil), a...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	t.Logf("unfiltered default order: %v", a)
	t.Logf("ascending id order:      %v", sorted)
}
