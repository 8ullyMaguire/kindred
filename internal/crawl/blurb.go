package crawl

// Reading a LISTING page: the blurbs, with their numbers.
//
// A listing page carries everything this project needs to CHOOSE a work before fetching it:
// title, authors, fandoms, typed tags, rating, completion, words, kudos, bookmarks, hits,
// comments. That matters because a work page costs one request and a listing page carries twenty
// works, so a harvest that chooses from blurbs spends 1/20th the requests to look at the same
// candidates -- and the choice is the part that needs breadth.
//
// The shapes below were read off live pages (2026-10-25) rather than guessed:
//
//	<li id="work_5096105" class="work blurb group work-5096105 user-1587650" role="article">
//	  <h4 class="heading"><a href="/works/5096105">In Another Life</a> by
//	    <a rel="author" href="/users/LittleLuxray/pseuds/LittleLuxray">LittleLuxray</a></h4>
//	  <h5 class="fandoms heading"><span class="landmark">Fandoms:</span>
//	    <a class="tag" href="/tags/Haikyuu!!/works">Haikyuu!!</a></h5>
//	  <ul class="required-tags"><li>…<span class="rating-teen rating" title="Teen And Up Audiences">…
//	  <p class="datetime">28 Oct 2015</p>
//	  <ul class="tags commas">
//	    <li class='warnings'><strong><a class="tag" href="/tags/Major%20Character%20Death/works">…
//	    <li class='relationships'><a class="tag" href="/tags/Akaashi%20Keiji*s*Bokuto…">…
//	  <dl class="stats"><dd class="words">1,234</dd><dd class="kudos">567</dd>…
//
// The tag TYPE is the `class` of the `<li>`, which is the site telling us directly. It is read
// here and carried, because the mirror's own work_tags column is 'freeforms' for essentially
// every pre-existing row (a sibling tool flattened it) and this is the only authoritative source.

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// Blurb is one work as a listing page describes it.
type Blurb struct {
	ID      int64
	Title   string
	Authors []string
	Fandoms []string
	// Tags are the work's tags with AO3's own type ('warnings', 'relationships',
	// 'characters', 'freeforms'). Fandoms are also in Tags, typed 'fandoms'.
	Tags     []ParsedTag
	Rating   string
	Complete bool
	Words    int64
	Kudos    int64
	// Bookmarks and Hits are pointers for the same reason ParsedWork's bookmarks is: AO3 omits a
	// count it has nothing to say about, and "no bookmarks shown" is not "zero bookmarks".
	Bookmarks *int64
	Hits      *int64
	Comments  int64
	Updated   string
}

var (
	blurbSplitRe = regexp.MustCompile(`(?s)<li\s+id="work_(\d+)"[^>]*class="[^"]*work blurb[^"]*"`)
	blurbTitleRe = regexp.MustCompile(`(?is)<h4[^>]*class="[^"]*heading[^"]*"[^>]*>\s*<a[^>]*href="/works/\d+"[^>]*>(.*?)</a>`)
	blurbAuthRe  = regexp.MustCompile(`(?is)<a\s+rel="author"[^>]*>(.*?)</a>`)
	blurbFandRe  = regexp.MustCompile(`(?is)<h5[^>]*class="[^"]*fandoms[^"]*"[^>]*>(.*?)</h5>`)
	tagAnchorRe  = regexp.MustCompile(`(?is)<a[^>]*class="tag"[^>]*>(.*?)</a>`)
	ratingRe     = regexp.MustCompile(`(?is)<span[^>]*class="rating-[a-z]+ rating"[^>]*title="([^"]+)"`)
	datetimeRe   = regexp.MustCompile(`(?is)<p[^>]*class="datetime"[^>]*>(.*?)</p>`)
	tagLIRe      = regexp.MustCompile(`(?is)<li[^>]*class=['"]?([a-z]+)['"]?[^>]*>(.*?)</li>`)
	completeRe   = regexp.MustCompile(`complete-yes`)
	ddStatRe     = regexp.MustCompile(`(?is)<dd[^>]*class="([a-z]+)"[^>]*>(.*?)</dd>`)
)

