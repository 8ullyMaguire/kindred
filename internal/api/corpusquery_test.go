package api

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// queryCorpus boots a live server over a fixture the corpus queries can
// actually rank.
//
// It carries tags with real document-frequency spread: the queries gate on
// evidence, so a fixture where every tag sits on every work makes "nothing
// cleared the gate" the only reachable answer, and a test asserting on an
// empty table would then prove nothing.
func queryCorpus(t *testing.T) *httptest.Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cq.db")

	f := &testcorpus.Corpus{
		Tags: []testcorpus.Tag{
			{ID: 1, Name: "shared"},
			{ID: 2, Name: "bright"},
		},
	}
	// Works 1-6 carry both tags, 7-9 carry none. The split is what lets a
	// gate bite on one group and not the other.
	for i := int64(1); i <= 9; i++ {
		f.Works = append(f.Works, testcorpus.Work{
			ID: i, Title: "work " + strconv.FormatInt(i, 10), Authors: "author",
			WordCount: 20000, Kudos: 100, Hits: 1000,
			Rating: "General Audiences", Language: "English", Complete: true,
		})
		if i <= 6 {
			for _, tag := range []int64{1, 2} {
				f.WorkTags = append(f.WorkTags, testcorpus.WorkTag{
					WorkID: i, TagID: tag, TagType: "freeform"})
			}
		}
	}

	written, err := f.Write(path)
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db, err := sql.Open("sqlite", written)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv := &Server{
		Engine:    &engine.Engine{Corpus: corpus.NewAO3(db)},
		Version:   "test",
		StartedAt: time.Now().Add(-time.Minute),
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// cqGet issues a GET against a live server and returns the status, the
// decoded body when the status is 200, and the raw text always.
//
// The raw text matters: several assertions here are about what an ERROR says
// — that it names the parameter, that it lists the alternatives — and a
// decoded map would throw exactly that away.
func cqGet(t *testing.T, ts *httptest.Server, path string) (int, map[string]any, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var body map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: body is not JSON: %v\n%s", path, err, raw)
		}
	}
	return resp.StatusCode, body, string(raw)
}

// cqAssertErr checks a refusal: the status, and that the message NAMES the
// parameter the caller got wrong.
//
// The second half is the part that matters. A 400 saying "bad request" tells
// a caller nothing they can act on when the request carried six filters, and
// on a page with six controls that is the difference between a fixable
// mistake and a dead end.
func cqAssertErr(t *testing.T, code int, raw string, wantCode int, mustMention string) {
	t.Helper()
	if code != wantCode {
		t.Fatalf("got %d, want %d: %s", code, wantCode, raw)
	}
	if mustMention != "" && !strings.Contains(raw, mustMention) {
		t.Fatalf("the error does not mention %q: %s", mustMention, raw)
	}
}

// TestEveryWorkingCorpusQueryModeIsReachableOverHTTP is the parity claim.
//
// These modes existed as pages and as a CLI verb. A script that wanted one
// had to scrape a rendered table, and the CLI path required a writable store
// this API does not. Each mode here must answer 200 through the API.
func TestEveryWorkingCorpusQueryModeIsReachableOverHTTP(t *testing.T) {
	ts := queryCorpus(t)

	paths := map[string]string{
		string(corpusquery.ModeFandomRanking): "/api/v1/corpus-query/fandom-ranking?limit=10",
		string(corpusquery.ModeUnderrated):    "/api/v1/corpus-query/underrated?limit=10",
		string(corpusquery.ModeTagNeighbours): "/api/v1/corpus-query/tag-neighbours?tag=shared&limit=10",
		string(corpusquery.ModeSurprise):      "/api/v1/corpus-query/surprise?limit=10",
	}
	// Every IMPLEMENTED mode must have a path, so adding a mode without one
	// fails here rather than being discovered by a caller.
	if len(paths) != len(corpusquery.Modes()) {
		t.Fatalf("the test covers %d modes but corpusquery has %d implemented: %v",
			len(paths), len(corpusquery.Modes()), corpusquery.Modes())
	}

	for mode, path := range paths {
		t.Run(mode, func(t *testing.T) {
			code, body, raw := cqGet(t, ts, path)
			if code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200: %s", path, code, raw)
			}
			if got := body["mode"]; got != mode {
				t.Fatalf("response mode = %v, want %q: the handler answered a "+
					"different mode than was asked for", got, mode)
			}
			if _, ok := body["rows"]; !ok {
				t.Fatalf("response has no rows key: %v", body)
			}
		})
	}
}

// TestADeclaredButUnimplementedModeAnswers501 is the honesty test.
//
// `similar` and `fandom-landscape` are DECLARED names in corpusquery. An
// endpoint answering them with an empty row set would be indistinguishable
// from a corpus containing nothing — a far stronger and wrong claim than
// "not implemented".
func TestADeclaredButUnimplementedModeAnswers501(t *testing.T) {
	ts := queryCorpus(t)

	for _, m := range corpusquery.DeclaredModes() {
		if corpusquery.IsImplemented(m) {
			continue
		}
		code, _, raw := cqGet(t, ts, "/api/v1/corpus-query/"+string(m))
		cqAssertErr(t, code, raw, http.StatusNotImplemented,
			string(corpusquery.ModeUnderrated))
	}
}

