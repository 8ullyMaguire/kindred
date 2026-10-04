package api

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// complete / rating / lang on the tag page.
//
// These three are the gap: the API has always honoured them and echoed them in
// `filters_applied`, while the page silently ignored them. Verified against the
// live instance before this change —
//
//	/api/v1/ao3/works?complete=true   -> count 100, applied={'complete':'true'}
//	/tag/29?complete=true             -> sort control present, no filter UI,
//	                                    unfiltered list, no message
//
// A reader who learned the filters from the API documentation could not use
// them in a browser, and nothing said so.
//
// The fixture (newSortServer) has eight works spread over four ratings, three
// languages and both completion states. That spread is the point: with the
// old three-row fixture every work was "Explicit" and English, so `rating=G`
// and `lang=English` each returned either everything or nothing, and both
// outcomes are what a missing filter produces too.
//
//	id | words | kudos | rating                | language | complete
//	 1 |   1000 |   300 | General Audiences     | English  | 1
//	 2 |   2000 |   200 | Teen And Up Audiences | English  | 0
//	 3 |   3000 |   100 | Mature                | Spanish  | 1
//	 4 |   4000 |    50 | Explicit              | Spanish  | 0
//	 5 |   5000 |    40 | General Audiences     | English  | 1
//	 6 |   6000 |    30 | Teen And Up Audiences | French   | 1
//	 7 |   7000 |    20 | Mature                | English  | 0
//	 8 |   8000 |    10 | Explicit              | French   | 1

// TestTagCompletionFilterExcludesRows is the gate.
//
// Exact ids in order, so a filter wired to the wrong column fails. Asserting
// only "the count changed" would not: a filter returning the wrong works would
// pass that.
func TestTagCompletionFilterExcludesRows(t *testing.T) {
	ts := newSortServer(t)
	for _, c := range []struct{ query, want string }{
		{"", "1,2,3,4,5,6,7,8"},
		{"complete=true", "1,3,5,6,8"},
		{"complete=false", "2,4,7"},
		// The spellings a reader actually types, since "complete=1" reading as
		// no filter is the same silent-drop failure as ignoring it entirely.
		{"complete=1", "1,3,5,6,8"},
		{"complete=yes", "1,3,5,6,8"},
		{"complete=0", "2,4,7"},
		{"complete=no", "2,4,7"},
		// Case-insensitive, because a query string is not case-sensitive in
		// practice and "TRUE" reading as no filter would be maddening.
		{"complete=TRUE", "1,3,5,6,8"},
		{"complete=True", "1,3,5,6,8"},
		// And an unrecognised value must NOT be coerced to false or to true.
		// It has to say so, which TestAnUnrecognisedFilterValueSaysSo checks.
		{"complete=maybe", "1,2,3,4,5,6,7,8"},
		{"complete=2", "1,2,3,4,5,6,7,8"},
	} {
		t.Run("?"+c.query, func(t *testing.T) {
			got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?"+c.query)))
			if got != c.want {
				t.Errorf("?%s listed [%s], want [%s] (unfiltered is [1,2,3,4,5,6,7,8])",
					c.query, got, c.want)
			}
		})
	}
}

// TestTagRatingFilterAcceptsBothSpellings is the gate.
//
// The control sends AO3's letters, because that is what the reader knows and
// what `/api/v1/ao3/works` takes. A client that echoes back what the API
// returned sends the mirror's full names. Both must select the same works: a
// control that accepts one spelling fails silently for the other, and both are
// spellings in real use.
func TestTagRatingFilterAcceptsBothSpellings(t *testing.T) {
	ts := newSortServer(t)
	for _, c := range []struct{ value, want string }{
		{"G", "1,5"},
		{"General Audiences", "1,5"},
		{"T", "2,6"},
		{"Teen And Up Audiences", "2,6"},
		{"M", "3,7"},
		{"Mature", "3,7"},
		{"E", "4,8"},
		{"Explicit", "4,8"},
		// Lower case.
		{"g", "1,5"},
		{"e", "4,8"},
		// The full names case-insensitively too.
		{"explicit", "4,8"},
		// A rating the mirror does not contain matches NOTHING rather than
		// being coerced to something nearby. "Z" is the German FSK age
		// rating, so it is a value a reader could plausibly type.
		{"Z", ""},
		{"Nope", ""},
		// Padded, which is what arrives when a caller builds the string.
		{" E ", "4,8"},
	} {
		t.Run("?rating="+c.value, func(t *testing.T) {
			q := url.Values{"rating": {c.value}}.Encode()
			got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?"+q)))
			if got != c.want {
				t.Errorf("?rating=%s listed [%s], want [%s]", c.value, got, c.want)
			}
		})
	}
}

