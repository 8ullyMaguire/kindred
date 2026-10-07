package api

// Search-by-address, author profiles, and the tag page's taste default --
// tested through the real handler over a live SQLite corpus, for the same
// reason tagsort_test.go is: a template test can prove the block RENDERS,
// and cannot prove the query behind it found the work.

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestSearchResolvesWorkURLAndID: a pasted AO3 address (or a bare id) is an
// exact match rendered above the fuzzy list, never buried in it. The
// URL-shaped MISS must be said out loud: a reader holding a link needs to
// know the mirror lacks the work, and "no results" would hide that.
func TestSearchResolvesWorkURLAndID(t *testing.T) {
	ts := newSortServer(t)
	defer ts.Close()

	cases := []struct {
		name string
		q    string
		want string // substring the page must contain
	}{
		{"full URL", "https://archiveofourown.org/works/3", "Work 3"},
		{"URL with query junk", "http://archiveofourown.org/works/3?view_adult=true", "Work 3"},
		{"path only", "/works/3", "Work 3"},
		{"bare id", "3", "Work 3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := getPage(t, ts, "/search?q="+url.QueryEscape(c.q))
			if !strings.Contains(body, `data-testid="exact-work"`) {
				t.Fatalf("no exact-work block for query %q", c.q)
			}
			if !strings.Contains(body, c.want) {
				t.Fatalf("exact block for %q does not contain %q", c.q, c.want)
			}
		})
	}

	// A URL naming an id this mirror does not hold says so.
	body := getPage(t, ts, "/search?q="+url.QueryEscape("https://archiveofourown.org/works/999999"))
	if !strings.Contains(body, `data-testid="exact-missing"`) {
		t.Error("URL-shaped query for an unknown id did not render exact-missing")
	}
	if !strings.Contains(body, "999999") {
		t.Error("exact-missing note does not name the id")
	}

	// A bare number that matches nothing is a search term, not a broken
	// link: no missing-note for "999999" typed bare.
	body = getPage(t, ts, "/search?q=999999")
	if strings.Contains(body, `data-testid="exact-missing"`) {
		t.Error("bare number rendered exact-missing; digits alone are not a link")
	}

	// The exact match must appear exactly once on the page: the fuzzy list
	// excludes it (bare id "3" also matches the title "Work 3", which is
	// precisely where duplication would happen without the exclusion).
	body = getPage(t, ts, "/search?q=3")
	if got := strings.Count(body, ">Work 3<"); got != 1 {
		t.Errorf("Work 3 appears %d times on the page, want exactly 1 (exact block)", got)
	}
}

// TestSearchTitlePlusAuthorTokens: "word1 word2" means the work carries both
// words -- one in the title, one in the byline -- which the whole-string
// LIKE could never match. That was the reported gap: title+author search.
func TestSearchTitlePlusAuthorTokens(t *testing.T) {
	ts := newSortServer(t)
	defer ts.Close()

	// Fixture: titles are "Work N", authors are "A". "3 A" must find
	// exactly Work 3: token "3" in the title, token "A" in the byline.
	// The phrase "3 A" appears in NEITHER column, so this only passes if
	// the tokens are ANDed across columns.
	body := getPage(t, ts, "/search?q="+url.QueryEscape("3 A"))
	i := strings.Index(body, `data-testid="works"`)
	if i < 0 {
		t.Fatal("title+author query returned no work section")
	}
	works := body[i:]
	if !strings.Contains(works, ">Work 3<") {
		t.Error("did not find Work 3 from tokens '3' (title) + 'A' (author)")
	}
	for _, id := range []int{1, 2, 4, 5, 6, 7, 8} {
		if strings.Contains(works, ">Work "+strconv.Itoa(id)+"<") {
			t.Errorf("Work %d matched '3 A' though its title has no 3", id)
		}
	}
}

// TestAuthorPageAndBylineLinks: authors are pages. Every byline on every
// card links to /author?q=..., and that page lists the works.
func TestAuthorPageAndBylineLinks(t *testing.T) {
	ts := newSortServer(t)
	defer ts.Close()

	// The byline on a work card links to the author page.
	body := getPage(t, ts, "/tag/1?sort=kudos")
	if !strings.Contains(body, `href="/author?q=A"`) {
		t.Error("work card byline has no /author link")
	}

	// The author page lists matching works, most kudos first, and counts
	// the total honestly.
	body = getPage(t, ts, "/author?q=A")
	if !strings.Contains(body, `data-testid="author-count"`) {
		t.Fatal("author page has no author-count")
	}
	if !strings.Contains(body, "8") {
		t.Error("author count does not mention the fixture's 8 works")
	}
	if got, want := joinIDs(workOrder(t, body)), "1,2,3,4,5,6,7,8"; got != want {
		t.Errorf("author page order = %s, want %s", got, want)
	}

	// No match: an honest empty message, not a blank table.
	body = getPage(t, ts, "/author?q="+url.QueryEscape("zzznobody"))
	if !strings.Contains(body, `data-testid="no-author-works"`) {
		t.Error("unknown author did not render no-author-works")
	}
}

// TestTagPageTasteDefault: the tag page opens on the blend. Either the
// blend ran (taste-note, scores on cards) or it said why it could not
// (taste-error) -- never silence. An explicit sort opts out entirely.
func TestTagPageTasteDefault(t *testing.T) {
	ts := newSortServer(t)
	defer ts.Close()

	body := getPage(t, ts, "/tag/1")
	if !strings.Contains(body, "Taste match") {
		t.Error("default tag page heading is not the taste blend")
	}
	if !strings.Contains(body, `data-testid="taste-note"`) &&
		!strings.Contains(body, `data-testid="taste-error"`) {
		t.Error("blend neither reported itself nor explained its absence")
	}
	if !strings.Contains(body, `name="n"`) {
		t.Error("sort/filter form does not carry the n parameter")
	}

	// An explicit exact sort opts out of the blend: no notes, no scores.
	body = getPage(t, ts, "/tag/1?sort=kudos")
	if strings.Contains(body, `data-testid="taste-note"`) ||
		strings.Contains(body, `data-testid="taste-error"`) {
		t.Error("explicit sort still rendered blend notes")
	}
	if strings.Contains(body, `data-testid="taste-score"`) {
		t.Error("explicit sort rendered taste scores")
	}
}

// TestWorkPageNRaisesRecommendations: ?n= reaches the work page, and the
// truncation note links to the raised count instead of leaving the reader
// to guess parameters.
func TestWorkPageNRaisesRecommendations(t *testing.T) {
	ts := newSortServer(t)
	defer ts.Close()

	one := getPage(t, ts, "/work/1?n=1")
	// Whatever the engine returned, n= must not break the page, and a
	// filled list must offer the doubled count.
	if !strings.Contains(one, "Work 1") {
		t.Error("?n=1 lost the work itself")
	}
	if strings.Contains(one, `data-testid="raise-n"`) &&
		!strings.Contains(one, `href="?n=2"`) {
		t.Error("raise-n link does not ask for 2 (double n=1)")
	}
	// The default page renders identically well at n=10.
	full := getPage(t, ts, "/work/1?n=10")
	if !strings.Contains(full, "Work 1") {
		t.Error("n=10 lost the work itself")
	}
}
