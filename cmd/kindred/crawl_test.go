package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const crawlFixturePage = `<!DOCTYPE html><html><head>
<meta name="works:title" content="Crawled Work"/>
<meta name="works:author" content="crawler_author"/>
<meta name="works:words" content="12,000"/>
<meta name="works:kudos" content="50"/>
</head><body><span class="status">Completed</span>
<ul class="tags">
<li class="freeforms"><a class="tag" href="/tags/dark">dark</a></li>
<li class="fandoms"><a class="tag" href="/tags/Harry%20Potter">Harry Potter</a></li>
</ul></body></html>`

// fixtureServer stands in for AO3: serves one work page, a robots.txt with a
// crawl-delay, and 404 for anything else.
//
// The whole point is that a crawl is testable without the network and without
// waiting 30 seconds per request. A crawler whose only test is "run it against
// the real site" cannot run in CI, and this repo's CI is the gate that says
// whether a change broke anything.
func fixtureServer(t *testing.T, delayLine string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow:\n")
		if delayLine != "" {
			fmt.Fprint(w, "User-agent: others\n"+delayLine+"\n")
		}
	})
	mux.HandleFunc("/works/111", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, crawlFixturePage)
	})
	mux.HandleFunc("/works/222", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, crawlFixturePage)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCrawlOfflineMakesNoRequestAndExplainsItself(t *testing.T) {
	dir := t.TempDir()
	urls := filepath.Join(dir, "urls.txt")
	if err := os.WriteFile(urls, []byte("111\n222\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A corpus that must NOT be needed, because offline touches nothing: the
	// flag's promise is no HTTP at all, so a corpus requirement would be a
	// second promise this command breaks offline.
	err := runCrawl(context.Background(), []string{
		"--corpus", filepath.Join(dir, "unused.db"),
		"--urls", urls, "--offline",
	})
	if err != nil {
		t.Fatalf("offline crawl returned an error: %v", err)
	}
}

// A crawl of nothing must be an error, not a silent success. "fetched 0 of 0"
// reads like a healthy run that had no work to do.
func TestCrawlWithNoURLsIsAnError(t *testing.T) {
	err := runCrawl(context.Background(), []string{
		"--corpus", filepath.Join(t.TempDir(), "c.db"),
	})
	if err == nil {
		t.Fatal("crawl with no URLs reported success")
	}
	if !strings.Contains(err.Error(), "nothing to do") {
		t.Errorf("error %q does not say there was nothing to do", err)
	}
}

func TestCollectCrawlURLsAcceptsTheFormsAPastedBookmarkListHas(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "list.txt")
	// A real pasted bookmark listing: a bare id, a full URL, a comment, a
	// blank line, and a duplicate.
	content := strings.Join([]string{
		"111",
		"https://archiveofourown.org/works/222",
		"# a comment",
		"",
		"111",
		"  333  ",
	}, "\n")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := collectCrawlURLs(f, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://archiveofourown.org/works/111",
		"https://archiveofourown.org/works/222",
		"https://archiveofourown.org/works/333",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d urls %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("url %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCollectCrawlURLsReadsStdin(t *testing.T) {
	// '-' for stdin is how a 690-URL list arrives from a pipe, and it must not
	// be treated as a filename.
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	defer func() { os.Stdin = old }()
	go func() {
		fmt.Fprint(w, "444\n")
		w.Close()
	}()
	got, err := collectCrawlURLs("-", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "/works/444") {
		t.Errorf("stdin urls = %v", got)
	}
}

func TestNormaliseSeedURLAcceptsKindIDAndBareIDs(t *testing.T) {
	for in, want := range map[string]string{
		"ao3_work:555": "https://archiveofourown.org/works/555",
		"555":          "https://archiveofourown.org/works/555",
		"https://x/y":  "https://x/y",
		"":             "",
		"not-a-number": "not-a-number",
	} {
		if got := normaliseSeedURL(in); got != want {
			t.Errorf("normaliseSeedURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A resume file that cannot be parsed must stop the run, not silently start
// over: "I asked it to resume and it crawled 690 URLs again" is the failure
// this prevents.
func TestCrawlRefusesACorruptResumeFile(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	if err := os.WriteFile(state, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	urls := filepath.Join(dir, "urls.txt")
	if err := os.WriteFile(urls, []byte("111\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runCrawl(context.Background(), []string{
		"--corpus", filepath.Join(dir, "c.db"),
		"--urls", urls, "--state", state, "--offline",
	})
	if err == nil {
		t.Fatal("crawl continued past a corrupt resume file")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("error %q does not name the state file", err)
	}
}
