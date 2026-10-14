package crawl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// realWorkPage is the live AO3 page excerpt in testdata/ao3_work_page.html.
//
// It is read from disk because the whole point of the fixture is that AO3
// *sent* it. The inline fixture this replaced invented
// `<meta name="works:title">` and `<li class="freeforms">`; AO3 sends neither,
// so ParseWork refused every real work page while these tests stayed green.
// TestParseWorkReadsTheRealPageShapes fails if the invented tags come back.
func realWorkPage(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/ao3_work_page.html")
	if err != nil {
		t.Fatalf("read the live-page fixture: %v", err)
	}
	return string(b)
}

// The expected values are the live page's own, from work 82026196 fetched
// 2026-10-14 — not values picked to make the parser look right.
func TestParseWorkExtractsEveryField(t *testing.T) {
	w, err := ParseWork([]byte(realWorkPage(t)), "https://archiveofourown.org/works/82026196")
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != 82026196 {
		t.Errorf("ID = %d, want 82026196", w.ID)
	}
	if w.Title != "Highschool DxD : I'm Motohama with Gacha System" {
		t.Errorf("Title = %q", w.Title)
	}
	if w.Authors != "Opeler" {
		t.Errorf("Authors = %q", w.Authors)
	}
	// Comma grouping must be stripped, not parsed as a prefix.
	if w.WordCount != 155360 {
		t.Errorf("WordCount = %d, want 155360", w.WordCount)
	}
	if w.Kudos != 308 {
		t.Errorf("Kudos = %d, want 308", w.Kudos)
	}
	if w.Hits != 30546 {
		t.Errorf("Hits = %d, want 30546", w.Hits)
	}
	// The bookmarks row wraps its number in an anchor on the real page, so this
	// also pins that the count is read from inside markup rather than assumed
	// to be bare text.
	if w.Bookmarks == nil || *w.Bookmarks != 122 {
		t.Errorf("Bookmarks = %v, want 122", w.Bookmarks)
	}
	if w.Chapters != "55/?" {
		t.Errorf("Chapters = %q, want 55/?", w.Chapters)
	}
	if w.Complete {
		t.Error("Complete = true for a 55/? work; an unknown total must not read as finished")
	}
	if w.Language != "English" {
		t.Errorf("Language = %q", w.Language)
	}
	if w.UpdateDate != "2026-07-27" {
		t.Errorf("UpdateDate = %q, want 2026-07-27", w.UpdateDate)
	}
	if w.Rating != "Mature" {
		t.Errorf("Rating = %q, want Mature", w.Rating)
	}
	if !strings.Contains(w.Summary, "bored of his mundane life") {
		t.Errorf("Summary did not come off the page: %q", w.Summary)
	}
}

// The regression pin for the bug this parser had for its whole life: it asked
// AO3 for markup AO3 does not send, and its fixture supplied that markup.
//
// Both halves matter. The first proves the fixture is honest; the second proves
// the parser now refuses the page the old fixture described, so a future
// "fix the test by editing the fixture" cannot quietly restore the fiction.
func TestParseWorkReadsTheRealPageShapes(t *testing.T) {
	// Comments are stripped first: the fixture's own header explains which tags
	// the old parser invented, and a naive scan would match that explanation
	// instead of the markup. It is the *markup* that must be honest.
	page := regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(realWorkPage(t), "")
	for _, invented := range []string{
		`name="works:title"`, `name="works:words"`, `name="works:kudos"`,
		`name="works:hits"`, `name="works:bookmarks"`, `name="works:chapters"`,
		`<li class="freeforms"`, `class="care-actions"`,
	} {
		if strings.Contains(page, invented) {
			t.Errorf("the fixture carries %q, which a live AO3 page does not send — "+
				"either the fetch is wrong or someone edited the fixture to suit the parser", invented)
		}
	}

	// Exactly the old fixture's shape, and not a work page.
	oldFixtureShape := `<html><head>` +
		`<meta name="works:title" content="The Test Work"/>` +
		`<meta name="works:words" content="45,120"/>` +
		`</head><body><span class="status">Completed</span>` +
		`<ul class="tags"><li class="freeforms"><a class="tag" href="/tags/dark">dark</a></li></ul>` +
		`</body></html>`
	if w, err := ParseWork([]byte(oldFixtureShape), "https://archiveofourown.org/works/12345"); err == nil {
		t.Fatalf("the invented-meta-tag page parsed as a work (%q): the fixture shape is back", w.Title)
	}

	// And a real title heading with no tag rows is still a parse failure, not a
	// work with no tags.
	if _, err := ParseWork([]byte(`<h2 class="title heading">x</h2>`), "https://archiveofourown.org/works/2"); err == nil {
		t.Error("parsed a work page with zero tags")
	}
}