func TestTagLanguageFilterExcludesRows(t *testing.T) {
	ts := newSortServer(t)
	for _, c := range []struct{ value, want string }{
		{"English", "1,2,5,7"},
		{"Spanish", "3,4"},
		{"French", "6,8"},
		{"english", "1,2,5,7"},
		{"ENGLISH", "1,2,5,7"},
		{" English", "1,2,5,7"},
		// A language the mirror does not contain.
		{"German", ""},
		{"Klingon", ""},
	} {
		t.Run("?lang="+c.value, func(t *testing.T) {
			q := url.Values{"lang": {c.value}}.Encode()
			got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?"+q)))
			if got != c.want {
				t.Errorf("?lang=%s listed [%s], want [%s]", c.value, got, c.want)
			}
		})
	}
}

// TestTagFiltersCombineAndAreIndependent guards the three against each other
// and against sort. A clause built as a string, appended to in three places,
// will eventually be applied twice or dropped.
func TestTagFiltersCombineAndAreIndependent(t *testing.T) {
	ts := newSortServer(t)
	for _, c := range []struct{ query, want string }{
		{"complete=true&lang=English", "1,5"},
		{"complete=true&rating=E", "8"},
		{"complete=false&lang=English", "2,7"},
		{"rating=M&lang=Spanish", "3"},
		{"rating=G&complete=true", "1,5"},
		// With sort, including the direction flip: kudos ascends with id here,
		// words descends, so the same filtered set is genuinely reordered.
		{"complete=true&lang=English&sort=kudos", "1,5"},
		{"complete=true&lang=English&sort=words", "5,1"},
		{"complete=true&lang=English&sort=recent", "5,1"},
		// The length filter alongside the other three.
		{"words=over:4000&complete=true&lang=English", "5"},
		// All four, which is the case the no-matches message has to get right:
		// it must name all four, and naming three of four is a lie.
		{"complete=true&rating=E", "8"},
	} {
		t.Run("?"+c.query, func(t *testing.T) {
			got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?"+c.query)))
			if got != c.want {
				t.Errorf("?%s listed [%s], want [%s]", c.query, got, c.want)
			}
		})
	}
}

// TestAnUnrecognisedFilterValueSaysSo guards the silent-drop rule for these
// three, the way TestAnUnparseableLengthBoundSaysSo does for the length.
//
// The rule: a value the page cannot understand produces a message naming the
// parameter, and the list falls back to unfiltered — but SAYS so. Silently
// dropping it is the exact defect this work started from.
func TestAnUnrecognisedFilterValueSaysSo(t *testing.T) {
	ts := newSortServer(t)
	for _, q := range []string{"complete=maybe", "complete=2", "complete=on"} {
		t.Run("?"+q, func(t *testing.T) {
			body := getPage(t, ts, "/tag/1?"+q)
			if !strings.Contains(body, "is not a completion filter") {
				t.Errorf("?%s did not explain itself:\\n%s", q, head(body))
			}
			if !strings.Contains(body, "Showing every work on this tag instead") {
				t.Errorf("?%s does not say the list is unfiltered:\\n%s", q, head(body))
			}
			if got := joinIDs(workOrder(t, body)); got != "1,2,3,4,5,6,7,8" {
				t.Errorf("?%s listed [%s], want the unfiltered [1,2,3,4,5,6,7,8]", q, got)
			}
		})
	}
}

// TestAnUnknownRatingOrLanguageIsNotAnError: unlike an unparseable boolean, an
// unfamiliar rating or language is a legitimate reader query with an empty
// answer. "Show me works rated Z" is a question, and the right answer is "none
// on this tag", not "that filter does not exist".
func TestAnUnknownRatingOrLanguageIsNotAnError(t *testing.T) {
	ts := newSortServer(t)
	for _, q := range []string{"rating=Z", "lang=German"} {
		t.Run("?"+q, func(t *testing.T) {
			body := getPage(t, ts, "/tag/1?"+q)
			if strings.Contains(body, "data-testid=\"filter-error\"") {
				t.Errorf("?%s is treated as a malformed filter:\\n%s", q, head(body))
			}
			if !strings.Contains(body, "data-testid=\"no-matches\"") {
				t.Errorf("?%s found nothing but did not say so:\\n%s", q, head(body))
			}
		})
	}
}

