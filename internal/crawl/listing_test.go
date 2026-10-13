package crawl

// Tests for listing.go: what a page POINTS AT, as opposed to what it says.

import (
	"reflect"
	"testing"
)

const sampleListingPage = `<!DOCTYPE html><html><head><title>Dark Fic - Works</title></head><body>
<div id="header"><a href="/works/new">Post New</a></div>
<ol class="work index group">
  <li class="work blurb group" id="work_12345">
    <h4 class="heading"><a href="/works/12345">Title One</a></h4>
    <a rel="author" href="/users/authorone/pseuds/authorone">authorone</a>
    <h5 class="fandoms"><a class="tag" href="/tags/Dark%20Fic">Dark Fic</a></h5>
    <a href="/works/12345/chapters/678">Chapter 1</a>
    <a href="/works/12345?show_comments=true">12 comments</a>
  </li>
  <li class="work blurb group" id="work_999">
    <h4 class="heading"><a href="/works/999">Title Two</a></h4>
    <a class="tag" href="/tags/Fluff">Fluff</a>
  </li>
</ol>
<ol class="pagination actions" role="navigation">
  <li><a rel="next" href="/tags/Dark%20Fic/works?page=2">Next</a></li>
</ol>
<a href="https://www.wikimedia.org/help">off-site</a>
</body></html>`

func TestExtractWorkIDsDeduplicatesAndKeepsFirstSeenOrder(t *testing.T) {
	// Work 12345 is linked four times on this page (title, chapter, comment
	// count, and the anchor id). A frontier that kept the duplicates would fetch
	// it four times, and at AO3's 30-second crawl delay that is two wasted
	// minutes, not three wasted comparisons.
	got := ExtractWorkIDs([]byte(sampleListingPage))
	want := []int64{12345, 999}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractWorkIDs = %v; want %v", got, want)
	}
}

func TestExtractWorkIDsIgnoresNonWorkPaths(t *testing.T) {
	// /works/new is in the site header of every page. The digits are required
	// precisely so it never enters the frontier.
	body := []byte(`<a href="/works/new">Post</a>
		<a href="/works">All works</a>
		<a href="/users/works">not that either</a>
		<a href="https://archiveofourown.org/works/4242">absolute</a>`)
	got := ExtractWorkIDs(body)
	if !reflect.DeepEqual(got, []int64{4242}) {
		t.Errorf("ExtractWorkIDs = %v; want [4242] (absolute form included, /works/new ignored)", got)
	}
}

func TestExtractWorkIDsOnAPageWithNone(t *testing.T) {
	if got := ExtractWorkIDs([]byte(`<html><body>nothing here</body></html>`)); len(got) != 0 {
		t.Errorf("ExtractWorkIDs = %v; want empty", got)
	}
}

func TestExtractHrefsDecodesEntitiesAndMakesAbsolute(t *testing.T) {
	// AO3 writes search links with escaped brackets and an &amp;. A caller that
	// used the raw text would request a different query while the log looked
	// right.
	body := []byte(`<a href="/works?work_search%5Bquery%5D=dragons&amp;page=2">more</a>`)
	got := ExtractHrefs(body, "https://archiveofourown.org/tags/Dark%20Fic/works")
	want := "https://archiveofourown.org/works?work_search%5Bquery%5D=dragons&page=2"
	if len(got) != 1 || got[0] != want {
		t.Errorf("ExtractHrefs = %v; want [%s]", got, want)
	}
}

func TestExtractHrefsRefusesOffSiteAndNonHTTP(t *testing.T) {
	// A frontier that followed off-site links would spend the crawl budget on
	// another domain. These are not faults — they are simply not frontier — so
	// they are dropped rather than reported.
	//
	// The ftp case is same-host, so only the scheme check rejects it; without
	// that case the check is unreachable and deleting it turns nothing red.
	body := []byte(`
		<a href="https://evil.example.com/works/12345">absolute other host</a>
		<a href="//evil.example.com/works/12345">protocol-relative</a>
		<a href="mailto:someone@example.com">mail</a>
		<a href="javascript:void(0)">js</a>
		<a href="ftp://archiveofourown.org/works/12345">same host, wrong scheme</a>
		<a href="#top">fragment only</a>
		<a href="">empty</a>
		<a href="/works/1">fine</a>`)
	got := ExtractHrefs(body, "https://archiveofourown.org/")
	if len(got) != 1 || got[0] != "https://archiveofourown.org/works/1" {
		t.Errorf("ExtractHrefs = %v; want only the same-host http link", got)
	}
}

func TestExtractHrefsDropsTheFragment(t *testing.T) {
	// Two hrefs that differ only by fragment are the same page. Keeping both
	// would put the same URL in the frontier twice.
	body := []byte(`<a href="/works/1#comments">c</a><a href="/works/1">w</a>`)
	got := ExtractHrefs(body, "https://archiveofourown.org/")
	if len(got) != 1 {
		t.Errorf("ExtractHrefs = %v; want one entry", got)
	}
}

func TestNextPageURLFindsTheNextLink(t *testing.T) {
	base := "https://archiveofourown.org/tags/Dark%20Fic/works"
	got := NextPageURL([]byte(sampleListingPage), base)
	want := "https://archiveofourown.org/tags/Dark%20Fic/works?page=2"
	if got != want {
		t.Errorf("NextPageURL = %q; want %q", got, want)
	}
}

func TestNextPageURLHandlesEitherAttributeOrder(t *testing.T) {
	// AO3 is consistent today. A parser that assumed one order would stop
	// paginating silently the day that changed, and a BFS that stops paginating
	// looks exactly like a BFS that reached the end of the results — it reports
	// success with a fraction of the works.
	base := "https://archiveofourown.org/works"
	for _, body := range []string{
		`<a rel="next" href="/works?page=2">Next</a>`,
		`<a href="/works?page=2" rel="next">Next</a>`,
		`<a class="next" href="/works?page=2" rel="next" title="Next">Next</a>`,
	} {
		if got, want := NextPageURL([]byte(body), base), "https://archiveofourown.org/works?page=2"; got != want {
			t.Errorf("NextPageURL(%q) = %q; want %q", body, got, want)
		}
	}
}

func TestNextPageURLEmptyOnTheLastPage(t *testing.T) {
	// The absence of rel="next" is how AO3 says "this is the end". Returning
	// something here would walk off the end forever.
	for _, body := range []string{
		`<ol class="pagination"><li class="current">1</li><li>2</li></ol>`,
		`<a rel="prev" href="/works?page=0">Previous</a>`,
		``,
	} {
		if got := NextPageURL([]byte(body), "https://archiveofourown.org/works"); got != "" {
			t.Errorf("NextPageURL(%q) = %q; want empty", body, got)
		}
	}
}

func TestNextPageURLRefusesAnOffSiteNextLink(t *testing.T) {
	// Same rule as ExtractHrefs: rel="next" pointing off-site is not frontier.
	body := []byte(`<a rel="next" href="https://evil.example.com/works?page=2">Next</a>`)
	if got := NextPageURL(body, "https://archiveofourown.org/works"); got != "" {
		t.Errorf("NextPageURL = %q; want empty for an off-site next link", got)
	}
}
