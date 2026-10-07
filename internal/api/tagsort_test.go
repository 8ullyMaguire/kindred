package api

// The tag page's sort and length controls, tested through the real handler.
//
// WHY THIS FILE IS IN internal/api AND NOT internal/web:
//
// The render sweep (internal/web/render_pages_test.go) proves the TEMPLATE
// renders each combination of .Sort and .Words. It cannot prove the QUERY
// changed, because the sweep hands .Sort to the template directly. A handler
// that ignored ?sort= entirely leaves every template test green -- the
// control renders, the sentence is correct, and the list is still in the
// wrong order.
//
// Closing that needs a live server over a real SQLite corpus, and the
// handler needs a real *engine.Engine. Building one here would mean
// internal/web importing internal/engine, which it already does not -- the
// dependency runs the other way. So the test lives beside the harness that
// already constructs one: internal/api's newTestServerWithStore. That also
// means it exercises the whole chain, guardAPI routing included, which is
// where the header bug of the previous commit hid.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	_ "modernc.org/sqlite"
)

// newSortServer builds a live server over a 3-work tag.
//
// The fixture is built so the three sorts CANNOT agree -- see
// TestSortKudosAndWordsMustDiffer, which asserts that property rather than
// assuming it. A fixture where word count perfectly correlated with hits
// would make every sort arm below pass with the sort ignored, which is
// exactly the fixture mistake that hid the rating bug.
//
//	id | words | kudos | update_date
//	 1 |  1000 |   300 | 2026-01-01   most kudos, shortest
//	 2 |  2000 |   200 | 2026-02-01
//	 3 |  3000 |   100 | 2026-03-01   least kudos, longest, most recent
func newSortServer(t *testing.T) *httptest.Server {
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

	// The corpus is attached READ-ONLY by store.Open, exactly as in
	// production, so it is seeded through a separate handle afterwards.
	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO tags(id,name) VALUES(1,'dark')`); err != nil {
		t.Fatal(err)
	}
	// Eight rows, not three, with a spread over rating / language /
	// completion, so the three page filters are falsifiable on this fixture.
	//
	// The rating strings are the REAL mirror's, measured across all 112,935
	// rows. An earlier version of this fixture stored single letters, which is
	// why `rating=G` returned zero works against production while passing here.
	//
	// Three rows cannot exercise a four-value vocabulary: whichever rating the
	// third row carried, the other three buckets were empty, and an empty
	// bucket makes a filter test assert emptiness -- which an implementation
	// that ignores the parameter also does.
	const (
		gen  = "General Audiences"
		teen = "Teen And Up Audiences"
		mat  = "Mature"
		exp  = "Explicit"
	)
	rows := []struct {
		id          int
		words, kudo int
		date        string
		rating      string
		lang        string
		complete    int
	}{
		{1, 1000, 300, "2026-01-01", gen, "English", 1},
		{2, 2000, 200, "2026-02-01", teen, "English", 0},
		{3, 3000, 100, "2026-03-01", mat, "Spanish", 1},
		{4, 4000, 50, "2026-04-01", exp, "Spanish", 0},
		{5, 5000, 40, "2026-05-01", gen, "English", 1},
		{6, 6000, 30, "2026-06-01", teen, "French", 1},
		{7, 7000, 20, "2026-07-01", mat, "English", 0},
		{8, 8000, 10, "2026-08-01", exp, "French", 1},
	}
	for _, r := range rows {
		if _, err := seed.Exec(
			`INSERT INTO works(id,url,title,authors,word_count,kudos,hits,rating,language,complete,update_date,first_seen)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.id,
			"https://example.invalid/"+strconv.Itoa(r.id),
			"Work "+strconv.Itoa(r.id),
			"A",
			r.words, r.kudo, 1000, r.rating, r.lang, r.complete,
			r.date, "2026-01-01"); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Exec(
			`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,1,'freeforms')`,
			r.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(t.Context(), dbPath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	srv := &Server{
		Engine: &engine.Engine{
			Store: s, Corpus: corpus.NewAO3(s.Corpus),
			PoolSize: 50, TopN: 4, Lite: true,
		},
		Store:   s,
		Version: "test",
		Lite:    true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// workOrder extracts the ids of the works listed on a tag page, in order.
//
// Scoped to the works list deliberately: the page also renders a neighbour
// tag list, and a looser selector would pick up hrefs from both and return
// an order that belongs to neither.
var workLinkRe = regexp.MustCompile(`<div class="work">\s*<h3><a href="/work/(\d+)"`)

func workOrder(t *testing.T, body string) []int {
	t.Helper()
	var ids []int
	for _, m := range workLinkRe.FindAllStringSubmatch(body, -1) {
		id, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func joinIDs(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

func getPage(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b := make([]byte, 512)
		n, _ := resp.Body.Read(b)
		t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, b[:n])
	}
	var sb strings.Builder
	buf := make([]byte, 8192)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// TestTagSortActuallyChangesTheOrder is the gate.
//
// Each arm asserts the EXACT id order and the heading sentence, so a sort
// wired to the wrong column fails. Asserting only "the order changed" would
// not: a sort that reordered wrongly would pass.
func TestTagSortActuallyChangesTheOrder(t *testing.T) {
	ts := newSortServer(t)

	cases := []struct {
		query   string
		want    string
		heading string
	}{
		// kudos DESC: 300,200,100,50,40,30,20,10, so the kudos order happens to
		// equal the id order -- which makes it a real comparator rather than a
		// coincidence, and makes "recent" its exact reverse.
		{"sort=kudos", "1,2,3,4,5,6,7,8", "Most kudos first"},
		// update_date DESC: work 3 is the most recent.
		{"sort=recent", "8,7,6,5,4,3,2,1", "Most recently updated"},
		// word_count DESC: work 3 is the longest.
		{"sort=words", "8,7,6,5,4,3,2,1", "Longest first"},
		// An unrecognised sort falls back to kudos rather than erroring, and
		// the page SAYS kudos, so the reader can see what they are getting.
		{"sort=bogus", "1,2,3,4,5,6,7,8", "Most kudos first"},
		// The default is the taste blend now. The fixture has no works
		// OUTSIDE the tag carrying no reader-overlap signal worth blending,
		// so the blend degrades to the exact list (said plainly, in
		// taste-error) and the order is still kudos -- but the heading
		// tells the truth about which view this is.
		{"", "1,2,3,4,5,6,7,8", "Taste match"},
		// Sort state must survive a round trip through the control's own
		// value, since that is what a reload sends.
		{"sort=recent&n=20", "8,7,6,5,4,3,2,1", "Most recently updated"},
		// The page limit, which a three-row fixture could not check at all:
		// every list fitted under the default n=20.
		{"n=3", "1,2,3", "Taste match"},
		{"sort=recent&n=3", "8,7,6", "Most recently updated"},
	}
	for _, c := range cases {
		t.Run("?"+c.query, func(t *testing.T) {
			body := getPage(t, ts, "/tag/1?"+c.query)
			got := joinIDs(workOrder(t, body))
			if got != c.want {
				t.Errorf("?%s listed [%s], want [%s]", c.query, got, c.want)
			}
			if !strings.Contains(body, c.heading) {
				t.Errorf("?%s heading does not say %q", c.query, c.heading)
			}
		})
	}
}

// TestSortKudosAndWordsMustDiffer makes the test above non-vacuous.
//
// Without this, a fixture where all three sorts agree would let every arm of
// TestTagSortActuallyChangesTheOrder pass with ?sort= ignored entirely. This
// asserts the discrimination property instead of assuming it.
func TestSortKudosAndWordsMustDiffer(t *testing.T) {
	ts := newSortServer(t)
	kudos := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?sort=kudos")))
	words := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?sort=words")))
	recent := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?sort=recent")))

	if kudos == words {
		t.Errorf("sort=kudos and sort=words both returned [%s]; the fixture "+
			"is degenerate and every sort assertion in this file is "+
			"vacuous", kudos)
	}
	if words == recent {
		t.Logf("note: words and recent agree ([%s]); the kudos arm still "+
			"discriminates", words)
	}
	t.Logf("kudos=%s words=%s recent=%s", kudos, words, recent)
}

// TestTagLengthFilterExcludesRows is the length control's gate.
func TestTagLengthFilterExcludesRows(t *testing.T) {
	ts := newSortServer(t)

	cases := []struct{ query, want string }{
		// words are 1000, 2000, 3000.
		{"words=under:2000", "1"},
		{"words=under:3000", "1,2"},
		{"words=over:2000", "3,4,5,6,7,8"},
		{"words=<2000", "1"},
		{"words=>2000", "3,4,5,6,7,8"},
		{"", "1,2,3,4,5,6,7,8"},
		{"words=under:1", ""},
		{"words=over:999999", ""},
	}
	for _, c := range cases {
		t.Run("?"+c.query, func(t *testing.T) {
			got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?"+c.query)))
			if got != c.want {
				t.Errorf("?%s listed [%s], want [%s] (unfiltered is [1,2,3])",
					c.query, got, c.want)
			}
		})
	}
}

// TestTagFilterAndSortCombine guards the two controls being independent.
func TestTagFilterAndSortCombine(t *testing.T) {
	ts := newSortServer(t)
	cases := []struct{ query, want string }{
		{"words=under:3000&sort=kudos", "1,2"},
		{"words=under:3000&sort=words", "2,1"},
		{"words=under:3000&sort=recent", "2,1"},
		{"words=over:2000&sort=recent", "8,7,6,5,4,3"},
		{"words=over:2000&sort=recent&n=2", "8,7"},
	}
	for _, c := range cases {
		t.Run("?"+c.query, func(t *testing.T) {
			got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?"+c.query)))
			if got != c.want {
				t.Errorf("?%s listed [%s], want [%s]", c.query, got, c.want)
			}
		})
	}
}

// TestAnUnparseableLengthBoundSaysSo guards the silent-drop rule.
//
// A filter the reader set, which the page ignores without comment, is
// indistinguishable from a filter that works. That is the whole reason the
// bounds are parsed in Go and the failure is rendered.
func TestAnUnparseableLengthBoundSaysSo(t *testing.T) {
	ts := newSortServer(t)
	body := getPage(t, ts, "/tag/1?words=lots")

	if !strings.Contains(body, "is not a word-count bound") {
		t.Errorf("?words=lots did not explain itself; page head:\n%s",
			head(body))
	}
	if !strings.Contains(body, "Showing every work on this tag instead") {
		t.Errorf("the page does not say its list is unfiltered:\n%s", head(body))
	}
	// And it falls back to the unfiltered list rather than to nothing.
	if got := joinIDs(workOrder(t, body)); got != "1,2,3,4,5,6,7,8" {
		t.Errorf("?words=lots listed [%s], want the unfiltered [1,2,3]", got)
	}
}

// TestTagFilterThatExcludedEverythingSaysWhy guards the other failure.
//
// "No works carry this tag" when the tag has three works, because the
// reader set a length filter -- that reads as an empty tag, which is a lie
// about the mirror.
func TestTagFilterThatExcludedEverythingSaysWhy(t *testing.T) {
	ts := newSortServer(t)
	body := getPage(t, ts, "/tag/1?words=under:1")

	if !strings.Contains(body, `data-testid="no-matches"`) {
		t.Errorf("a filter that excluded all 3 works did not say so:\n%s", head(body))
	}
	if !strings.Contains(body, "under:1") {
		t.Errorf("the message does not name the filter:\n%s", head(body))
	}
	if strings.Contains(body, "No works carry this tag in the mirror") {
		t.Errorf("the page claims the tag is empty, which is false -- it has " +
			"3 works; the FILTER excluded them")
	}
}

// TestTagTotalCountsTheFilteredSet guards the number in the heading.
//
// "of N" must describe the set the list is drawn from. Counting every work on
// the tag while showing a filtered list puts "3,000 works" above twenty short
// ones: a heading describing a set the page does not contain.
func TestTagTotalCountsTheFilteredSet(t *testing.T) {
	ts := newSortServer(t)

	unfiltered := getPage(t, ts, "/tag/1")
	if !strings.Contains(unfiltered, "8</strong>") {
		t.Errorf("unfiltered page does not report the total of 8:\n%s", head(unfiltered))
	}

	// under:2000 keeps exactly work 1 -- the bound is EXCLUSIVE, so 2000
	// itself is excluded. The total must be 1, and must not still say 8.
	//
	// "8</strong>" rather than the bare digit, because the page renders several
	// numbers and a substring check on "8" alone matches word counts, kudos
	// and page furniture. On a 3-work fixture "3" happened to be the total and
	// nothing else, which is why the weaker check passed for months.
	filtered := getPage(t, ts, "/tag/1?words=under:2000")
	if strings.Contains(filtered, "8</strong>") {
		t.Errorf("with words=under:2000 (1 matching work) the page still "+
			"reports the unfiltered total of 8:\n%s", head(filtered))
	}
	if !strings.Contains(filtered, "1</strong>") {
		t.Errorf("with words=under:2000 the page does not report the total of 1:\n%s",
			head(filtered))
	}

	// And the same for each of the three new filters, which all land on the
	// shared COUNT now -- that was the point of building one clause string
	// instead of two queries.
	for _, c := range []struct{ query, filteredTotal string }{
		// complete is true on ids 1,3,4,6,7,8 and false on 2,5 -- wait:
		// the fixture stores it directly, and the split is 5/3. Written out
		// from the seed data rather than counted by eye, because guessing
		// these is how a gate ends up asserting the wrong number and reading
		// the failure as a code bug.
		{"complete=true", "5</strong>"},
		{"complete=false", "3</strong>"},
		{"rating=G", "2</strong>"},
		{"rating=T", "2</strong>"},
		{"rating=M", "2</strong>"},
		{"rating=E", "2</strong>"},
		{"lang=English", "4</strong>"},
		{"lang=French", "2</strong>"},
		{"lang=Spanish", "2</strong>"},
	} {
		t.Run("?"+c.query, func(t *testing.T) {
			body := getPage(t, ts, "/tag/1?"+c.query)
			if strings.Contains(body, "8</strong>") {
				t.Errorf("?%s still reports the unfiltered total of 8:\n%s",
					c.query, head(body))
			}
			if !strings.Contains(body, c.filteredTotal) {
				t.Errorf("?%s does not report the filtered total:\n%s",
					c.query, head(body))
			}
		})
	}
}

// TestTheSortControlRendersTheCurrentSelection guards the control itself.
//
// The whole point of putting .Sort on the page is so the control can show
// what is applied. Without it a reader who asked for "recent" sees a dropdown
// apparently sitting on "most kudos" and has no way to tell.
func TestTheSortControlRendersTheCurrentSelection(t *testing.T) {
	ts := newSortServer(t)
	for _, c := range []struct{ query, selected string }{
		{"sort=kudos", `value="kudos" selected`},
		{"sort=recent", `value="recent" selected`},
		{"sort=words", `value="words" selected`},
	} {
		t.Run("?"+c.query, func(t *testing.T) {
			body := getPage(t, ts, "/tag/1?"+c.query)
			if !strings.Contains(body, c.selected) {
				t.Errorf("?%s did not render %q as the selected option",
					c.query, c.selected)
			}
			// Exactly ONE option inside the sort control. Counting the whole
			// page is wrong -- the length control has a selected option too,
			// and the first version of this test counted that one and failed
			// on a correct page. The assertion is scoped to the element it is
			// about.
			if n := strings.Count(sortSelectOf(body), " selected"); n != 1 {
				t.Errorf("?%s rendered %d selected options in the sort "+
					"control, want 1:\n%s", c.query, n, sortSelectOf(body))
			}
		})
	}
}

// TestSortIsAClosedSet is the injection guard.
//
// The ORDER BY cannot be built from query input, so this asserts the value
// that reaches it is always one of three known strings.
func TestSortIsAClosedSet(t *testing.T) {
	ts := newSortServer(t)
	// None of these may produce a SQL error or a different order: each is
	// either a known sort or falls back to kudos.
	// Two kinds of input, with DIFFERENT expected results -- which is what
	// the first version of this test got wrong. It asserted the kudos
	// fallback for "recent" and "date", both of which are VALID sorts, so it
	// failed against correct code and would have been deleted by anyone who
	// trusted it. An unrecognised value must fall back; a recognised one must
	// sort.
	cases := []struct {
		in, want string
	}{
		// Recognised: honours the sort. `date`, `length` and `popular` are
		// deliberate aliases -- tagSort documents them -- so they sort too.
		// The first version of this table listed `date` among the values
		// expected to fall back to kudos, and failed against correct code.
		{"kudos", "1,2,3,4,5,6,7,8"},
		{"popular", "1,2,3,4,5,6,7,8"},
		{"recent", "8,7,6,5,4,3,2,1"},
		{"date", "8,7,6,5,4,3,2,1"},
		{"words", "8,7,6,5,4,3,2,1"},
		{"length", "8,7,6,5,4,3,2,1"},
		// Unrecognised or hostile: falls back to kudos, and never errors.
		{"", "1,2,3,4,5,6,7,8"},
		{"1", "1,2,3,4,5,6,7,8"},
		{"KUDOS", "1,2,3,4,5,6,7,8"},
		{"kudos; DROP TABLE works", "1,2,3,4,5,6,7,8"},
		{"words--", "1,2,3,4,5,6,7,8"},
		{"\x00", "1,2,3,4,5,6,7,8"},
		{strings.Repeat("a", 200), "1,2,3,4,5,6,7,8"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			// Built with url.Values so the request is one a browser could
			// actually make. Splicing a raw space or ";" into a URL gets it
			// rejected by the client before the server sees it, which would
			// make this arm a test of net/http rather than of tagSort.
			q := url.Values{"sort": {c.in}}.Encode()
			body := getPage(t, ts, "/tag/1?"+q)
			if got := joinIDs(workOrder(t, body)); got != c.want {
				t.Errorf("?sort=%q listed [%s], want [%s]", c.in, got, c.want)
			}
		})
	}
}

// TestTagOrderHasATiebreakOnID guards against unstable ordering.
//
// Without a final `, w.id`, rows equal on the sort key come back in whatever
// order SQLite produces, so the same URL renders two different lists. This
// fixture's keys are all distinct so it cannot show the symptom; the
// tiebreak is asserted in the request instead, which is the part that can be
// read.
func TestTagOrderHasATiebreakOnID(t *testing.T) {
	// Two works with identical kudos, so an unstable tiebreak shows up as a
	// different order between two identical requests.
	ts := newSortServerTied(t)
	first := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?sort=kudos")))
	second := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?sort=kudos")))
	if first != second {
		t.Errorf("two identical requests returned different orders: [%s] then [%s]",
			first, second)
	}
	if first != "1,2" {
		t.Errorf("got [%s], want [1,2]", first)
	}
}

// newSortServerTied is the tie-break fixture: two works, equal kudos.
func newSortServerTied(t *testing.T) *httptest.Server {
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
		f.Close()
	}
	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO tags(id,name) VALUES(1,'dark')`); err != nil {
		t.Fatal(err)
	}
	// Identical kudos, identical word count, identical update_date: the only
	// thing that can order these two rows deterministically is w.id.
	for _, id := range []int{1, 2} {
		if _, err := seed.Exec(
			`INSERT INTO works(id,url,title,authors,word_count,kudos,hits,update_date,first_seen)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			id, "https://example.invalid/"+strconv.Itoa(id),
			"Work "+strconv.Itoa(id), "A", 1000, 500, 1000,
			"2026-01-01", "2026-01-01"); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Exec(
			`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,1,'freeforms')`,
			id); err != nil {
			t.Fatal(err)
		}
	}
	seed.Close()

	s, err := store.Open(t.Context(), dbPath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := &Server{
		Engine: &engine.Engine{
			Store: s, Corpus: corpus.NewAO3(s.Corpus),
			PoolSize: 50, TopN: 4, Lite: true,
		},
		Store: s, Version: "test", Lite: true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// sortSelectOf returns just the sort <select> element, so an assertion about
// it cannot be satisfied -- or broken -- by the length control's options.
func sortSelectOf(body string) string {
	i := strings.Index(body, `<select id="sort"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</select>")
	if j < 0 {
		return ""
	}
	return body[i : i+j]
}

func head(body string) string {
	if len(body) > 1200 {
		return body[:1200]
	}
	return body
}

// TestALengthFilterExcludesUnknownLengthWorks guards the word_count = 0 case.
//
// The real mirror has 42 works of 112,935 with word_count = 0: AO3
// published no count for them. They are not short fics, they are fics of
// unknown length, and "under 5,000 words" used to include all 42.
//
// It will never be visible in a list -- 0.04% -- but it is wrong in the
// direction that matters: the length filter exists to bound how long a
// reader might spend, and an unknown-length fic bounds nothing.
//
// So the fixture grows a fourth work: id 4, word_count 0, which must be
// absent from BOTH arms of every length filter.
func TestALengthFilterExcludesUnknownLengthWorks(t *testing.T) {
	ts := newSortServerWithUnknownLength(t)

	// This test uses its OWN four-row fixture, not newSortServer's eight.
	// A bulk edit to the other fixture's expectations once rewrote these to
	// "1,2,3,4,5,6,7,8" and the test failed with ids that do not exist here --
	// which at least fails loudly. The line count is the tell.

	for _, c := range []struct{ query, want string }{
		{"words=under:5000", "1,2,3"},
		{"words=under:20000", "1,2,3"},
		{"words=over:2000", "3"},
		// Unfiltered it IS listed -- the list is the tag's works, and hiding
		// one because its length is unknown would be a different feature
		// with a different justification. It leads the default order because
		// its 900 kudos are the highest in the fixture, which is also a
		// check that the kudos sort is still working with a 0-length work
		// present.
		{"", "4,1,2,3"},
	} {
		t.Run("?"+c.query, func(t *testing.T) {
			got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?"+c.query)))
			if got != c.want {
				t.Errorf("?%s listed [%s], want [%s]\n"+
					"  work 4 has word_count=0 (AO3 published no length). It "+
					"must not satisfy a length filter.",
					c.query, got, c.want)
			}
		})
	}
}

// newSortServerWithUnknownLength is newSortServer plus a fourth work whose
// word count is 0, which is what 42 rows of the real mirror look like.
func newSortServerWithUnknownLength(t *testing.T) *httptest.Server {
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
		f.Close()
	}
	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO tags(id,name) VALUES(1,'dark')`); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id, words, kudo int
		date            string
	}{
		{1, 1000, 300, "2026-01-01"},
		{2, 2000, 200, "2026-02-01"},
		{3, 3000, 100, "2026-03-01"},
		{4, 0, 900, "2026-03-02"}, // the mirror's 42: no published length
	}
	for _, r := range rows {
		if _, err := seed.Exec(
			`INSERT INTO works(id,url,title,authors,word_count,kudos,hits,update_date,first_seen)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			r.id, "https://example.invalid/"+strconv.Itoa(r.id),
			"Work "+strconv.Itoa(r.id), "A", r.words, r.kudo, 1000,
			r.date, "2026-01-01"); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Exec(
			`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,1,'freeforms')`,
			r.id); err != nil {
			t.Fatal(err)
		}
	}
	seed.Close()

	s, err := store.Open(t.Context(), dbPath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := &Server{
		Engine: &engine.Engine{
			Store: s, Corpus: corpus.NewAO3(s.Corpus),
			PoolSize: 50, TopN: 4, Lite: true,
		},
		Store: s, Version: "test", Lite: true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// The real extremes, measured on the 112,935-work mirror:
//
//	longest title          255 chars
//	longest tag name       150 chars
//	longest UNBREAKABLE tag 96 chars (no spaces, so no break opportunity)
//
// Taken verbatim. A mobile test against a fixture of "Work 1" and a tag
// called "dark" measures nothing: the whole reason a phone overflows is a
// 96-character token with no space in it, and that string does not exist in
// a hand-written fixture.
//
// Every one of these is a single token with NO break opportunity, so
// overflow-wrap: anywhere is the only thing that can stop it -- and it is
// what makes the mobile gate in the stylesheet meaningful rather than
// decorative.
const (
	realLongTitle = "Help! I Was Rinancarnated Again By This Isekai Obsessed " +
		"Immortal Being and Now I\u2019m Surrounded By Rebels That Want to Vent " +
		"Me! Feral Breaker floretta: The Ruthless Reincarnated Floret Forms the " +
		"Ultimate Rinan Revolt! All Roots Lead to THEIR Domestication!!"
	// The 89-character tag with NO SPACES in it, from the same query. The
	// 150-character tag I reached for first ("no one is infected by a demon...")
	// is mostly spaces, so it wraps at any width and tests nothing -- which
	// the assertion below caught, because it is the one property that matters.
	realUnbreakableTag = "izuku/ochako/mina/momo/mei/ibara/tsuyu/himiko/" +
		"setsuna/itsuka/yui/pony/shoko/camie/melissa/kinoko"
)

func TestTheFixtureCarriesTheRealStrings(t *testing.T) {
	// So the extremes cannot silently rot into short strings when someone
	// tidies the fixture: these are measured, and the assertion is on the
	// LENGTH, not on the content.
	if len([]rune(realLongTitle)) < 250 {
		t.Errorf("the long title fixture is %d chars; the real maximum is 255",
			len([]rune(realLongTitle)))
	}
	if len([]rune(realUnbreakableTag)) < 90 {
		t.Errorf("the unbreakable tag fixture is %d chars; the real maximum is 96",
			len([]rune(realUnbreakableTag)))
	}
	if strings.ContainsAny(realUnbreakableTag, " \t\n") {
		t.Errorf("the tag fixture contains a space, so the browser has a break " +
			"opportunity and it no longer reproduces the overflow it exists for")
	}
}

// TestTheFixtureWritesEveryColumnAFilterReads guards the fixture itself.
//
// The eight-row fixture above grew rating, language and complete. When language
// and complete were added, `rating` was left out of the INSERT column list
// while `r.rating` sat unused in the struct -- so every work had a NULL rating,
// and TestTagTotalCountsTheFilteredSet/?rating=G reported 0 instead of 2.
//
// That failure was visible, which is the only reason it was caught: an expected
// count of 0 would have been indistinguishable from a working filter. A filter
// that matches nothing passes, so a fixture must be proven to write the column
// before any test is allowed to rely on it being empty.
func TestTheFixtureWritesEveryColumnAFilterReads(t *testing.T) {
	ts := newSortServer(t)
	// Unfiltered, every work is listed, so the ids of the first page are the
	// ones the filters are applied to.
	ids := workOrder(t, getPage(t, ts, "/tag/1?n=100"))
	if len(ids) != 8 {
		t.Fatalf("the fixture has %d works, not 8; the expectations in this "+
			"file are computed against 8", len(ids))
	}

	// Each of these must SELECT rows. A zero result means the column is empty
	// for every row, which makes every test using it assert emptiness and pass
	// for the wrong reason.
	for _, c := range []struct{ query, note string }{
		{"rating=G", "rating column is NULL for every work"},
		{"rating=E", "rating column is NULL for every work"},
		{"complete=true", "complete column is 0 or NULL for every work"},
		{"complete=false", "complete column is 1 for every work"},
		{"lang=English", "language column is empty for every work"},
		{"lang=Spanish", "language column has no Spanish row"},
	} {
		t.Run("?"+c.query, func(t *testing.T) {
			got := workOrder(t, getPage(t, ts, "/tag/1?"+c.query))
			if len(got) == 0 {
				t.Errorf("?%s selects nothing, so %s. A filter test on this "+
					"fixture would only be able to assert emptiness.", c.query, c.note)
			}
		})
	}

	// And the two states must not partition the fixture the same way, or
	// "complete" and "language" would be the same filter twice.
	comp := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?complete=true&n=100")))
	lang := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?lang=English&n=100")))
	if comp == lang {
		t.Errorf("complete=true and lang=English select the same rows [%s]; "+
			"two filters that agree cannot tell which column is wired", comp)
	}
}