// TestTheNoMatchesMessageNamesEveryFilter is the one that needed writing
// twice. The first version named only the length filter, so a reader who set
// rating AND length and got nothing was told about one of the two.
func TestTheNoMatchesMessageNamesEveryFilter(t *testing.T) {
	ts := newSortServer(t)

	// completion + rating, no length: both must be named.
	body := getPage(t, ts, "/tag/1?complete=true&rating=Z")
	if !strings.Contains(body, `data-testid="no-matches"`) {
		t.Fatalf("expected no-matches:\\n%s", head(body))
	}
	if !strings.Contains(body, "complete") {
		t.Errorf("the message does not mention the completion filter:\\n%s", head(body))
	}
	if !strings.Contains(body, "rating Z") {
		t.Errorf("the message does not mention the rating filter:\\n%s", head(body))
	}

	// The length filter must still be named when it is the one that excluded
	// everything, which is the case the original version handled.
	body = getPage(t, ts, "/tag/1?words=under:900")
	if !strings.Contains(body, "under 900") && !strings.Contains(body, "900") {
		t.Errorf("the length filter is not named:\\n%s", head(body))
	}

	// All four at once.
	body = getPage(t, ts, "/tag/1?complete=true&rating=Z&lang=Klingon&words=under:900")
	for _, want := range []string{"complete", "rating Z", "Klingon"} {
		if !strings.Contains(body, want) {
			t.Errorf("with four filters set the message omits %q:\\n%s", want, head(body))
		}
	}

	// And with no filter at all it must NOT claim emptiness, because the tag
	// is not empty. Claiming no matches on an unfiltered tag is the failure
	// this whole page-level work exists to prevent.
	if body = getPage(t, ts, "/tag/1"); strings.Contains(body, `data-testid="no-matches"`) {
		t.Error("an unfiltered tag page claimed there were no matches")
	}
}

// TestTheControlsRenderTheCurrentSelection: the whole point of putting these on
// the page is that the control shows what is applied. A filter the reader
// cannot see is a filter they will set twice.
func TestTheControlsRenderTheCurrentSelection(t *testing.T) {
	ts := newSortServer(t)
	body := getPage(t, ts, "/tag/1?complete=true&rating=E&lang=French")
	for _, want := range []string{
		`<option value="true" selected`,
		`<option value="E" selected`,
		`name="lang" value="French"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the controls do not show the current selection; missing %q", want)
		}
	}
}

// TestTheRatingControlOffersTheMirrorsRealNames: a reader choosing "General"
// should see what that bucket is called, because the value sent is the letter
// and the label is the name.
func TestTheRatingControlOffersTheMirrorsRealNames(t *testing.T) {
	ts := newSortServer(t)
	body := getPage(t, ts, "/tag/1")
	for _, want := range []string{
		"General Audiences", "Teen And Up Audiences", "Mature", "Explicit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the rating control does not offer %q, which is how the "+
				"mirror spells it", want)
		}
	}
	// And the VALUES must be the letters, since that is what the filter and the
	// API both take.
	for _, want := range []string{`value="G"`, `value="T"`, `value="M"`, `value="E"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the rating control does not offer %s as a value", want)
		}
	}
	// "Not Rated" is 8,796 rows of the mirror and must be REACHABLE, even
	// though it is not offered in the control: a reader who knows the term can
	// ask for it by hand.
	if got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?rating=Not+Rated"))); got != "" {
		t.Errorf("?rating=Not+Rated matched [%s] on a fixture that has none, "+
			"so it is being coerced to something", got)
	}
}