// TestAnUnknownModeNamesTheOnesThatWork pins the 400.
func TestAnUnknownModeNamesTheOnesThatWork(t *testing.T) {
	ts := queryCorpus(t)

	code, _, raw := cqGet(t, ts, "/api/v1/corpus-query/nonsense")
	cqAssertErr(t, code, raw, http.StatusBadRequest, "fandom-ranking")
}

// TestTagNeighboursRequiresATag pins the required parameter, and that its
// absence is named rather than silently returning everything.
func TestTagNeighboursRequiresATag(t *testing.T) {
	ts := queryCorpus(t)

	code, _, raw := cqGet(t, ts, "/api/v1/corpus-query/tag-neighbours")
	cqAssertErr(t, code, raw, http.StatusBadRequest, "tag")
}

// TestTheLimitIsBounded pins that a caller cannot ask for an unbounded run.
//
// ?limit=1000000 on the real mirror is not a slow request, it is a request
// that takes the process down. Out-of-range falls back to the default rather
// than erroring, because a script leaving `limit=` unset or templated-empty
// is normal and refusing the whole call would be pedantic.
func TestTheLimitIsBounded(t *testing.T) {
	ts := queryCorpus(t)

	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"5", 5},
		{"999999999", 0}, // out of range -> the default
		{"-3", 0},        // negative -> the default
		{"not-a-number", 0},
	} {
		q := url.Values{"limit": {tc.raw}}
		code, _, raw := cqGet(t, ts, "/api/v1/corpus-query/underrated?"+q.Encode())
		if code != http.StatusOK {
			t.Fatalf("limit=%q answered %d, want 200: %s", tc.raw, code, raw)
		}
		if got := parseAPIBoundedInt(tc.raw, 0, 1000, 0); got != tc.want {
			t.Fatalf("parseAPIBoundedInt(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestSeedTagsMustBeIDs pins that a malformed seed list is refused by name,
// not silently ignored — an ignored seed list turns a personalised ranking
// into a corpus-wide one that looks exactly like a working answer.
func TestSeedTagsMustBeIDs(t *testing.T) {
	ts := queryCorpus(t)

	code, _, raw := cqGet(t, ts, "/api/v1/corpus-query/fandom-ranking?seed_tags=shared")
	cqAssertErr(t, code, raw, http.StatusBadRequest, "seed_tags")

	// And a well-formed one is accepted, so the test is not simply rejecting
	// everything.
	code, _, raw = cqGet(t, ts, "/api/v1/corpus-query/fandom-ranking?seed_tags=1,2")
	if code != http.StatusOK {
		t.Fatalf("valid seed_tags answered %d, want 200: %s", code, raw)
	}
}

// TestMinCoWorksIsSigned pins that a negative value is honoured, because
// disabling the evidence gate is a decision and not an accident.
func TestMinCoWorksIsSigned(t *testing.T) {
	ts := queryCorpus(t)

	code, _, raw := cqGet(t, ts, "/api/v1/corpus-query/fandom-ranking?min_co_works=-1")
	if code != http.StatusOK {
		t.Fatalf("a negative min_co_works answered %d, want 200: %s", code, raw)
	}
	code, _, raw = cqGet(t, ts, "/api/v1/corpus-query/fandom-ranking?min_co_works=lots")
	cqAssertErr(t, code, raw, http.StatusBadRequest, "min_co_works")
}

// TestTheEndpointNeedsACorpus pins the 503 rather than a panic. A server
// mounted with no corpus has to say so; answering 200 with zero rows would
// claim the corpus is empty.
func TestTheEndpointNeedsACorpus(t *testing.T) {
	srv := &Server{Version: "test", StartedAt: time.Now().Add(-time.Minute)}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	code, _, raw := cqGet(t, ts, "/api/v1/corpus-query/underrated")
	cqAssertErr(t, code, raw, http.StatusServiceUnavailable, "corpus")
}

// TestTheModeIsDispatchedNotGuessed pins that the requested mode reaches the
// right Runner method. A handler that ran every mode and returned the first
// non-empty result would answer 200 for all four.
func TestTheModeIsDispatchedNotGuessed(t *testing.T) {
	ts := queryCorpus(t)

	for _, m := range corpusquery.Modes() {
		path := "/api/v1/corpus-query/" + string(m)
		if m == corpusquery.ModeTagNeighbours {
			path += "?tag=shared"
		}
		code, body, raw := cqGet(t, ts, path)
		if code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", m, code, raw)
		}
		if got := body["mode"]; got != string(m) {
			t.Fatalf("asked for %q, response says %q: the handler did not "+
				"dispatch on the mode", m, got)
		}
	}
}
