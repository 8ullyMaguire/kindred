package api

// Feeds (#61) and the search descriptor (#62), tested on what a reader's client
// actually requires: well-formed XML, a content type that makes it a feed, an
// absolute guid, chronological order, and an honest answer for a URL that names
// nothing.
//
// The order test is the one with teeth. The mirror stores update_date in AO3's
// "31 Oct 2025" form, so a plain `update_date DESC` sorts by DAY OF MONTH and
// never compares the month: measured on the live corpus, the naive order called
// 31 Oct 2025 the most recent work when the true one was 26 Jun 2026. A fixture
// of ISO dates would have passed against both. So the fixture is deliberately
// mixed and deliberately adversarial.

import (
	"database/sql"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// feedDoc is the subset of RSS the assertions read.
type feedDoc struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Channel struct {
		Title         string `xml:"title"`
		Link          string `xml:"link"`
		LastBuildDate string `xml:"lastBuildDate"`
		Items         []struct {
			Title string `xml:"title"`
			Link  string `xml:"link"`
			GUID  struct {
				Value       string `xml:",chardata"`
				IsPermaLink string `xml:"isPermaLink,attr"`
			} `xml:"guid"`
			PubDate     string `xml:"pubDate"`
			Description string `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

func getFeed(t *testing.T, url string) (*feedDoc, string, int) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw := readBody(t, res.Body)
	if res.StatusCode != http.StatusOK {
		return nil, raw, res.StatusCode
	}
	var doc feedDoc
	if err := xml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("the feed is not well-formed XML: %v\n%s", err, raw)
	}
	return &doc, raw, res.StatusCode
}

// workURL is the corpus url for a fixture work, empty for the one that tests
// the minted-link fallback.
func workURL(id int) string {
	if id == 15 {
		return ""
	}
	return "https://archiveofourown.org/works/" + strconv.Itoa(id)
}

// newFeedServer builds a server whose corpus is seeded BEFORE the store opens
// it. That ordering is not a detail: store.Open attaches the mirror READ-ONLY
// (as production does), so a fixture written afterwards fails with "attempt to
// write a readonly database" — which reads as a broken test rather than a
// mis-ordered one.
//
// The dates are deliberately mixed: AO3's "31 Oct 2025" form and ISO, so a
// string sort and a chronological sort disagree. A fixture of ISO dates alone
// validates whichever order it was written for.
func newFeedServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	corpusPath := filepath.Join(dir, "corpus.db")

	{
		f, err := sql.Open("sqlite", corpusPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Exec(corpusSchema); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO tags(id,name) VALUES(1,'coffee shop')`); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id      int
		title   string
		updated string
	}{
		// The hostile one: a title with an ampersand, an angle bracket and a
		// quote. Bypass the encoder with string concatenation and the document
		// stops being well-formed, so the parse assertion is the escaping test.
		{11, `Tea & "Sympathy" <3`, "31 Oct 2025"},
		{12, "Older but later day", "12 Jan 2026"},
		{13, "The actual newest", "26 Jun 2026"},
		{14, "An ISO date row", "2026-07-01"},
		// No url column: the mirror has works like this, and the feed then has
		// to MINT one. Without a row like it, the fallback is untested and a
		// relative link in it survives every assertion (measured: the mutation
		// to "/work/%d" survived until this row existed).
		{15, "No url in the mirror", "01 Jan 2020"},
	}
	for _, r := range rows {
		if _, err := seed.Exec(
			`INSERT INTO works(id,url,title,authors,word_count,kudos,rating,language,complete,update_date,first_seen)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			r.id, workURL(r.id), r.title, "authorA",
			1200, 34, "General Audiences", "English", 1, r.updated, "2026-01-01"); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Exec(
			`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,?,?)`,
			r.id, 1, "freeforms"); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.Context(), dbPath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	srv := &Server{
		Engine: &engine.Engine{
			Store: st, Corpus: corpus.NewAO3(st.Corpus),
			PoolSize: 50, TopN: 4, Lite: true,
		},
		Store:   st,
		Version: "test",
		Lite:    true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func TestFeedTagIsAWellFormedFeedInChronologicalOrder(t *testing.T) {
	ts := newFeedServer(t)

	doc, raw, code := getFeed(t, ts.URL+"/feed/tag/1")
	if code != http.StatusOK {
		t.Fatalf("GET /feed/tag/1 -> %d\n%s", code, raw)
	}
	if doc.Version != "2.0" {
		t.Fatalf("rss version = %q, want 2.0", doc.Version)
	}
	if n := len(doc.Channel.Items); n < 5 {
		t.Fatalf("the feed carries %d items; an order assertion needs all four, "+
			"or it is vacuous", n)
	}
	// 2026-07-01, 26 Jun 2026, 12 Jan 2026, 31 Oct 2025.
	want := []string{"An ISO date row", "The actual newest", "Older but later day",
		`Tea & "Sympathy" <3`, "No url in the mirror"}
	for i, w := range want {
		if doc.Channel.Items[i].Title != w {
			t.Fatalf("item %d is %q, want %q: the feed is not in chronological order "+
				"(a string sort puts 31 Oct 2025 first)", i, doc.Channel.Items[i].Title, w)
		}
	}
	// Dates are RFC1123Z, because RSS requires it and a client will not parse
	// a bare "31 Oct 2025".
	for _, it := range doc.Channel.Items {
		if it.PubDate == "" {
			continue
		}
		if _, err := time.Parse(time.RFC1123Z, it.PubDate); err != nil {
			t.Fatalf("pubDate %q is not RFC1123Z: %v", it.PubDate, err)
		}
	}
}

func TestFeedTagItemsCarryAbsoluteResolvableLinks(t *testing.T) {
	ts := newFeedServer(t)
	doc, raw, _ := getFeed(t, ts.URL+"/feed/tag/1")
	if len(doc.Channel.Items) == 0 {
		t.Fatalf("no items; link assertions would be vacuous\n%s", raw)
	}
	// Every item, not just the first: the first has a url from the corpus and
	// the last has none, so the fallback that MINTS a link is only reachable by
	// walking them all.
	minted := 0
	for _, it := range doc.Channel.Items {
		if !strings.HasPrefix(it.Link, "http://") && !strings.HasPrefix(it.Link, "https://") {
			t.Fatalf("item link %q is not absolute: RSS clients mint their own identity "+
				"for a guid they cannot resolve", it.Link)
		}
		if it.GUID.Value != it.Link {
			t.Fatalf("guid %q differs from link %q", it.GUID.Value, it.Link)
		}
		if it.GUID.IsPermaLink != "true" {
			t.Fatalf("guid isPermaLink = %q, want true", it.GUID.IsPermaLink)
		}
		if strings.HasSuffix(it.Link, "/work/15") {
			minted++
		}
	}
	if minted != 1 {
		t.Fatalf("the work with no url in the mirror produced %d minted links, want 1: "+
			"the fallback is what a corpus row without a url depends on", minted)
	}
}

func TestFeedTagUnknownTagIs404NotAnEmptyFeed(t *testing.T) {
	ts := newTestServer(t)
	_, raw, code := getFeed(t, ts.URL+"/feed/tag/999999")
	if code != http.StatusNotFound {
		t.Fatalf("an unknown tag -> %d, want 404. An empty 200 reads as \"nothing "+
			"new\", which is a claim about the corpus rather than about the URL", code)
	}
	if strings.Contains(raw, "<rss") {
		t.Fatal("a 404 rendered an RSS document")
	}
}

func TestFeedTagContentTypeAndXmlSuffix(t *testing.T) {
	ts := newFeedServer(t)
	res, err := http.Get(ts.URL + "/feed/tag/1.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf(".xml suffix -> %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Fatalf("Content-Type = %q, want application/rss+xml: served as text/html a "+
			"reader renders it as a wall of text", ct)
	}
}

func TestFeedTagRespectsTheItemBound(t *testing.T) {
	ts := newFeedServer(t)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?n=2", 2},
		{"?n=0", 1},
		{"?n=99999", 5}, // capped: only five works exist
	} {
		doc, raw, _ := getFeed(t, ts.URL+"/feed/tag/1"+tc.query)
		if len(doc.Channel.Items) != tc.want {
			t.Fatalf("%s -> %d items, want %d\n%s", tc.query, len(doc.Channel.Items), tc.want, raw)
		}
	}
}

func TestFeedSeedRanksFromTheSeed(t *testing.T) {
	ts := newFeedServer(t)
	doc, raw, code := getFeed(t, ts.URL+"/feed/seed/ao3_work/11")
	if code != http.StatusOK {
		t.Fatalf("seed feed -> %d\n%s", code, raw)
	}
	if len(doc.Channel.Items) == 0 {
		t.Fatalf("the seed feed is empty for a work with tag siblings\n%s", raw)
	}
	// The seed itself is never in its own recommendations.
	for _, it := range doc.Channel.Items {
		if strings.HasSuffix(it.Link, "/work/11") {
			t.Fatalf("the seed work appears in its own feed: %q", it.Link)
		}
	}
	if !strings.Contains(doc.Channel.Title, "Tea &") {
		t.Fatalf("channel title %q does not name the seed", doc.Channel.Title)
	}
}

func TestFeedSeedUnknownSeedIs404(t *testing.T) {
	ts := newTestServer(t)
	_, _, code := getFeed(t, ts.URL+"/feed/seed/ao3_work/999999")
	if code != http.StatusNotFound {
		t.Fatalf("unknown seed -> %d, want 404", code)
	}
	_, _, malformed := getFeed(t, ts.URL+"/feed/seed/ao3_work")
	if malformed != http.StatusNotFound {
		t.Fatalf("a seed feed with no id -> %d, want 404 with the shape named", malformed)
	}
}

func TestOpenSearchDescriptorPointsAtTheSearchPage(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/opensearch.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/opensearch.xml -> %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "opensearchdescription+xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	raw := readBody(t, res.Body)
	for _, want := range []string{
		"<ShortName>kindred</ShortName>",
		"http://a9.com/-/spec/opensearch/1.1/",
		"/search?q={searchTerms}",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("descriptor is missing %q", want)
		}
	}
	// A browser refuses a descriptor that is not well-formed.
	var v struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("descriptor is not well-formed XML: %v\n%s", err, raw)
	}
}

// readBody drains a response body, failing the test rather than returning a
// truncated document that would make a well-formedness assertion lie.
func readBody(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