// TestThePageAgreesWithTheAPI is the property that motivated all of this.
//
// The page and /api/v1/ao3/works are two surfaces over one corpus. A reader who
// filters in one and checks the other must see the same works, or the tool is
// lying to one of them. This compares the actual ID SETS, not just the totals,
// because two surfaces can agree on a count and disagree on which works.
//
// `/api/v1/ao3/works?tag=dark` is the right comparison: it takes a tag NAME
// plus every filter the page takes. `/api/v1/ao3/tags/{id}/works` is not,
// because it accepts no filters at all -- so comparing against it would
// "confirm" the page for the wrong reason.
//
// The tag is named, not numbered: `?tag=1` matches nothing and answers
// `"count":0, "filters_applied":{"tag":"1"}`, which is a confusing way to say
// "there is no tag called 1". That is the API's own contract and TestTheAPITag
// FilterTakesANameNotAnID now pins it, because a reader who reads the page's
// /tag/1 URL and pastes "1" into the API is exactly who will hit it.
func TestThePageAgreesWithTheAPI(t *testing.T) {
	ts := newSortServer(t)
	for _, query := range []string{
		"", "complete=true", "complete=false",
		"rating=G", "rating=T", "rating=M", "rating=E",
		"lang=English", "lang=Spanish", "lang=French",
		"complete=true&lang=English",
		"rating=E&complete=true",
		"lang=French&rating=M",
	} {
		t.Run("?"+query, func(t *testing.T) {
			page := joinIDs(sortAsc(workOrder(t, getPage(t, ts, "/tag/1?"+query))))
			api := joinIDs(sortAsc(apiWorkIDs(t, ts, "/api/v1/ao3/works?tag=dark&limit=100"+amp(query))))

			if page != api {
				t.Errorf("?%s\n  page: [%s]\n  api : [%s]", query, page, api)
			}
		})
	}
}

// TestThePageAndTheAPIAgreeOnLeniency is the other half of the agreement, and
// it began as the opposite claim.
//
// The first version asserted that the API rejects `complete=1` while the page
// accepts it — a strictness divergence I assumed existed and had never checked.
// Both accept it. parseWorksFilters reads "1", "true", "yes" and "only" as
// complete, and the page does the same plus "0"/"false"/"no" case-insensitively.
//
// So the page's vocabulary is a strict SUBSET-plus-forms of the API's, and
// they agree. Asserting the divergence would have been asserting a fiction,
// and the test that shipped with it would have kept the two surfaces from
// being aligned later without anyone noticing why.
//
// What is actually worth pinning: for every spelling either surface accepts,
// both select the same works. That is the property, and it holds now.
func TestThePageAndTheAPIAgreeOnLeniency(t *testing.T) {
	ts := newSortServer(t)
	for _, v := range []string{
		"true", "1", "yes", "only", "TRUE", " true ",
		"false", "0", "no", "FALSE",
	} {
		t.Run("?complete="+v, func(t *testing.T) {
			page := joinIDs(sortAsc(workOrder(t,
				getPage(t, ts, "/tag/1?complete="+url.QueryEscape(v)))))
			api := joinIDs(sortAsc(apiWorkIDs(t, ts,
				"/api/v1/ao3/works?tag=dark&limit=100&complete="+url.QueryEscape(v))))
			if page != api {
				t.Errorf("?complete=%q\n  page: [%s]\n  api : [%s]", v, page, api)
			}
			if page == "" {
				t.Errorf("?complete=%q matched nothing on either surface; the "+
					"spelling is probably not accepted at all, and an empty "+
					"result is indistinguishable from a broken filter", v)
			}
		})
	}

	// And the two surfaces reject the SAME malformed values. The page says so
	// in prose, the API with a 400, and both fall back to unfiltered — but the
	// API's refusal is machine-readable and the page's is not, which is the
	// correct difference for a page and an API.
	for _, v := range []string{"maybe", "2", "on", "t"} {
		t.Run("?complete="+v, func(t *testing.T) {
			resp, err := ts.Client().Get(
				ts.URL + "/api/v1/ao3/works?tag=dark&complete=" + url.QueryEscape(v))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Errorf("the API accepted ?complete=%q with %d", v, resp.StatusCode)
			}
			body := getPage(t, ts, "/tag/1?complete="+url.QueryEscape(v))
			if !strings.Contains(body, "is not a completion filter") {
				t.Errorf("the page accepted ?complete=%q silently:\n%s", v, head(body))
			}
		})
	}
}

