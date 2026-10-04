package api

// SPEC §3.2.2: "Every response carries X-Kindred-Index-Age and
// X-Kindred-Index-Version, so a stale mirror is visible to the client."
//
// The header was named in SPEC.md, in PLAN.md §5.1's verification steps
// (TestIndexAgeHeader), and in a comment in cmd/kindred/ingest.go -- and was
// set nowhere. `grep -rn X-Kindred --include=*.go` returned exactly one hit:
// the comment. This file is that missing TestIndexAgeHeader.
//
// What it pins, and why each part is asserted the way it is:
//
//   - EVERY route, not a sample. A middleware that is wired onto one
//     handler is a middleware that is missing from the handler added next
//     year. The list below is deliberately wider than the routes that
//     "should" have it.
//   - Including the failure paths. A 404 and a 500 are responses too; a
//     client deciding whether to trust an error message needs the same
//     context.
//   - "unknown" rather than "0s" when no stamp exists. A zero age claims
//     the index was built just now, and it is the single most wrong claim a
//     client could accept.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// headerRoutes is every route worth checking, including the ones that fail.
//
// The 404 and the malformed-seed cases are in here on purpose: a header
// middleware wrapped INSIDE recovery would stamp successful responses and
// miss the ones the panic handler produced, which is a bug you only find by
// asking for a response nobody thought to stamp.
var headerRoutes = []string{
	"/healthz",
	"/stats",
	"/api/v1/stats",
	"/api/v1/ao3/works",
	"/api/v1/ao3/tags",
	"/api/v1/recommend?seed=ao3_work:1&n=5",
	"/api/v1/tags/1/similar",
	"/api/v1/arena/leaderboard",
	// Failure paths.
	"/api/v1/nonsense",                          // 404 JSON
	"/api/v1/recommend?seed=ao3_work:999999999", // 404 no such entity
	"/api/v1/recommend?seed=book:1",             // 400 unsupported kind
	"/",                                         // HTML home page
	"/search?q=dark",
	"/work/1",
	"/arena",
}

// TestIndexAgeHeaderIsOnEveryResponse is the gate for SPEC §3.2.2.
func TestIndexAgeHeaderIsOnEveryResponse(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range headerRoutes {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			age := resp.Header.Get("X-Kindred-Index-Age")
			if age == "" {
				t.Errorf("GET %s (status %d) has no X-Kindred-Index-Age; "+
					"SPEC 3.2.2 requires it on EVERY response", path, resp.StatusCode)
			}
			version := resp.Header.Get("X-Kindred-Index-Version")
			if version == "" {
				t.Errorf("GET %s (status %d) has no X-Kindred-Index-Version",
					path, resp.StatusCode)
			}
			// SPEC §3.2.3: the response must say what it is. Go's net/http
			// omits Server entirely by default, so this was absent from every
			// response in the deployment.
			if srv := resp.Header.Get("Server"); srv != "kindred" {
				t.Errorf("GET %s (status %d) has Server %q, want %q "+
					"(SPEC 3.2.3)", path, resp.StatusCode, srv, "kindred")
			}
			// The machine-readable twin must agree with the human one.
			// Two representations of one fact drift if nothing checks.
			if age != "" {
				secs := resp.Header.Get("X-Kindred-Index-Age-Seconds")
				if age == "unknown" {
					if secs != "0" {
						t.Errorf("age is %q but seconds is %q; an unknown age "+
							"must not look like a measured zero", age, secs)
					}
				} else if _, err := time.ParseDuration(age); err != nil {
					t.Errorf("X-Kindred-Index-Age %q is not a Go duration, so "+
						"a client cannot parse it: %v", age, err)
				} else if secs == "" {
					t.Errorf("age is %q but X-Kindred-Index-Age-Seconds is empty",
						age)
				}
			}
		})
	}
}

// TestIndexAgeIsUnknownNotZeroWhenNoStampExists is the honesty clause.
//
// A zero age is a claim: it says the index was built moments ago. A server
// that has never been ingested does not know that, and saying 0s is the one
// value a client would be most wrong to act on.
func TestIndexAgeIsUnknownNotZeroWhenNoStampExists(t *testing.T) {
	ts := newTestServer(t) // the fixture store never runs ingest
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	age := resp.Header.Get("X-Kindred-Index-Age")
	if age != "unknown" {
		t.Errorf("X-Kindred-Index-Age = %q on a store with no index_built_at, "+
			"want %q", age, "unknown")
	}
	if secs := resp.Header.Get("X-Kindred-Index-Age-Seconds"); secs != "0" {
		t.Errorf("X-Kindred-Index-Age-Seconds = %q for an unknown age, want 0 "+
			"(0 seconds of MEASURED age, which is not the same as unknown)", secs)
	}
}

