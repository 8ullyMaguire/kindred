package web

// #66: a work page names its author and links the byline, and the
// author-similarity endpoint gets a way in from a work.
//
// The row's own words were "the work page does not link to it and no template
// renders it", so the test is about reachability, not about a score: an author
// similarity nobody can arrive at is a JSON endpoint with no reader.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTheWorkPageNamesItsAuthorAndLinksTheByline(t *testing.T) {
	d, _ := collectionDeps(t)
	rr := httptest.NewRecorder()
	d.renderWork(rr, httptest.NewRequest(http.MethodGet, "/works/1", nil), "1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, headOf(rr.Body.String(), 400))
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-testid="work-byline"`) {
		t.Fatalf("the work page does not name its author at all:\n%s", headOf(body, 900))
	}
	// The byline must be a LINK to the shelf view, not a string: that link is
	// the only way /author?q= is reachable from a work.
	if !strings.Contains(body, `rel="author"`) || !strings.Contains(body, "/author?q=author") {
		t.Errorf("the byline is not linked to the author's shelf:\n%s", headOf(body, 900))
	}
}

func TestTheWorkPageShowsAuthorsWhenTheEngineFindsThem(t *testing.T) {
	ctx := context.Background()
	d, _ := collectionDeps(t)

	// What the engine answers is the fact the page must reflect. Asking the
	// engine directly keeps the test honest in both directions: a fixture where
	// no other author shares tags must not make the test assert a panel that
	// cannot exist.
	want, err := d.Engine.SimilarAuthors(ctx, "author1", 8)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		t.Fatalf("engine: %v", err)
	}

	rr := httptest.NewRecorder()
	d.renderWork(rr, httptest.NewRequest(http.MethodGet, "/works/1", nil), "1")
	body := rr.Body.String()

	if len(want) == 0 {
		if strings.Contains(body, `data-testid="work-authors"`) {
			t.Fatalf("the engine found no similar authors but the page rendered a list")
		}
		t.Log("fixture has no similar author for author1; the byline half is asserted by the other test")
		return
	}
	if !strings.Contains(body, `data-testid="work-authors"`) {
		t.Fatalf("the engine names %d similar authors and the page shows none of them:\n%s",
			len(want), headOf(body, 1200))
	}
	for _, a := range want {
		if !strings.Contains(body, "/author?q="+url.QueryEscape(a.Name)) {
			t.Errorf("similar author %q is not linked from the work page", a.Name)
		}
	}
}