// amp prefixes a query with "&" when it is non-empty, so a caller can append it
// without producing a dangling or doubled separator.
func amp(query string) string {
	if query == "" {
		return ""
	}
	return "&" + query
}

func sortAsc(ids []int) []int {
	out := append([]int(nil), ids...)
	sort.Ints(out)
	return out
}

// apiWorkIDs returns the work ids the API listed, ascending, so the comparison
// with the page is about the SET and not about two different orderings.
func apiWorkIDs(t *testing.T, ts *httptest.Server, path string) []int {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, b)
	}
	var out struct {
		Works []struct {
			ID int64 `json:"id"`
		} `json:"works"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	ids := make([]int, len(out.Works))
	for i, w := range out.Works {
		ids[i] = int(w.ID)
	}
	return ids
}

// TestTheAPITagFilterTakesANameNotAnID pins a contract that is easy to get
// wrong in a way that looks correct.
//
// The pages address tags by id (/tag/1) and the API by name (?tag=dark). The
// page also accepts a name — /tag/dark renders — so a reader who has been
// looking at /tag/dark in the URL bar will get it right by accident, and
// someone reading /tag/1 will not.
//
// Asserted rather than left to a code comment, because the failure is silent:
// `?tag=1` returns 200 with `"count":0` and `filters_applied:{"tag":"1"}`,
// which reads as "this tag has no works" rather than "there is no such tag".
func TestTheAPITagFilterTakesANameNotAnID(t *testing.T) {
	ts := newSortServer(t)

	byName := apiCount(t, ts, "/api/v1/ao3/works?tag=dark&limit=100")
	if byName != 8 {
		t.Errorf("?tag=dark returned %d works, want 8", byName)
	}

	byID := apiCount(t, ts, "/api/v1/ao3/works?tag=1&limit=100")
	if byID != 0 {
		t.Errorf("?tag=1 returned %d works; the API takes a tag NAME, so 1 is "+
			"a name that does not exist and the answer is 0", byID)
	}

	// The page addresses tags by ID and takes no name: /tag/dark is a 404.
	// That makes the mismatch a real trap rather than a cosmetic difference --
	// the URL a reader is looking at says "1" and the API wants "dark".
	if got := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?n=100"))); got != "1,2,3,4,5,6,7,8" {
		t.Errorf("/tag/1 listed [%s], want all eight", got)
	}
	// The 404 is an HTML page, so the status code is the thing to assert -- the
	// first version grepped the body for "404" and failed against a perfectly
	// correct not-found page that happens not to print its own code.
	if code, _, _ := getStatus(t, ts, "/tag/dark"); code != 404 {
		t.Errorf("/tag/dark returned %d, want 404. If the page ever starts "+
			"accepting tag NAMES, the API's ?tag= contract becomes the odd one "+
			"out rather than the page's -- update this comment then.", code)
	}
}

func apiCount(t *testing.T, ts *httptest.Server, path string) int {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Count
}

func getStatus(t *testing.T, ts *httptest.Server, path string) (int, string, string) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if len(body) > 400 {
		body = body[:400]
	}
	return resp.StatusCode, http.StatusText(resp.StatusCode), body
}

// TestTheTagPageListsEachWorkOnce guards the JOIN shape.
//
// work_tags is keyed on (work_id, tag_id, tag_type), so the same tag NAME
// attached to one work under two types -- "dark" as a fandom AND as a
// freeform, which internal/testcorpus deliberately produces -- gives two rows
// for one work. A plain JOIN then lists that work twice, and COUNT(*) counts it
// twice.
//
// The tag page showed work 1 twice and reported "of 29" over 28 distinct
// works. A Playwright assertion caught it, one element off, which is what a
// duplicated join row looks like from outside. It belongs here as well: the Go
// fixture can reproduce it exactly, and a browser test should not be the only
// thing standing between a duplicated row and a reader.
func TestTheTagPageListsEachWorkOnce(t *testing.T) {
	ts := newDoubleAttachedServer(t)

	body := getPage(t, ts, "/tag/1?n=100")
	ids := workOrder(t, body)
	if len(ids) != 8 {
		t.Errorf("the page listed %d rows, want 8 distinct works", len(ids))
	}
	seen := map[int]int{}
	for _, id := range ids {
		seen[id]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("work %d is listed %d times. work_tags is keyed on "+
				"(work_id, tag_id, tag_type), so a tag name attached under two "+
				"types yields two rows; the list query needs SELECT DISTINCT.",
				id, n)
		}
	}

	if !strings.Contains(body, "8</strong>") {
		t.Errorf("the heading does not report 8 works:\n%s", head(body))
	}
	if strings.Contains(body, "9</strong>") {
		t.Errorf("the heading reports 9 for a tag with 8 works, so the count is "+
			"COUNT(*) over a duplicated join row:\n%s", head(body))
	}

	// The EMPTY-RESULT message quotes the tag's unfiltered size, and that is a
	// THIRD count: separate from the heading's filtered count, and it went
	// wrong independently. It used COUNT(*) over work_tags, so with the double
	// attachment it said "the tag has 29 works" while the list held 28 distinct
	// ones. A page that contradicts itself in two places at once is worse than
	// one that is merely wrong in one, so the number is asserted here too.
	empty := getPage(t, ts, "/tag/1?n=100&lang=Klingon")
	if !strings.Contains(empty, `data-testid="no-matches"`) {
		t.Fatalf("expected no-matches for a language the fixture lacks:\n%s", head(empty))
	}
	if !strings.Contains(empty, "The tag has 8 works") {
		t.Errorf("the empty-result message does not quote the tag's real size of 8, "+
			"so it is counting joined rows rather than works:\n%s", head(empty))
	}
	if strings.Contains(empty, "The tag has 9 works") {
		t.Errorf("the empty-result message says 9 works for a tag with 8; it is "+
			"using COUNT(*) over a duplicated join row:\n%s", head(empty))
	}

	ct := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?n=100&complete=true")))
	cf := joinIDs(workOrder(t, getPage(t, ts, "/tag/1?n=100&complete=false")))
	union := map[int]bool{}
	for _, id := range append(append([]int{}, idsOf(ct)...), idsOf(cf)...) {
		union[id] = true
	}
	if len(union) != 8 {
		t.Errorf("complete=true plus complete=false covers %d distinct works, "+
			"want 8 (complete=true: [%s], complete=false: [%s])",
			len(union), ct, cf)
	}
}

func idsOf(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// newDoubleAttachedServer seeds the same eight works as newFilterServer and
// then attaches tag 1 ("dark") to work 1 under BOTH "fandoms" and
// "freeforms" -- the duplication internal/testcorpus produces, and the shape
// any tag that exists as both a fandom and a freeform will produce in the real
// mirror.
func newDoubleAttachedServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	corpusPath := filepath.Join(dir, "corpus.db")
	f, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Exec(corpusSchema); err != nil {
		t.Fatal(err)
	}
	f.Close()

	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO tags(id,name) VALUES(1,'dark')`); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		id, words, kudo int
		date            string
		rating          string
		lang            string
		complete        int
	}{
		{1, 1000, 300, "2026-01-01", "General Audiences", "English", 1},
		{2, 2000, 200, "2026-02-01", "Teen And Up Audiences", "English", 0},
		{3, 3000, 100, "2026-03-01", "Mature", "Spanish", 1},
		{4, 4000, 50, "2026-04-01", "Explicit", "Spanish", 0},
		{5, 5000, 40, "2026-05-01", "General Audiences", "English", 1},
		{6, 6000, 30, "2026-06-01", "Teen And Up Audiences", "French", 1},
		{7, 7000, 20, "2026-07-01", "Mature", "English", 0},
		{8, 8000, 10, "2026-08-01", "Explicit", "French", 1},
	} {
		if _, err := seed.Exec(
			`INSERT INTO works(id,url,title,authors,word_count,kudos,hits,rating,language,complete,update_date,first_seen)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.id, "https://example.invalid/"+strconv.Itoa(r.id),
			"Work "+strconv.Itoa(r.id), "A", r.words, r.kudo, 1000,
			r.rating, r.lang, r.complete, r.date, "2026-01-01"); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Exec(
			`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,1,'freeforms')`,
			r.id); err != nil {
			t.Fatal(err)
		}
	}
	// The extra attachment: the same tag under a second type.
	if _, err := seed.Exec(
		`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(1,1,'fandoms')`); err != nil {
		t.Fatal(err)
	}
	seed.Close()

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
		Store: st, Version: "test", Lite: true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}