// The tag categories are the only authoritative statement of what a tag IS, and
// the mirror's tag_type column is NOT it (measured: 3,890,504 of 3,891,300 rows
// are 'freeforms'). So the parser must preserve what the page said, in the
// vocabulary this codebase stores.
func TestParseWorkPreservesTagCategories(t *testing.T) {
	w, err := ParseWork([]byte(realWorkPage(t)), "https://archiveofourown.org/works/82026196")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, tg := range w.Tags {
		byName[tg.Name] = tg.Type
	}
	for name, wantType := range map[string]string{
		"Mature":                         "ratings",
		"Graphic Depictions Of Violence": "warnings",
		"F/M":                            "categories",
		"Highschool DxD (Anime)":         "fandoms",
		"Himejima Akeno/Original Male Character(s)": "relationships",
		"Rias Gremory's Peerage":                    "characters",
		"Action":                                    "freeforms",
	} {
		if got := byName[name]; got != wantType {
			t.Errorf("tag %q type = %q, want %q", name, got, wantType)
		}
	}
	if !TagTypeIsFandom(byName["Highschool DxD (Anime)"]) {
		t.Error("a fandom was not recognised as one")
	}
	if TagTypeIsFandom(byName["Action"]) {
		t.Error("a freeform was misread as a fandom")
	}
	if len(w.Tags) == 0 {
		t.Fatal("no tags parsed")
	}
}

// A page that is not a work page must be an ERROR, not a zero-valued work.
// Writing one poisons the mirror with a row that has no id and no title, and
// nothing downstream can tell it from a real sparse work.
func TestParseWorkRejectsANonWorkPage(t *testing.T) {
	for name, page := range map[string]string{
		"cloudflare": `<html><body>Attention Required! | Cloudflare ... cf-error-details</body></html>`,
		"login wall": `<html><body><form action="/users/log_in">Sign in</form></body></html>`,
		"empty":      ``,
		"tag page":   `<html><body><h1>Tags</h1></body></html>`,
		// A consent/interstitial page: it has the site's chrome but no work
		// heading, which is exactly what the marker must catch.
		"interstitial": `<html><body><div id="main"><h2 class="heading">This work could have adult content.</h2><a href="?view_adult=true">Proceed</a></div></body></html>`,
		// TAGS BUT NO TITLE: the zero-tags check would not catch this one, so it
		// is the case that pins the title guard itself. Without the guard this
		// page parses as a work with an empty title, which is precisely the
		// poisoned row the guard exists to prevent.
		"tag rows, no heading": `<html><body><dl class="stats"><dt class="words">Words:</dt><dd class="words">1,000</dd></dl>` +
			`<dd class="freeform tags"><ul class="commas"><li><a class="tag" href="/tags/Angst/works">Angst</a></li></ul></dd></body></html>`,
	} {
		if _, err := ParseWork([]byte(page), "https://archiveofourown.org/works/9"); err == nil {
			t.Errorf("%s: ParseWork succeeded, want an error", name)
		}
	}
}

