package web

// The family intersection, folded from /browse into /search.
//
// These tests exist because the fold has two failure modes that a template
// check cannot see:
//
//  1. the capability silently stops working (the form renders, the query does
//     not run), and
//  2. the TEXT half runs on a family-only request — searchTags("") matches
//     every tag and searchWorks("") matches every work, so a missing guard
//     would answer "here are all 120 works" instead of the intersection.
//
// The fixture corpus is the same one the Go tests and the e2e suite use. Its
// tag vocabulary includes "harry potter - j. k. rowling" (a declared spelling
// of the harry potter family) and "alternate universe - canon divergence" (a
// declared tag of the alternate universe family), so the pair intersects for
// real rather than by luck.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func familyDeps(t *testing.T) Deps {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	corpusPath, err := testcorpus.New(120).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture corpus: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"), corpusPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return Deps{
		Version: "test",
		Engine: &engine.Engine{
			Store:  st,
			Corpus: corpus.NewAO3(st.Corpus),
		},
	}
}

// TestFamilySearchAnswersOnTheSearchPage is the capability: a pair returns
// rows, the counts are stated, and the rows link to real work pages.
func TestFamilySearchAnswersOnTheSearchPage(t *testing.T) {
	d := familyDeps(t)
	code, body := arenaBody(t, d, "/search?universe=harry+potter&concept=alternate+universe&recent=730")
	if code != http.StatusOK {
		t.Fatalf("GET /search?universe=... = %d, want 200", code)
	}
	if strings.Contains(body, `data-testid="family-error"`) {
		t.Fatalf("the family query reported an error: %s", body)
	}
	if !strings.Contains(body, `data-testid="family-counts"`) {
		t.Error("no pool counts: a reader cannot tell 'no overlap' from 'no data'")
	}
	if strings.Contains(body, `data-testid="family-empty"`) {
		t.Error("the fixture pair matched nothing; the query is not wired")
	}
	if !strings.Contains(body, `data-testid="family-works"`) {
		t.Error("no family rows rendered")
	}
	if !strings.Contains(body, `href="/work/`) {
		t.Error("the family rows do not link to work pages")
	}
	if !strings.Contains(body, "730 days") {
		t.Error("the recency window in force is not stated")
	}
}

// TestFamilyOnlySearchDoesNotRunTheTextHalf is the guard: an empty q must not
// reach searchTags/searchWorks. The failure looks like a helpful page (120
// works listed) and is exactly wrong.
func TestFamilyOnlySearchDoesNotRunTheTextHalf(t *testing.T) {
	d := familyDeps(t)
	code, body := arenaBody(t, d, "/search?universe=harry+potter&recent=730")
	if code != http.StatusOK {
		t.Fatalf("GET family-only = %d, want 200", code)
	}
	for _, marker := range []string{
		`data-testid="works"`,
		`data-testid="no-works"`,
		`data-testid="tag-count"`,
		`data-testid="no-tags"`,
	} {
		if strings.Contains(body, marker) {
			t.Errorf("a family-only request rendered the text section %s (empty q reached the text half)", marker)
		}
	}
	// And the family section IS there: the guard must not have silenced both.
	if !strings.Contains(body, `data-testid="family-counts"`) {
		t.Error("neither half rendered")
	}
}

// TestFamilyAndTextQueryRenderTogether: the form carries q as a hidden field,
// so a reader who narrows a text search by family keeps their words.
func TestFamilyAndTextQueryRenderTogether(t *testing.T) {
	d := familyDeps(t)
	code, body := arenaBody(t, d, "/search?q=harry&universe=harry+potter&recent=730")
	if code != http.StatusOK {
		t.Fatalf("GET combined = %d, want 200", code)
	}
	if !strings.Contains(body, `data-testid="works"`) && !strings.Contains(body, `data-testid="no-works"`) {
		t.Error("the text half did not run for a combined query")
	}
	if !strings.Contains(body, `data-testid="family-counts"`) {
		t.Error("the family half did not run for a combined query")
	}
	// The reader's words survive into the form's hidden field.
	if !strings.Contains(body, `name="q" value="harry"`) {
		t.Error("the text query was dropped from the family form")
	}
}

// TestUnknownUniverseIsNamed: a filter that cannot fire must say so. Returning
// an unfiltered list, or an empty one, is the accepted-and-ignored shape this
// project refuses (and the API refuses it too).
func TestUnknownUniverseIsNamed(t *testing.T) {
	d := familyDeps(t)
	code, body := arenaBody(t, d, "/search?universe=not-a-family")
	if code != http.StatusOK {
		t.Fatalf("GET unknown family = %d, want 200 (a page, not a crash)", code)
	}
	if !strings.Contains(body, `data-testid="family-error"`) {
		t.Fatal("an unknown family was not named")
	}
	if !strings.Contains(body, "unknown universe") {
		t.Error("the error does not say which filter was wrong")
	}
	if !strings.Contains(body, "harry potter") {
		t.Error("the error does not list the known families")
	}
	if strings.Contains(body, `data-testid="family-counts"`) {
		t.Error("a refused query still rendered counts, which read as a result")
	}
}

// TestTheBrowseRouteIsGone is the other half of a FOLD: moving a page is not
// removing it. A live /browse would keep the surface the owner objected to.
func TestTheBrowseRouteIsGone(t *testing.T) {
	d := familyDeps(t)
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/browse", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /browse = %d, want 404 (the page was folded into /search)", rec.Code)
	}
}

// TestTheNavOffersEachFamilyRouteOnce: the nav is where a stale entry survives
// a route deletion, and where a duplicate would appear if the form were ever
// re-promoted to a page.
func TestTheNavOffersNoBrowseEntry(t *testing.T) {
	d := familyDeps(t)
	code, body := arenaBody(t, d, "/search")
	if code != http.StatusOK {
		t.Fatalf("GET /search = %d", code)
	}
	if strings.Contains(body, `href="/browse"`) {
		t.Error("the nav still links to /browse")
	}
	// The family form is the replacement, and it is on this page.
	if !strings.Contains(body, `action="/search"`) {
		t.Error("the family form is not on /search")
	}
}
