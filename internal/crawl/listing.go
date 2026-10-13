package crawl

// Link and listing extraction: reading a page for what it POINTS AT.
//
// parse.go answers "what does this page say"; this file answers "where does
// this page lead". They are separate because they fail separately: a listing
// page has no works:title meta tag and a work page has no pagination, and a
// parser that tried to do both would report a work page as malformed half the
// time.
//
// Nothing here decides whether to FOLLOW a link. That is policy and lives a
// layer up (see the package doc). Extracting a frontier and choosing to walk it
// are different jobs, and this file only does the first.

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// hrefRe grabs every href on the page.
//
// Deliberately not `<a ... href=` only: AO3 also links works from
// `<link>`/`<form>`/`<option>` elements on some layouts, and a work linked from
// anywhere is still a work worth knowing about.
var hrefRe = regexp.MustCompile(`(?is)href\s*=\s*"([^"]*)"`)

// workPathRe matches the work-id segment of an AO3 work path in any of the
// forms the site uses:
//
//	/works/12345
//	/works/12345/chapters/678
//	/works/12345?show_comments=true
//
// The terminator is `\b`, not `[/?#]|$`. These regexes run over the WHOLE page,
// where a link is followed by a quote (`href="/works/12345"`), and a class that
// only accepted `/`, `?`, `#` or end-of-input matched none of them — the
// frontier came back empty on a page full of work links.
//
// The digits are required, which is what keeps /works/new (the "post a work"
// link in the site header of every page) out of the frontier.
var workPathRe = regexp.MustCompile(`/works/(\d{1,12})\b`)

// nextPageRe finds the rel="next" link AO3 puts in its pagination nav.
var nextPageRe = regexp.MustCompile(`(?is)<a[^>]*\brel\s*=\s*"next"[^>]*href\s*=\s*"([^"]*)"`)

// nextPageAltRe is the same link with the attributes the other way round.
// AO3 is consistent today; a parser that assumed one order would silently stop
// paginating the day it changed, and a BFS that silently stops paginating looks
// exactly like a BFS that reached the end of the results.
var nextPageAltRe = regexp.MustCompile(`(?is)<a[^>]*href\s*=\s*"([^"]*)"[^>]*\brel\s*=\s*"next"`)

// ExtractWorkIDs returns every distinct AO3 work id linked from body, in the
// order first seen.
//
// Deduplication is not an optimisation. A listing page links the same work from
// its title, its series line and its bookmark count; a frontier that kept all
// three would fetch the work three times, and at AO3's 30-second crawl delay a
// duplicate is not a wasted comparison but a wasted half-minute.
func ExtractWorkIDs(body []byte) []int64 {
	seen := make(map[int64]bool)
	var out []int64
	for _, m := range workPathRe.FindAllSubmatch(body, -1) {
		id, err := strconv.ParseInt(string(m[1]), 10, 64)
		if err != nil || id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ExtractHrefs returns every href on the page, resolved to an absolute URL
// against pageURL, with HTML entities decoded and the fragment dropped.
//
// Entity decoding is load-bearing: AO3 writes search links as
// `?work_search%5Bquery%5D=x&amp;page=2`, and a caller that compared or fetched
// the raw text would query the wrong thing while looking correct in a log.
func ExtractHrefs(body []byte, pageURL string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, m := range hrefRe.FindAllSubmatch(body, -1) {
		raw := html.UnescapeString(strings.TrimSpace(string(m[1])))
		abs, ok := resolveHref(base, raw)
		if !ok || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// resolveHref turns an href into an absolute URL on the same host, and reports
// whether it is one.
//
// Same-host is enforced rather than assumed: a page that links off-site is
// ordinary (AO3 links its own help wiki), and a frontier that followed those
// would spend its 30-second budget on another domain entirely. Returning false
// rather than an error because an off-site link is not a fault, it is simply
// not frontier.
//
// Two checks, and the mutation gate showed there are exactly two needed. An
// earlier version also skipped `mailto:`, `javascript:` and `data:` by prefix;
// that switch was DEAD CODE — `base.Parse("mailto:x")` yields a URL with an
// empty Host and a non-http Scheme, so both checks below already reject it, and
// deleting the switch turned no test red. It is gone rather than kept as
// decoration.
func resolveHref(base *url.URL, raw string) (string, bool) {
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", false
	}
	u, err := base.Parse(raw)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(u.Host, base.Host) {
		return "", false
	}
	// This is what rejects `ftp://<same host>/...`, which passes the host check
	// above. Without it the frontier could hold a URL the fetcher cannot use.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	u.Fragment = ""
	return u.String(), true
}

// ListingKind classifies an AO3 URL by whether it yields a LIST OF WORKS, and
// which kind of list it is. It returns "" for anything else.
//
// This is the difference between a frontier that grows usefully and one that
// wanders. From a listing page, the links on it are mostly tag links, author
// pages and the site's own chrome; only some of those lead to more works, and
// at a 30-second crawl delay a wrong guess is half a minute spent learning
// nothing. Naming the kinds also lets a caller decide which to follow — an
// operator expanding a fandom does not necessarily want every bookmarker.
//
// The kinds are the shapes AO3 actually uses, established by reading the site's
// own link structure:
//
//	/tags/<name>/works                      "tag"
//	/works?work_search[query]=...           "search"
//	/users/<name>/works                     "user-works"
//	/users/<name>/bookmarks                 "user-bookmarks"
//	/users/<name>/pseuds/<pseudo>/bookmarks "user-bookmarks"
//	/collections/<name>/works               "collection"
//	/series/<id>                            "series"
func ListingKind(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	p := strings.Trim(u.Path, "/")
	if p == "" {
		return ""
	}
	seg := strings.Split(p, "/")
	switch seg[0] {
	case "tags":
		// /tags/<name>/works. The bare /tags/<name> is the tag landing page,
		// which is also a work list (it is what a reader sees when they click a
		// tag), so it counts.
		if len(seg) >= 3 && seg[len(seg)-1] == "works" {
			return "tag"
		}
		if len(seg) == 2 {
			return "tag"
		}
		return ""
	case "works":
		// /works?query — a search. Without a query it is the whole-mirror
		// listing, which is a valid seed but says nothing about taste.
		if u.RawQuery == "" {
			return ""
		}
		return "search"
	case "users":
		last := seg[len(seg)-1]
		switch last {
		case "works":
			return "user-works"
		case "bookmarks":
			return "user-bookmarks"
		}
		return ""
	case "collections":
		if seg[len(seg)-1] == "works" {
			return "collection"
		}
		return ""
	case "series":
		if len(seg) == 2 && seg[1] != "new" {
			return "series"
		}
		return ""
	}
	return ""
}

// NextPageURL returns the absolute URL of the next page of a listing, or "" when
// this page is the last one.
//
// Both attribute orders are tried because a missed next-link does not fail
// loudly: the BFS reaches depth 1, reports success, and silently holds a
// fraction of the works it claimed to have discovered.
func NextPageURL(body []byte, pageURL string) string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	for _, re := range []*regexp.Regexp{nextPageRe, nextPageAltRe} {
		if m := re.FindSubmatch(body); m != nil {
			raw := html.UnescapeString(strings.TrimSpace(string(m[1])))
			if abs, ok := resolveHref(base, raw); ok {
				return abs
			}
		}
	}
	return ""
}
