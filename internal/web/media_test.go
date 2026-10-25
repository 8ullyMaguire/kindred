package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/harvest"
)

func TestResolveMediaAcceptsAUniqueFirstWord(t *testing.T) {
	cat := &harvest.Catalogue{
		Media: []string{"Books *a* Literature", "Other Media", "Movies"},
		MediaPaths: map[string]string{
			"Books *a* Literature": "Books%20*a*%20Literature",
			"Other Media":          "Other%20Media",
			"Movies":               "Movies",
		},
	}
	for _, want := range []string{"Books", "books", "Books & Literature", "Books%20*a*%20Literature", "Movies"} {
		if _, ok := resolveMedia(cat, want); !ok {
			t.Errorf("resolveMedia(%q) = no match, want a media", want)
		}
	}
	// A segment that could mean two media is refused rather than guessed: "M" is not a URL AO3
	// publishes, and picking the first match would silently serve the wrong index.
	if got, ok := resolveMedia(cat, "M"); ok {
		t.Errorf("resolveMedia(%q) = %q, want no match (ambiguous)", "M", got)
	}
}

// TestMediaRoutes pins the three shapes /media accepts and the two it refuses. The distinction
// matters because a prefix route that answers 200 for every tail makes a typo look like a page:
// this is the test that would have caught /media/Nope rendering the index.
func TestMediaRoutes(t *testing.T) {
	cat := &harvest.Catalogue{
		Media:      []string{"Books *a* Literature", "Movies"},
		MediaPaths: map[string]string{"Books *a* Literature": "Books%20*a*%20Literature", "Movies": "Movies"},
		Fandoms: []harvest.Fandom{
			{Name: "Harry Potter - J. K. Rowling", Path: "Harry-Potter", Media: "Books *a* Literature", Works: 340000},
			{Name: "Inception", Path: "Inception", Media: "Movies", Works: 12000},
		},
	}
	d := Deps{Catalogue: cat}
	cases := []struct {
		path string
		want int
		loc  string
		has  string
	}{
		{path: "/media/", want: http.StatusOK, has: "Books &amp; Literature"},
		{path: "/media/Books", want: http.StatusMovedPermanently, loc: "/media/Books%20*a*%20Literature/fandoms"},
		{path: "/media/Books%20*a*%20Literature/fandoms", want: http.StatusOK, has: "Harry Potter"},
		{path: "/media/Movies/fandoms", want: http.StatusOK, has: "Inception"},
		{path: "/media/Nope/fandoms", want: http.StatusNotFound},
		{path: "/media/Nope", want: http.StatusNotFound},
		{path: "/media/Movies/works", want: http.StatusNotFound},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		d.route(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s = %d, want %d", tc.path, rec.Code, tc.want)
			continue
		}
		if tc.loc != "" && rec.Header().Get("Location") != tc.loc {
			t.Errorf("%s redirects to %q, want %q", tc.path, rec.Header().Get("Location"), tc.loc)
		}
		if tc.has != "" && !strings.Contains(rec.Body.String(), tc.has) {
			t.Errorf("%s body does not contain %q", tc.path, tc.has)
		}
	}
}
