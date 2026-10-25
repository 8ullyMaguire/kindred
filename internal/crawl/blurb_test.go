package crawl

// The fixture is THREE REAL BLURBS copied out of a live listing page (Haikyuu!!, sorted by
// kudos, fetched 2026-10-25). A synthetic fixture would test the regexes against the shapes I
// imagined; the previous parser in this package failed on every real AO3 page while its invented
// fixture stayed green, and this is the same trap one level down.
//
// The assertions that matter are the ones a plausible-but-wrong parser passes:
//   - the tag TYPE must come from the <li class>, not be assumed 'freeforms' (the mirror's
//     pre-existing rows are flattened, so a parser that copies that bug looks correct);
//   - a missing count must stay nil, not become zero;
//   - a page that is not a listing must ERROR, because "no blurbs" from a Cloudflare page would
//     silently shrink a harvest's quota.

import (
	"os"
	"strings"
	"testing"
)

func listingFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/ao3_listing.html")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return body
}

func TestParseListingReadsEveryBlurb(t *testing.T) {
	got, err := ParseListing(listingFixture(t))
	if err != nil {
		t.Fatalf("ParseListing: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("%d blurbs, want 3", len(got))
	}
	first := got[0]
	if first.ID == 0 || first.Title == "" {
		t.Fatalf("first blurb has no identity: %+v", first)
	}
	if len(first.Authors) == 0 || first.Authors[0] == "" {
		t.Fatalf("first blurb has no author: %+v", first.Authors)
	}
	if len(first.Fandoms) == 0 {
		t.Fatalf("first blurb names no fandom: %+v", first)
	}
	if first.Kudos <= 0 {
		t.Fatalf("kudos = %d on a list sorted by kudos", first.Kudos)
	}
	if first.Words <= 0 {
		t.Fatalf("words = %d", first.Words)
	}
	if first.Rating == "" {
		t.Fatal("no rating: the required-tags block was not read")
	}
}

// The tag type is the point of parsing blurbs at all: it is the site's own classification, and
// the mirror's is flattened.
func TestParseListingKeepsAOTagTypes(t *testing.T) {
	got, err := ParseListing(listingFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, b := range got {
		for _, tg := range b.Tags {
			types[tg.Type]++
			if tg.Type == "" {
				t.Fatalf("tag %q has no type", tg.Name)
			}
		}
	}
	for _, want := range []string{"fandoms", "warnings", "relationships", "freeforms"} {
		if types[want] == 0 {
			t.Errorf("no %q tags parsed at all (types seen: %v)", want, types)
		}
	}
	if types["freeforms"] > 0 && types["fandoms"] == 0 {
		t.Fatal("every tag was typed freeforms: the class map is not being read")
	}
}

// A count AO3 does not print is UNKNOWN, not zero. A zero would make a work with no bookmarks
// look like one whose bookmarks are hidden, and the selection score reads these numbers.
func TestParseListingKeepsMissingCountsNil(t *testing.T) {
	// Strip every bookmarks dd: the shape AO3 sends for a work nobody has bookmarked.
	body := strings.ReplaceAll(string(listingFixture(t)), `class="bookmarks"`, `class="nobookmarks"`)
	got, err := ParseListing([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got {
		if b.Bookmarks != nil {
			t.Fatalf("work %d has bookmarks=%d after the dd was removed", b.ID, *b.Bookmarks)
		}
	}
}

func TestParseListingRefusesANonListingPage(t *testing.T) {
	if _, err := ParseListing([]byte(`<html><body><h1>525 SSL handshake failed</h1></body></html>`)); err == nil {
		t.Fatal("a Cloudflare error page was accepted as a listing")
	}
	// And an empty result set is NOT an error: a fandom with no works is a real answer.
	page := []byte(`<div id="main" class="container"><ol class="work index group"></ol></div>`)
	got, err := ParseListing(page)
	if err != nil {
		t.Fatalf("an empty listing errored: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("%d blurbs from an empty listing", len(got))
	}
}