// TestIndexAgeReflectsTheRecordedStamp is the positive case: a store WITH a
// stamp reports a real age, and the age is roughly right.
func TestIndexAgeReflectsTheRecordedStamp(t *testing.T) {
	ts, s := newTestServerWithStore(t)
	threeDaysAgo := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	if err := s.SetMeta(context.Background(), "index_built_at", threeDaysAgo); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	age := resp.Header.Get("X-Kindred-Index-Age")
	d, err := time.ParseDuration(age)
	if err != nil {
		t.Fatalf("X-Kindred-Index-Age = %q, not a duration: %v", age, err)
	}
	// Three days, within a minute of slack for the request's own latency and
	// for the stamp being written at second resolution.
	if diff := d - 72*time.Hour; diff > time.Minute || diff < -time.Minute {
		t.Errorf("age %v does not match a stamp written 72h ago", d)
	}

	// And the seconds header must agree with it.
	wantSecs := int64(d.Seconds())
	gotSecs := resp.Header.Get("X-Kindred-Index-Age-Seconds")
	if gotSecs != "" && gotSecs != strconv.FormatInt(wantSecs, 10) {
		t.Errorf("seconds header %q disagrees with age %v (%d seconds)",
			gotSecs, d, wantSecs)
	}

	// The version falls back to the stamp's date when no explicit version
	// key exists, so it changes when the index is rebuilt.
	if v := resp.Header.Get("X-Kindred-Index-Version"); v == "" || v == "unknown" {
		t.Errorf("X-Kindred-Index-Version = %q, want the build date derived "+
			"from the stamp rather than 'unknown'", v)
	}
}

// TestAFutureStampIsReportedAsNegative checks the clock-disagreement case is
// visible rather than silently clamped to zero.
func TestAFutureStampIsReportedAsNegative(t *testing.T) {
	ts, s := newTestServerWithStore(t)
	in48h := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if err := s.SetMeta(context.Background(), "index_built_at", in48h); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	age := resp.Header.Get("X-Kindred-Index-Age")
	d, err := time.ParseDuration(age)
	if err != nil {
		t.Fatalf("age %q not a duration: %v", age, err)
	}
	if d >= 0 {
		t.Errorf("a build stamp 48h in the future reported as age %v; the "+
			"clock disagreement must be visible, not clamped", d)
	}
}

// TestAnUnparseableStampIsUnknownNotZero is the same honesty clause for a
// corrupt value.
func TestAnUnparseableStampIsUnknownNotZero(t *testing.T) {
	ts, s := newTestServerWithStore(t)
	if err := s.SetMeta(context.Background(), "index_built_at", "last tuesday"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if age := resp.Header.Get("X-Kindred-Index-Age"); age != "unknown" {
		t.Errorf("an unparseable stamp reported age %q, want \"unknown\"; a "+
			"garbage value must not become a confident number", age)
	}
}

// TestAgeHeaderStringNeverInventsAnAge drives the surviving implementation.
//
// This test used to cover api.ageSeconds(string), a helper that existed only
// in the API layer's copy of the header logic. That copy turned out to be
// dead -- web.withFreshnessHeaders already stamps every response because it
// wraps the HTML frontend, the outermost handler -- so the API copy was
// deleted rather than kept "just in case". This test follows the code.
//
// It asserts through HTTP rather than by calling a helper, because a helper
// test passes even when the helper is not the one wired in.
func TestAgeHeaderStringNeverInventsAnAge(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// No stamp in this fixture, so unknown/0 -- and specifically NOT a
	// duration that would parse as a real measurement.
	if age := resp.Header.Get("X-Kindred-Index-Age"); age != "unknown" {
		t.Errorf("age %q on a store with no stamp; want unknown", age)
	}
	if secs := resp.Header.Get("X-Kindred-Index-Age-Seconds"); secs != "0" {
		t.Errorf("seconds %q for an unknown age, want 0", secs)
	}
}

// newTestServerWithStore exposes the store so a test can write a build stamp.
func newTestServerWithStore(t *testing.T) (*httptest.Server, *store.Store) {
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
	s, err := store.Open(context.Background(), dbPath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(
		`INSERT INTO works(id,url,title,word_count,hits,kudos,update_date,first_seen)
		 VALUES(1,'https://example.invalid/1','Work 1',5000,1000,100,'2026-06-01','2026-06-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO tags(id,name) VALUES(1,'dark')`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(
		`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(1,1,'freeforms')`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		Engine:    &engine.Engine{Store: s, Corpus: corpus.NewAO3(s.Corpus), PoolSize: 50, TopN: 4, Lite: true},
		Store:     s,
		Version:   "test",
		StartedAt: time.Now().Add(-time.Minute),
		Lite:      true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, s
}