// tagTypeStored resolves a tag category from a page, accepting BOTH spellings.
//
// The work page writes the category SINGULAR ("fandom tags", "freeform tags") and the listing
// page writes it PLURAL as the <li> class ("fandoms", "freeforms"). tagTypePlural is keyed by
// the work page's singular, so a blurb lookup on it missed every category and reported fandoms
// alone -- which is exactly the failure a test that only checked "some tags parsed" would miss,
// since fandoms are collected by a different code path.
func tagTypeStored(class string) (string, bool) {
	if v, ok := tagTypePlural[class]; ok {
		return v, true
	}
	if v, ok := tagTypePlural[strings.TrimSuffix(class, "s")]; ok {
		return v, true
	}
	return "", false
}

// ParseListing returns the blurbs on a listing page.
//
// An empty slice with no error is a legitimate answer (a page of zero results); an error means
// the body does not look like a listing at all. The distinction matters because a harvest that
// read a Cloudflare page as "no results" would quietly shrink its own quota.
func ParseListing(body []byte) ([]Blurb, error) {
	idxs := blurbSplitRe.FindAllSubmatchIndex(body, -1)
	if len(idxs) == 0 {
		// A listing always has the works list container; its absence means this is not one.
		if !strings.Contains(string(body), `class="work index group"`) &&
			!strings.Contains(string(body), `id="main"`) {
			return nil, fmt.Errorf("crawl: not a listing page (no work blurbs, no main section)")
		}
		return nil, nil
	}
	out := make([]Blurb, 0, len(idxs))
	for i, loc := range idxs {
		id, err := strconv.ParseInt(string(body[loc[2]:loc[3]]), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		end := len(body)
		if i+1 < len(idxs) {
			end = idxs[i+1][0]
		}
		chunk := body[loc[0]:end]
		out = append(out, parseBlurb(id, chunk))
	}
	return out, nil
}

func parseBlurb(id int64, chunk []byte) Blurb {
	b := Blurb{ID: id}
	if m := blurbTitleRe.FindSubmatch(chunk); m != nil {
		b.Title = cleanText(string(m[1]))
	}
	for _, m := range blurbAuthRe.FindAllSubmatch(chunk, -1) {
		if name := cleanText(string(m[1])); name != "" {
			b.Authors = append(b.Authors, name)
		}
	}
	if m := blurbFandRe.FindSubmatch(chunk); m != nil {
		for _, a := range tagAnchorRe.FindAllSubmatch(m[1], -1) {
			if name := cleanText(string(a[1])); name != "" {
				b.Fandoms = append(b.Fandoms, name)
				b.Tags = append(b.Tags, ParsedTag{Name: name, Type: "fandoms"})
			}
		}
	}
	// The typed tags are `<li class='TYPE'>` inside the tags list. The scan runs over the whole
	// blurb rather than over one <ul>: the required-tags block is ALSO a <ul> whose class contains
	// "tags", so anchoring on a tags-looking ul matched THAT one first and every real category was
	// skipped (the first version of this parser did exactly that and reported fandoms only).
	for _, li := range tagLIRe.FindAllSubmatch(chunk, -1) {
		typ := strings.ToLower(string(li[1]))
		stored, known := tagTypeStored(typ)
		if !known {
			// A class that is not a tag category (a helper list, say). Skip rather than
			// invent a type: the mirror's column must mean what AO3 means.
			continue
		}
		for _, a := range tagAnchorRe.FindAllSubmatch(li[2], -1) {
			if name := cleanText(string(a[1])); name != "" {
				b.Tags = append(b.Tags, ParsedTag{Name: name, Type: stored})
			}
		}
	}
	if m := ratingRe.FindSubmatch(chunk); m != nil {
		b.Rating = html.UnescapeString(string(m[1]))
	}
	b.Complete = completeRe.Match(chunk)
	if m := datetimeRe.FindSubmatch(chunk); m != nil {
		b.Updated = cleanText(string(m[1]))
	}
	for _, m := range ddStatRe.FindAllSubmatch(chunk, -1) {
		class := strings.ToLower(string(m[1]))
		n := parseCount(cleanText(string(m[2])))
		switch class {
		case "words":
			b.Words = n
		case "kudos":
			b.Kudos = n
		case "comments":
			b.Comments = n
		case "bookmarks":
			v := n
			b.Bookmarks = &v
		case "hits":
			v := n
			b.Hits = &v
		}
	}
	return b
}
