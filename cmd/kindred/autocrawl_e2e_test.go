package main

// End-to-end for `autocrawl` — the command the installed timer runs.
//
// Why this exists as a test of its own, and why it runs against a fixture
// rather than the live site: AO3 answers this deployment's network with a
// Cloudflare 525 — three attempts out of three on thinkcentre and three out of
// three from the machine this was written on, re-measured 2026-10-15 — so
// nothing here can fetch the real site. What that left unproven is the whole
// point of the unit: that draining the queue FETCHES, PARSES and STORES a work.
// The previous state of the world is worth stating plainly, because it is why
// the gap mattered: the drain could be observed failing per row and nothing
// else, and a crawler that fetches and cannot store looks exactly like one that
// works, right up until the mirror stops growing.
//
// The page served is the one AO3 actually sent (internal/crawl/testdata), not
// markup written to agree with the parser — the parser in this repo once
// required tags AO3 does not send and stayed green for weeks against a fixture
// that invented them.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func TestAutoCrawlDrainsTheQueueIntoTheMirror(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "internal", "crawl", "testdata", "ao3_work_page.html"))
	if err != nil {
		t.Fatalf("the captured AO3 page: %v", err)
	}

	dir := t.TempDir()
	corpusPath := filepath.Join(dir, "corpus.db")
	statePath := filepath.Join(dir, "state.db")

	// The mirror, from the same corpus DDL the discover e2e uses. The schema is
	// deliberately not `IF NOT EXISTS`: a fixture that silently reuses an
	// existing file tests nothing.
	cdb, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cdb.Exec(testcorpus.Schema); err != nil {
		t.Fatalf("corpus schema: %v", err)
	}
	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	var fetches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			// One second, never zero: LoadRobotsDelay refuses to return 0 and
			// falls back to the 30s site default, and a test that sat through
			// the real delay is a test somebody deletes.
			_, _ = w.Write([]byte("User-agent: *\nCrawl-delay: 1\n"))
		case strings.HasPrefix(r.URL.Path, "/works/"):
			atomic.AddInt32(&fetches, 1)
			_, _ = w.Write(page)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()

	// A queue row is what a reader's 404 leaves behind, so it is seeded through
	// the store rather than by hand — the drain's input is the store's shape.
	s, err := store.Open(ctx, statePath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueCrawl(ctx, "https://archiveofourown.org/works/1", "test"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The command the timer runs, with only the base URL redirected.
	if err := runAutoCrawlCmd(ctx, []string{
		"--corpus", corpusPath,
		"--db", statePath,
		"--base-url", srv.URL,
		"--max", "1",
	}); err != nil {
		t.Fatalf("runAutoCrawlCmd: %v", err)
	}

	if n := atomic.LoadInt32(&fetches); n == 0 {
		t.Fatal("the fixture was never fetched: the drain never reached the network path")
	}

	s2, err := store.Open(ctx, statePath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	// These are the traps the captured page carries, not just "a row appeared".
	// Every one of them is a bug this repo has already paid for once: bookmarks
	// lives inside an <a> and parses as 0 without cleanText — and 0 is a real
	// value, so that failure is invisible in the data as well as in the tests;
	// words and hits carry thousands separators; and "55/?" must NOT read as
	// complete, because a mirror that calls a WIP complete stops re-checking it.
	var (
		title     string
		words     int
		kudos     int
		bookmarks sql.NullInt64
		hits      int
		complete  int
	)
	// Read the MIRROR, not the state database: store.Open's DB handle is the
	// state DB, and the corpus is opened separately (read-only for serve,
	// read-write only inside a crawl). Reading the wrong one is how this test
	// first failed — "no such table: works" from a store whose write had just
	// logged "mirrored a work".
	mdb, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	if err := mdb.QueryRowContext(ctx,
		`SELECT title, word_count, kudos, bookmarks, hits, complete FROM works WHERE id = 1`,
	).Scan(&title, &words, &kudos, &bookmarks, &hits, &complete); err != nil {
		t.Fatalf("the drained work is not in the mirror: %v", err)
	}

	if want := "Highschool DxD : I'm Motohama with Gacha System"; title != want {
		t.Errorf("title = %q, want %q (the markup pads it with whitespace)", title, want)
	}
	if words != 155360 {
		t.Errorf("word_count = %d, want 155360: a comma'd stat must parse whole, not as its first field", words)
	}
	if hits != 30546 {
		t.Errorf("hits = %d, want 30546", hits)
	}
	if kudos != 308 {
		t.Errorf("kudos = %d, want 308", kudos)
	}
	if !bookmarks.Valid || bookmarks.Int64 != 122 {
		t.Errorf("bookmarks = %v, want 122: the value sits inside an <a>, and 0 is a real value, "+
			"so this is the one stat whose parse failure nothing downstream can notice", bookmarks)
	}
	if complete != 0 {
		t.Errorf("complete = %d, want 0: the page says 55/?, and completion fails closed", complete)
	}

	var tags int
	if err := mdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_tags WHERE work_id = 1`).Scan(&tags); err != nil {
		t.Fatal(err)
	}
	if tags == 0 {
		t.Error("the work landed with no tag rows: the tag vocabulary did not reach work_tags, " +
			"which the engine's fandom filter reads")
	}

	// And the row is closed, or the next tick fetches the same work forever.
	st, err := s2.CrawlQueueStats(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 0 || st.Done != 1 {
		t.Errorf("queue after the drain: pending=%d done=%d, want 0/1", st.Pending, st.Done)
	}
}
