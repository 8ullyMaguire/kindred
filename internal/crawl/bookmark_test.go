package crawl

// The fixture is three REAL bookmark items from a public AO3 reader's page (fetched 2026-10-25).
//
// What it has to prove, and why each assertion is the one that can fail:
//   - the WORK id comes out of the class list, not from the first /works/ link (a bookmark item
//     contains several /works/ links: the work, its bookmarkers, its series);
//   - the item is a bookmark, not a work blurb -- a parser looking for id="work_" finds nothing and
//     reports an empty page, which is exactly what a broken import looks like;
//   - tag categories survive, because the taste map this feeds is built from them.

import (
	"os"
	"testing"
)

func TestParseBookmarksReadsEveryItem(t *testing.T) {
	body, err := os.ReadFile("testdata/ao3_bookmarks.html")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	got, err := ParseBookmarks(body)
	if err != nil {
		t.Fatalf("ParseBookmarks: %v", err)
	}
	// The fixture's third item is a SERIES bookmark (`class="... series-1545 ..."`, no work id).
	// Two works, not three: a series bookmark points at a group of works and expanding it is a
	// different request. It is skipped ON PURPOSE and pinned here, because "3 items, 2 imported"
	// must not look like a parser that lost one.
	if len(got) != 2 {
		t.Fatalf("%d work bookmarks, want 2 of the 3 items (the third is a series bookmark)", len(got))
	}
	for _, b := range got {
		if b.BookmarkID == 0 || b.WorkID == 0 {
			t.Fatalf("item without ids: %+v", b)
		}
		if b.Title == "" {
			t.Fatalf("bookmark %d has no title", b.BookmarkID)
		}
		if len(b.Fandoms) == 0 {
			t.Fatalf("bookmark %d names no fandom", b.BookmarkID)
		}
		if len(b.Tags) == 0 {
			t.Fatalf("bookmark %d carries no tags: the taste map would learn nothing", b.BookmarkID)
		}
	}
	if got[0].WorkID == got[1].WorkID {
		t.Fatalf("two bookmarks resolved to the same work (%d): the id is being read off the page, "+
			"not off the item", got[0].WorkID)
	}
}

func TestParseBookmarksRefusesANonBookmarksPage(t *testing.T) {
	// A work listing has blurbs and no bookmark items: importing it as bookmarks would attribute
	// another reader's list to this one.
	if _, err := ParseBookmarks([]byte(`<ol class="work index group"><li id="work_1" class="work blurb group"></li></ol>`)); err == nil {
		t.Fatal("a work listing was accepted as a bookmarks page")
	}
	// An empty bookmarks page is a real answer, not an error.
	empty := []byte(`<div id="main"><h3 class="landmark heading">List of Bookmarks</h3><ol class="bookmark index group"></ol></div>`)
	got, err := ParseBookmarks(empty)
	if err != nil {
		t.Fatalf("an empty bookmarks page errored: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("%d bookmarks from an empty page", len(got))
	}
}
