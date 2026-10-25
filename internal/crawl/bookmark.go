package crawl

// Reading a reader's BOOKMARKS page.
//
// A bookmarks page is a work listing with one difference that matters: the list item is the
// BOOKMARK (`<li id="bookmark_3127754476" class="bookmark blurb group work-34456360 ...">`), and the
// work is inside it. Parsing it as a work listing finds no `id="work_…"` at all and reports an
// empty page -- which is what a "0 bookmarks" import looks like when the parser is aimed at the
// wrong element.
//
// The work id is in the class list (`work-34456360`), which is more reliable than the first
// `/works/` href on the item: when a reader bookmarks a series or a collection the item's first
// work link is not the bookmark's subject, while the class is written by AO3 from the bookmark's
// own record.
//
// Everything else inside the item is the ordinary blurb markup, so the field parsing is shared
// with ParseListing rather than re-implemented: two parsers for one markup is two places for the
// tag-category lookup to drift.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Bookmark is one item on a reader's bookmarks page.
type Bookmark struct {
	// BookmarkID is AO3's id for the bookmark itself, not for the work.
	BookmarkID int64
	// WorkID is the bookmarked work.
	WorkID   int64
	Title    string
	Authors  []string
	Fandoms  []string
	Tags     []ParsedTag
	Rating   string
	Complete bool
	// BookmarkedAt is the date AO3 shows for the bookmark ("04 Mar 2024" as displayed).
	BookmarkedAt string
	// BookmarkCount is how many readers have bookmarked this WORK ("16 Bookmarks" in the status
	// line). It is the work's popularity among bookmarkers, not this reader's judgement.
	BookmarkCount int64
}

var (
	bookmarkSplitRe = regexp.MustCompile(`(?s)<li\s+id="bookmark_(\d+)"[^>]*class="[^"]*bookmark blurb[^"]*"`)
	workClassRe     = regexp.MustCompile(`\bwork-(\d+)\b`)
	bmCountRe       = regexp.MustCompile(`(?s)<p[^>]*class="status"[^>]*title="([\d,]+) Bookmarks?"`)
	bmDateRe        = regexp.MustCompile(`(?is)<p[^>]*class="datetime"[^>]*>(.*?)</p>`)
)

// ParseBookmarks returns the bookmarks on a reader's bookmarks page.
func ParseBookmarks(body []byte) ([]Bookmark, error) {
	matches := bookmarkSplitRe.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		text := string(body)
		// A reader with no (public) bookmarks is a real answer; a page that is not a bookmarks
		// page is not. AO3 prints the list container with its landmark heading in both cases.
		if strings.Contains(text, "List of Bookmarks") || strings.Contains(text, "bookmark index") {
			return nil, nil
		}
		return nil, fmt.Errorf("crawl: not a bookmarks page")
	}
	out := make([]Bookmark, 0, len(matches))
	for i, loc := range matches {
		bookmarkID, err := strconv.ParseInt(string(body[loc[2]:loc[3]]), 10, 64)
		if err != nil {
			continue
		}
		end := len(body)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		chunk := body[loc[0]:end]
		b := Bookmark{BookmarkID: bookmarkID}
		if m := workClassRe.FindSubmatch(chunk[:min(len(chunk), 400)]); m != nil {
			b.WorkID, _ = strconv.ParseInt(string(m[1]), 10, 64)
		}
		if b.WorkID == 0 {
			// No work id in the class: skip rather than store a bookmark that points at nothing.
			continue
		}
		pb := parseBlurb(b.WorkID, chunk)
		b.Title, b.Authors, b.Fandoms, b.Tags = pb.Title, pb.Authors, pb.Fandoms, pb.Tags
		b.Rating, b.Complete = pb.Rating, pb.Complete
		if m := bmCountRe.FindSubmatch(chunk); m != nil {
			b.BookmarkCount = parseCount(string(m[1]))
		} else if m := bmDateRe.FindSubmatch(chunk); m != nil {
			b.BookmarkedAt = cleanText(string(m[1]))
		}
		if b.BookmarkedAt == "" {
			if m := bmDateRe.FindSubmatch(chunk); m != nil {
				b.BookmarkedAt = cleanText(string(m[1]))
			}
		}
		out = append(out, b)
	}
	return out, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