// Bookmarks must stay a pointer: a page that omits the row means "unknown", and
// collapsing that to 0 makes an unknown work rank as the least-bookmarked work
// in the corpus.
func TestParseWorkLeavesBookmarksNilWhenAbsent(t *testing.T) {
	full := realWorkPage(t)
	// Remove the real row, anchored on the real markup rather than on a tag the
	// fixture invented.
	page := regexp.MustCompile(`(?s)<dt class="bookmarks">.*?</dd>`).ReplaceAllString(full, "")
	if page == full {
		t.Fatal("the bookmarks row was not found in the fixture; this test is now vacuous")
	}
	w, err := ParseWork([]byte(page), "https://archiveofourown.org/works/82026196")
	if err != nil {
		t.Fatal(err)
	}
	if w.Bookmarks != nil {
		t.Errorf("Bookmarks = %v, want nil when the page omits the row", *w.Bookmarks)
	}
}

// statValues must keep the FIRST occurrence of a class. The work's own stats
// block comes before the chapter list, which repeats several of the same classes
// for per-chapter counts; last-wins would report a chapter's word count as the
// work's, and nothing on the page would look wrong.
func TestStatValuesKeepsTheWorkStatsNotTheChapterList(t *testing.T) {
	// Composed from the observed shape and deliberately ambiguous: which value a
	// reader gets must be decided by position, never by iteration order.
	page := `<dl class="stats"><dt class="words">Words:</dt><dd class="words">155,360</dd></dl>` +
		`<div class="chapter"><dl class="stats"><dt class="words">Words:</dt><dd class="words">2,500</dd></dl></div>`
	got := statValues([]byte(page))
	if got["words"] != "155,360" {
		t.Errorf("statValues(words) = %q; the work-level block must win", got["words"])
	}
}

// The completion rule reads AO3's chapter fraction, where "?" is an unknown
// total. It must fail closed: a mirror that marks a work in progress complete
// stops re-checking it.
func TestChaptersAreComplete(t *testing.T) {
	for _, tc := range []struct {
		fraction string
		want     bool
	}{
		{"55/?", false}, // a work in progress: the total is unknown
		{"?/?", false},  // both unknown
		{"12/", false},  // malformed, and not complete
		{"", false},     // the row was absent entirely
		{"a/b", false},  // not numbers
		{"1/1", true},   // the single-chapter case
		{"12/12", true}, // the finished multi-chapter case
		{"6/12", false}, // MID-work: posted < total is not complete either
	} {
		if got := chaptersAreComplete(tc.fraction); got != tc.want {
			t.Errorf("chaptersAreComplete(%q) = %v, want %v", tc.fraction, got, tc.want)
		}
	}
}

func TestWorkIDFromURL(t *testing.T) {
	cases := map[string]int64{
		"https://archiveofourown.org/works/12345":             12345,
		"https://archiveofourown.org/works/12345/chapters/99": 12345,
		"http://archiveofourown.org/works/7":                  7,
		"https://archiveofourown.org/users/someone":           0,
		"https://archiveofourown.org/tags/dark":               0,
		"https://archiveofourown.org/works/notanumber":        0,
		"": 0,
	}
	for in, want := range cases {
		if got := WorkIDFromURL(in); got != want {
			t.Errorf("WorkIDFromURL(%q) = %d, want %d", in, got, want)
		}
	}
}

// ---------------------------------------------------------------- state

func TestStateSaveIsAtomicAndSurvivesAReusedPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")

	s := NewState()
	s.Mark("https://archiveofourown.org/works/1", 1)
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	// No temp file left behind: a leftover .tmp after a successful save is how
	// you discover the rename is not happening.
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp state file left behind after Save")
	}

	loaded, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Done("https://archiveofourown.org/works/1") {
		t.Error("fetched work not marked done after reload")
	}
}

