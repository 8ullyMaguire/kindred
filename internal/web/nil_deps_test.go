package web

// W4: nil-dependency degradation.
//
// Every page must answer with a status and a reason when a dependency is
// missing -- never take the process down. Three corpus pages already did this;
// /search and /works/{id} panicked. These tests pin the whole set, and the
// mutation gate is what makes them worth having: `hasCorpus() { return true }`
// must fail them.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
)

// TestSearchIsNotRefusedWhenACorpusIsPresent is the counter-test, and it exists
// because the mutation gate demanded it: with `hasCorpus() { return false }`
// every assertion above still passed. Each expected 503 is *also* the expected
// answer under a permanent refusal, so the table proves the guard fires but
// cannot prove it ever allows anything through.
//
// An empty in-memory database is enough: the point is that the guard lets the
// handler reach the corpus, not that a query succeeds. searchTags turns a
// failing query into page.TagError, so the page still renders.
func TestSearchIsNotRefusedWhenACorpusIsPresent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening an in-memory sqlite: %v", err)
	}
	defer db.Close()

	d := Deps{Version: "test", Engine: &engine.Engine{Corpus: corpus.NewAO3(db)}}
	if !d.hasCorpus() {
		t.Fatal("hasCorpus() is false for a Deps that has an Engine with a Corpus")
	}

	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/search?q=x", nil))

	if rec.Code == http.StatusServiceUnavailable {
		t.Error("GET /search?q=x = 503 with a corpus present; hasCorpus() refuses too much")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("GET /search?q=x = %d; want 200", rec.Code)
	}
}

// fakeCorpus satisfies CorpusQuerier with empty results. It exists to prove the
// corpus pages render when their dependency IS present, so the 503 assertions
// below are about the dependency and not about the page being unable to render.
type fakeCorpus struct{ calls int }

func (f *fakeCorpus) FandomRanking(context.Context, []int32, corpusquery.Options) (corpusquery.Result, error) {
	f.calls++
	return corpusquery.Result{}, nil
}
func (f *fakeCorpus) Underrated(context.Context, corpusquery.Options) (corpusquery.Result, error) {
	f.calls++
	return corpusquery.Result{}, nil
}
func (f *fakeCorpus) TagNeighbours(context.Context, string, corpusquery.Options) (corpusquery.Result, error) {
	f.calls++
	return corpusquery.Result{}, nil
}
func (f *fakeCorpus) Surprise(context.Context, corpusquery.Options) (corpusquery.Result, error) {
	f.calls++
	return corpusquery.Result{}, nil
}

func TestPagesDegradeInsteadOfPanickingWhenTheCorpusIsMissing(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
		why  string
	}{
		// The front page is the search box with no query, and it needs no
		// corpus. It must keep working on a server that has none.
		{"/", http.StatusOK, "the home page needs no corpus"},
		{"/profiles", http.StatusOK, "profiles come from the state db, not the corpus"},
		// /crawl with no queue is a page that says so, not a 503 — the same
		// reasoning as /profiles: the absence is a configuration, and the page
		// can explain it in words. A 503 here would tell a read-only deployment
		// that its site is broken rather than that it is read-only.
		{"/crawl", http.StatusOK, "a deployment with no queue must explain itself, not 503"},
		// These four cannot work without the corpus, and each must say so.
		{"/search?q=x", http.StatusServiceUnavailable, "a search that cannot search must not read as 'nothing matched'"},
		{"/works/1", http.StatusServiceUnavailable, "must not 404: that claims the work does not exist"},
		// The family half answers from the corpus too, and an empty result on a
		// corpus-less deployment would read as "no work matched", which is a
		// claim about data nobody has.
		{"/search?universe=worm", http.StatusServiceUnavailable, "the family intersection needs the corpus"},
		{"/fandoms", http.StatusServiceUnavailable, "corpus queries are not enabled"},
		{"/underrated", http.StatusServiceUnavailable, "corpus queries are not enabled"},
		{"/surprise", http.StatusServiceUnavailable, "corpus queries are not enabled"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			// A zero Deps: no Engine, no CorpusQuery, no Profiles. This is the
			// state the assertion is about, so it is built here rather than
			// arranged by a helper that could drift.
			d := Deps{Version: "test"}
			rec := httptest.NewRecorder()

			// Recover rather than let the panic abort the run, so every path is
			// reported and not just the first one that crashes.
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("GET %s panicked with a zero Deps: %v", tc.path, r)
					}
				}()
				d.route(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			}()

			if rec.Code != tc.want {
				t.Errorf("GET %s = %d; want %d (%s)", tc.path, rec.Code, tc.want, tc.why)
			}
		})
	}
}

func TestAnEngineWithoutACorpusAlsoDegrades(t *testing.T) {
	// hasCorpus() checks TWO things, and the table above only exercises the
	// first: a zero Deps has a nil Engine, so `d.Engine != nil` short-circuits
	// and the Corpus half is never reached. An Engine that exists but carries
	// no corpus is exactly the state internal/api/server.go guards for
	// (`s.Engine != nil && s.Engine.Corpus != nil` when it builds CorpusQuery),
	// so it is the half a test is most likely to leave unverified.
	d := Deps{Version: "test", Engine: &engine.Engine{}}

	for _, path := range []string{"/search?q=x", "/works/1"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("GET %s panicked with a corpus-less Engine: %v", path, r)
					}
				}()
				d.route(rec, httptest.NewRequest(http.MethodGet, path, nil))
			}()
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("GET %s = %d; want 503", path, rec.Code)
			}
		})
	}

	// And the front page still works in this state too.
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET / = %d with a corpus-less Engine; want 200", rec.Code)
	}
}

func TestTheNoCorpusPageSaysWhy(t *testing.T) {
	// A 503 with an empty body is a dead end for whoever is looking at it. The
	// reason is the whole value of answering 503 rather than 500.
	d := Deps{Version: "test"}
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/search?q=anything", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(strings.ToLower(body), "corpus") {
		t.Errorf("the 503 body does not mention the corpus, so it does not explain itself: %q", body)
	}
}

func TestCorpusPagesStillRenderWhenTheirDependencyIsPresent(t *testing.T) {
	// The other half of the guard: it must not be so eager that a server WITH a
	// corpus query runner is refused. If this fails while the tests above pass,
	// the guard has been put on the wrong condition.
	for _, path := range []string{"/fandoms", "/underrated", "/surprise"} {
		t.Run(path, func(t *testing.T) {
			fake := &fakeCorpus{}
			d := Deps{Version: "test", CorpusQuery: fake}
			rec := httptest.NewRecorder()
			d.route(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code == http.StatusServiceUnavailable {
				t.Errorf("GET %s = 503 with a CorpusQuery present; the guard is on the wrong condition", path)
			}
			if fake.calls == 0 {
				t.Errorf("GET %s never called the corpus querier", path)
			}
		})
	}
}
