package web

import (
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