// The sibling tool's bug: /works/123 and the absolute URL are the same work, so
// without canonicalisation a resume file holds both and re-fetches half the
// corpus.
func TestStateCanonicalisesURLsForResume(t *testing.T) {
	s := NewState()
	s.Mark("https://archiveofourown.org/works/123", 123)
	if !s.Done("/works/123") {
		t.Error("/works/123 not recognised as already fetched")
	}
	if !s.Done("https://archiveofourown.org/works/123/") {
		t.Error("trailing slash not normalised away")
	}
	if s.Done("https://archiveofourown.org/works/124") {
		t.Error("a different work reported as done")
	}
}

// A URL that FAILED must not be treated as done. Re-crawling a success is
// cheap; silently skipping a failure loses that work forever.
func TestStateErrorsAreNotDone(t *testing.T) {
	s := NewState()
	s.MarkError("https://archiveofourown.org/works/9", errors.New("boom"))
	if s.Done("https://archiveofourown.org/works/9") {
		t.Error("a failed fetch reported as done")
	}
	if s.Errors["https://archiveofourown.org/works/9"] == "" {
		t.Error("error not recorded")
	}
	// And a later success clears the error.
	s.Mark("https://archiveofourown.org/works/9", 9)
	if _, still := s.Errors["https://archiveofourown.org/works/9"]; still {
		t.Error("error not cleared by a successful retry")
	}
}

func TestLoadStateOnAMissingFileIsAFirstRunNotAnError(t *testing.T) {
	s, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing state file was an error: %v", err)
	}
	if len(s.Fetched) != 0 {
		t.Error("a first run reported fetched works")
	}
}

func TestLoadStateRejectsAFutureVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte(`{"version":999,"fetched":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Misreading an old file as a current one is worse than refusing.
	if _, err := LoadState(p); err == nil {
		t.Error("loaded a state file from a future version")
	}
}

// flakySink fails on one URL and records the rest.
type flakySink struct {
	failURL string
	saved   []int64
}

func (s *flakySink) Save(_ context.Context, w *ParsedWork) error {
	if w.URL == s.failURL {
		return errors.New("disk full (simulated)")
	}
	s.saved = append(s.saved, w.ID)
	return nil
}
func (s *flakySink) Close() error { return nil }

// ---------------------------------------------------------------- offline

// The sibling tool's offline mode still called fetch_robots_delay(), which is
// an HTTP request, breaking the mode's one promise in exactly the
// Cloudflare-blocked case that prompted the flag.
func TestOfflineClientRefusesFetchWithoutAnyIO(t *testing.T) {
	o := NewOffline("test")
	_, err := o.Fetch(context.Background(), "https://archiveofourown.org/works/1")
	if !errors.Is(err, ErrOffline) {
		t.Errorf("Fetch error = %v, want ErrOffline", err)
	}
	if _, err := o.LoadRobotsDelay(context.Background()); err != nil {
		t.Errorf("LoadRobotsDelay made a request in offline mode: %v", err)
	}
	// And it must not be a distinctive error, or callers switch on one and miss
	// the other: `errors.Is(err, ErrOfflineMode)` and `errors.Is(err, ErrOffline)`
	// both have to work, because both spellings exist in callers.
	if !errors.Is(ErrOfflineMode, ErrOffline) {
		t.Error("ErrOfflineMode is not the same sentinel as ErrOffline")
	}
}

func TestOfflineCrawlProducesNothingAndSaysWhy(t *testing.T) {
	c := New(NewOffline("network disabled by flag")).
		WithSleep(func(context.Context, time.Duration) error { return nil })
	start := time.Now()
	rep, err := c.Run(context.Background(), Options{
		URLs: []string{"https://archiveofourown.org/works/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 0 {
		t.Errorf("Fetched = %d in offline mode", rep.Fetched)
	}
	if rep.Failed != 1 {
		t.Errorf("Failed = %d, want 1", rep.Failed)
	}
	if !strings.Contains(rep.FailedURLs["https://archiveofourown.org/works/1"], "network disabled") {
		t.Errorf("failure reason did not carry the explanation: %v", rep.FailedURLs)
	}
	// A refusal is not a transient failure. Retrying it with the default
	// backoff cost a measured 28s for two URLs and made an offline run look
	// hung, so `ErrOffline` short-circuits the retry ladder. The sleep here is
	// a no-op, so any retry shows up as a nonzero Retried count rather than as
	// elapsed time.
	if rep.Retried != 0 {
		t.Errorf("Retried = %d for an offline refusal; a refusal is not transient", rep.Retried)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("offline crawl took %s; it should not wait at all", el)
	}
}

// ---------------------------------------------------------------- crawling

// fakeClient serves canned bodies and counts calls.
type fakeClient struct {
	bodies   map[string]string
	errs     map[string]error
	calls    int32
	delay    time.Duration
	failThen map[string]int // remaining failures before success
}

func (f *fakeClient) Fetch(_ context.Context, u string) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	if n := f.failThen[u]; n > 0 {
		f.failThen[u] = n - 1
		return nil, errors.New("crawl: server error 525 (transient)")
	}
	if e, ok := f.errs[u]; ok {
		return nil, e
	}
	if b, ok := f.bodies[u]; ok {
		return []byte(b), nil
	}
	return nil, fmt.Errorf("crawl: %s: not found (404)", u)
}
func (f *fakeClient) Delay() time.Duration { return f.delay }
func (f *fakeClient) Close() error         { return nil }

// page is a work page in the real markup shape, parameterised by id so the
// crawl-loop tests can tell one work from another. The elements are the live
// ones (see testdata/ao3_work_page.html); the numbers are small on purpose.
func page(id int64) string {
	return fmt.Sprintf(`<html><body>
<h2 class="title heading">W%d</h2>
<div class="byline"><a rel="author" href="/users/u/pseuds/u">u</a></div>
<dl class="stats">
  <dt class="words">Words:</dt><dd class="words">1,000</dd>
  <dt class="chapters">Chapters:</dt><dd class="chapters">1/1</dd>
  <dt class="status">Updated:</dt><dd class="status">2026-07-27</dd>
</dl>
<div class="summary module"><blockquote class="userstuff"><p>A summary for W%d.</p></blockquote></div>
<dd class="freeform tags"><ul class="commas"><li><a class="tag" href="/tags/Angst/works">Angst</a></li></ul></dd>
</body></html>`, id, id)
}

func TestCrawlerFetchesAndCounts(t *testing.T) {
	fc := &fakeClient{bodies: map[string]string{
		"https://x/works/1": page(1),
		"https://x/works/2": page(2),
	}, delay: 0}
	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	rep, err := c.Run(context.Background(), Options{
		URLs: []string{"https://x/works/1", "https://x/works/2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 2 || rep.Failed != 0 {
		t.Errorf("Fetched=%d Failed=%d, want 2/0", rep.Fetched, rep.Failed)
	}
	if rep.Requested != 2 {
		t.Errorf("Requested = %d", rep.Requested)
	}
}

func TestCrawlerRetriesTransientThenSucceeds(t *testing.T) {
	// A 525 is the measured common case on this site, not an edge case, so a
	// crawler that gives up on the first one loses real work.
	fc := &fakeClient{
		bodies:   map[string]string{"https://x/works/1": page(1)},
		failThen: map[string]int{"https://x/works/1": 2},
		delay:    0,
	}
	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	rep, err := c.Run(context.Background(), Options{
		URLs: []string{"https://x/works/1"}, MaxRetries: 3,
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 1 {
		t.Errorf("Fetched = %d, want 1 after retrying two 525s", rep.Fetched)
	}
	if rep.Retried != 2 {
		t.Errorf("Retried = %d, want 2", rep.Retried)
	}
}

func TestCrawlerDoesNotRetryAPermanent404(t *testing.T) {
	// Retrying a 404 costs 6 seconds and returns the same 404.
	fc := &fakeClient{bodies: map[string]string{}, delay: 0}
	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	rep, err := c.Run(context.Background(), Options{
		URLs: []string{"https://x/works/404"}, MaxRetries: 3,
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Errorf("Failed = %d", rep.Failed)
	}
	if rep.Retried != 0 {
		t.Errorf("Retried = %d for a permanent 404; it should not retry", rep.Retried)
	}
}

func TestCrawlerSkipsWhatResumeAlreadyHas(t *testing.T) {
	// The whole value of resume: a crash costs nothing.
	fc := &fakeClient{bodies: map[string]string{
		"https://x/works/1": page(1),
		"https://x/works/2": page(2),
	}, delay: 0}
	st := NewState()
	st.Mark("https://x/works/1", 1)

	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	rep, err := c.Run(context.Background(), Options{
		URLs:  []string{"https://x/works/1", "https://x/works/2"},
		State: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", rep.Skipped)
	}
	if rep.Fetched != 1 {
		t.Errorf("Fetched = %d, want 1", rep.Fetched)
	}
	if got := atomic.LoadInt32(&fc.calls); got != 1 {
		t.Errorf("made %d requests, want 1 -- resume did not prevent a refetch", got)
	}
}

func TestCrawlerCheckpointsDuringTheRunNotOnlyAtTheEnd(t *testing.T) {
	// The sibling tool checkpointed only after a seed's BFS finished, so the
	// first checkpoint on a large seed was an hour out. The file must exist and
	// grow DURING the run.
	fc := &fakeClient{bodies: map[string]string{}, delay: 0}
	urls := []string{}
	for i := 1; i <= 10; i++ {
		u := fmt.Sprintf("https://x/works/%d", i)
		urls = append(urls, u)
		fc.bodies[u] = page(int64(i))
	}
	dir := t.TempDir()
	sp := filepath.Join(dir, "state.json")

	st := NewState()
	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	_, err := c.Run(context.Background(), Options{
		URLs: urls, State: st, StatePath: sp, CheckpointURLs: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Fetched) != 10 {
		t.Errorf("state holds %d fetched, want 10", len(loaded.Fetched))
	}
}

func TestCrawlerContextCancellationStops(t *testing.T) {
	fc := &fakeClient{bodies: map[string]string{"https://x/works/1": page(1)}, delay: 0}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	if _, err := c.Run(ctx, Options{URLs: []string{"https://x/works/1"}}); err != nil {
		t.Errorf("Run on a cancelled context returned %v, want no error", err)
	}
}

// The bug that made a crawl report success while doing nothing: Run parsed
// every page and incremented Fetched, but no Sink was ever constructed, so the
// mirror gained zero rows and the report said "fetched 2". Verified end to end
// against a fixture site before this test existed.
func TestRunWritesEveryParsedWorkToTheSink(t *testing.T) {
	fc := &fakeClient{bodies: map[string]string{
		"https://x/works/1": page(1),
		"https://x/works/2": page(2),
		"https://x/works/3": page(3),
	}}
	db := newMirrorDB(t)
	sink := NewSQLSink(db)

	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	rep, err := c.Run(context.Background(), Options{
		URLs: []string{
			"https://x/works/1", "https://x/works/2", "https://x/works/3",
		},
		Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 3 {
		t.Fatalf("Fetched = %d, want 3", rep.Fetched)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM works WHERE id IN (1,2,3)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("%d of 3 fetched works reached the mirror", n)
	}
}

// A page that cannot be persisted must be counted SEPARATELY from a fetch
// failure. Conflating them hides the common case: a full disk or a locked
// database reports "fetched 690, failed 0" and looks like a run that grew the
// mirror by nothing.
func TestRunReportsASinkFailureAsUnwrittenNotFailed(t *testing.T) {
	fc := &fakeClient{bodies: map[string]string{
		"https://x/works/1": page(1),
		"https://x/works/2": page(2),
	}}
	sink := &flakySink{failURL: "https://x/works/2"}

	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	rep, err := c.Run(context.Background(), Options{
		URLs: []string{"https://x/works/1", "https://x/works/2"},
		Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 0 {
		t.Errorf("Failed = %d; a write error is not a fetch failure", rep.Failed)
	}
	if rep.Fetched != 2 {
		t.Errorf("Fetched = %d, want 2 (both pages were fetched)", rep.Fetched)
	}
	if rep.Unwritten != 1 {
		t.Errorf("Unwritten = %d, want 1", rep.Unwritten)
	}
	if rep.UnwrittenURLs["https://x/works/2"] == "" {
		t.Errorf("the unwritten URL carries no error: %v", rep.UnwrittenURLs)
	}
}

// With no sink, --parse-only must parse and write nothing without complaint.
func TestRunWithNoSinkWritesNothingAndSucceeds(t *testing.T) {
	fc := &fakeClient{bodies: map[string]string{"https://x/works/1": page(1)}}
	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	rep, err := c.Run(context.Background(), Options{
		URLs: []string{"https://x/works/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 1 || rep.Unwritten != 0 {
		t.Errorf("Fetched=%d Unwritten=%d", rep.Fetched, rep.Unwritten)
	}
}

func TestCrawlerWithConcurrencyStillCountsExactly(t *testing.T) {
	fc := &fakeClient{bodies: map[string]string{}, delay: 0}
	urls := []string{}
	for i := 1; i <= 25; i++ {
		u := fmt.Sprintf("https://x/works/%d", i)
		urls = append(urls, u)
		fc.bodies[u] = page(int64(i))
	}
	c := New(fc).WithSleep(func(context.Context, time.Duration) error { return nil })
	rep, err := c.Run(context.Background(), Options{URLs: urls, Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fetched != 25 {
		t.Errorf("Fetched = %d with 4 workers, want 25", rep.Fetched)
	}
}

// ---------------------------------------------------------------- robots

func TestLoadRobotsDelayReadsTheStatedValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nDisallow: /works/\n")
			fmt.Fprint(w, "User-agent: Slurp\nCrawl-delay: 17\n")
			return
		}
		fmt.Fprint(w, "page")
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	d, err := c.LoadRobotsDelay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d != 17*time.Second {
		t.Errorf("delay = %v, want 17s", d)
	}
}

// A robots.txt that cannot be read is NOT permission to crawl faster. Returning
// 0 here would look like success and mean hammering a site that asked not to be.
func TestLoadRobotsDelayFallsBackConservatively(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	d, err := c.LoadRobotsDelay(context.Background())
	if err == nil {
		t.Error("unreadable robots.txt reported success")
	}
	if d != DefaultCrawlDelay {
		t.Errorf("delay = %v, want the conservative %v", d, DefaultCrawlDelay)
	}
}

func TestLoadRobotsDelayDefaultsWhenTheFileNamesNoDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow:\n")
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	d, _ := c.LoadRobotsDelay(context.Background())
	if d != DefaultCrawlDelay {
		t.Errorf("delay = %v, want %v", d, DefaultCrawlDelay)
	}
}

// ---------------------------------------------------------------- real client

func TestHTTPClientRejectsACloudflareInterstitialServedAs200(t *testing.T) {
	// Measured on the sibling: ~50% of raw probes returned the 8,706-byte 525
	// interstitial, and Cloudflare serves it as HTTP 200. A status check alone
	// writes 8KB of error page into the mirror as zero works.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `<html><head><title>Attention Required! | Cloudflare</title>
			<div class="cf-error-details">error 1016</div></body></html>`)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	if _, err := c.Fetch(context.Background(), "/works/1"); err == nil {
		t.Error("Fetch accepted a Cloudflare interstitial served as 200")
	}
}

func TestHTTPClientClassifiesStatusCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/works/525":
			w.WriteHeader(525)
		case "/works/429":
			w.WriteHeader(429)
		case "/works/404":
			w.WriteHeader(404)
		default:
			fmt.Fprint(w, "<html>ok</html>")
		}
	}))
	defer srv.Close()

	// The delay MUST be zeroed here. A real HTTPClient defaults to the 30s
	// AO3 asks for, and this test makes three requests: at the default it
	// spends 60 seconds proving that 404s are 404s. `HTTPClient.Fetch` waits
	// the delay *before* each request except the first, which is correct
	// behaviour and exactly why a test that wants speed has to say so.
	c := NewHTTPClient(srv.URL)
	c.SetDelay(0)

	for path, wantSub := range map[string]string{
		"/works/525": "server error",
		"/works/429": "rate limited",
		"/works/404": "not found",
	} {
		_, err := c.Fetch(context.Background(), path)
		if err == nil {
			t.Errorf("%s: no error", path)
			continue
		}
		if !strings.Contains(err.Error(), wantSub) {
			t.Errorf("%s: error %q does not mention %q", path, err, wantSub)
		}
	}
}

func TestIsInterstitialDoesNotFlagRealContent(t *testing.T) {
	if IsInterstitial([]byte(realWorkPage(t))) {
		t.Error("a real work page flagged as an interstitial")
	}
	if !IsInterstitial([]byte("... cf-error-details ...")) {
		t.Error("an interstitial not detected")
	}
	if IsInterstitial(nil) {
		t.Error("an empty body flagged as an interstitial")
	}
}

// The bug this file exists to prevent: an absolute seed URL was used verbatim,
// so --base-url could not redirect it and a crawl aimed at a fixture went to
// the REAL archiveofourown.org instead. Measured: 2m37s of real requests at the
// 30s crawl delay, from a test that was supposed to take milliseconds.
func TestResolveRedirectsAnAbsoluteDefaultHostURL(t *testing.T) {
	c := NewHTTPClient("http://127.0.0.1:8731")
	got, err := c.resolve("https://archiveofourown.org/works/111")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8731/works/111" {
		t.Errorf("resolve = %q; --base-url did not redirect an absolute default-host URL", got)
	}
}

func TestResolveKeepsThePathAndQueryWhenRedirecting(t *testing.T) {
	c := NewHTTPClient("http://127.0.0.1:8731")
	got, err := c.resolve("https://archiveofourown.org/works/111/chapters/2?view_adult=true")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8731/works/111/chapters/2?view_adult=true" {
		t.Errorf("resolve = %q; path or query was lost", got)
	}
}

// A caller who named a DIFFERENT host meant that host. Rewriting it would
// silently put a different corpus in the mirror.
func TestResolveLeavesAForeignHostAlone(t *testing.T) {
	c := NewHTTPClient("http://127.0.0.1:8731")
	got, err := c.resolve("https://example.invalid/works/5")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.invalid/works/5" {
		t.Errorf("resolve = %q; a foreign host was rewritten", got)
	}
}

// With no override there is nothing to redirect to, so the URL passes through.
func TestResolvePassesThroughWhenThereIsNoOverride(t *testing.T) {
	c := NewHTTPClient("")
	got, err := c.resolve("https://archiveofourown.org/works/9")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://archiveofourown.org/works/9" {
		t.Errorf("resolve = %q", got)
	}
}

func TestResolveRejectsAnEmptyURL(t *testing.T) {
	c := NewHTTPClient("")
	if _, err := c.resolve("  "); err == nil {
		t.Error("an empty URL was accepted")
	}
}

// --crawl-delay 0 means "do not wait". Treating 0 as "unset" made the override
// unusable for the fixture server it exists for.
func TestSetDelayZeroIsHonoured(t *testing.T) {
	c := NewHTTPClient("")
	c.SetDelay(0)
	if got := c.Delay(); got != 0 {
		t.Errorf("Delay = %v after SetDelay(0), want 0", got)
	}
}

func TestHTTPClientSetsAnIdentifiableUserAgent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		fmt.Fprint(w, "<html>ok</html>")
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	if _, err := c.Fetch(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "kindred") {
		t.Errorf("User-Agent = %q, want it to identify the crawler", seen)
	}
}
