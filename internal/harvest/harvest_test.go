package harvest

// The properties worth testing here are DISTRIBUTION and RESUME, because both fail silently: a
// plan that puts 40% of the budget in one fandom still prints "22,600 works", and a run that
// resumes by re-fetching everything still ends with the right count.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

func fandom(name string, works int) Fandom {
	return Fandom{Name: name, Path: name, Media: "Anime", Works: works}
}

// The href is not a URL: AO3 writes HTML entities into it, and a 404 is reported as one failed
// fandom rather than as a bug, so nothing downstream notices.
func TestParseFandomListCanonicalisesThePath(t *testing.T) {
	body := []byte(`<ul class="index group"><li><a class="tag" href="/tags/James%20Cameron&#39;s%20Avatar%20-%20All%20Media%20Types/works">Avatar</a> (1,234)</li></ul>`)
	fs, err := ParseFandomList(body, "Movies")
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 {
		t.Fatalf("%d fandoms", len(fs))
	}
	if fs[0].Path != "James Cameron's Avatar - All Media Types" {
		t.Fatalf("path = %q, want the plain tag text", fs[0].Path)
	}
	if fs[0].Works != 1234 {
		t.Fatalf("works = %d", fs[0].Works)
	}
	u := listingURL("https://example.test", fs[0].Path, 1)
	if strings.Contains(u, "&#39;") || strings.Contains(u, "%2520") {
		t.Fatalf("listing URL still carries the raw href: %s", u)
	}
}

func TestAllocateGivesEveryFandomTheFloorAndCapsTheBiggest(t *testing.T) {
	fs := []Fandom{
		fandom("huge", 400000), fandom("big", 100000), fandom("mid", 20000),
		fandom("small", 2000), fandom("tiny", 500), fandom("speck", 30),
	}
	p := Allocate(fs, 1200, 25, 200, 0)
	if p.Coverage.FandomsPlanned != len(fs) {
		t.Fatalf("%d fandoms planned, want all %d (the floor fits)", p.Coverage.FandomsPlanned, len(fs))
	}
	for name, q := range p.Quota {
		if q < 25 {
			t.Errorf("%s got %d, below the floor", name, q)
		}
		if q > 200 {
			t.Errorf("%s got %d, above the cap", name, q)
		}
	}
	// The point of the sqrt split: the largest fandom may lead, but never past the cap. A
	// size-proportional split would give "huge" the lot. (A share threshold is NOT the test at six
	// fandoms -- 200 of 692 is 29% and correct there -- so the share property is measured on a
	// realistic catalogue below.)
	if p.Quota["huge"] > 200 {
		t.Fatalf("huge got %d, above the cap: the split is size-proportional, not sqrt", p.Quota["huge"])
	}
	if p.Total < len(fs)*25 {
		t.Fatalf("total %d, below the floors the plan promised", p.Total)
	}
}

// On a catalogue the size of AO3's, no single fandom may take a big slice. This is the property
// the row that motivated the harvest was about: "don't take most content from a handful".
func TestAllocateOnARealisticCatalogueKeepsEveryShareSmall(t *testing.T) {
	var fs []Fandom
	// A Zipf-ish spread: a few giants, a long middle, a tail.
	for i := 0; i < 900; i++ {
		works := 400000 / (i + 1)
		if works < 30 {
			works = 30
		}
		fs = append(fs, fandom(fmt.Sprintf("fandom-%03d", i), works))
	}
	p := Allocate(fs, 22600, 10, 200, 0)
	if p.Total > 22600 {
		t.Fatalf("plan total %d exceeds the target", p.Total)
	}
	var biggest int
	for _, q := range p.Quota {
		if q > biggest {
			biggest = q
		}
	}
	share := float64(biggest) / float64(p.Total)
	if share > 0.02 {
		t.Fatalf("largest fandom takes %.1f%% (%d of %d): the harvest is concentrated",
			100*share, biggest, p.Total)
	}
	if p.Coverage.FandomsPlanned < 500 {
		t.Fatalf("only %d fandoms planned", p.Coverage.FandomsPlanned)
	}
}

// A fandom listed under several media must be ONE fandom, or Total counts it several times and
// the plan promises more works than it allocates.
func TestAllocateMergesAFandomListedUnderSeveralMedia(t *testing.T) {
	fs := []Fandom{
		{Name: "Avatar", Path: "Avatar", Media: "Movies", Works: 5000},
		{Name: "Avatar", Path: "Avatar", Media: "TV Shows", Works: 5000},
		{Name: "Avatar", Path: "Avatar", Media: "Video Games", Works: 5000},
		{Name: "Other", Path: "Other", Media: "Books", Works: 5000},
	}
	p := Allocate(fs, 400, 25, 200, 0)
	if p.Coverage.FandomsInCatalogue != 2 {
		t.Fatalf("catalogue counted %d fandoms, want 2 distinct", p.Coverage.FandomsInCatalogue)
	}
	if len(p.Quota) != 2 {
		t.Fatalf("%d quotas for 2 fandoms", len(p.Quota))
	}
	for name, q := range p.Quota {
		if p.Total != p.Quota["Avatar"]+p.Quota["Other"] {
			t.Fatalf("Total %d does not match the quotas (%s=%d)", p.Total, name, q)
		}
	}
}

func TestAllocateSkipsFandomsTooSmallForTheFloor(t *testing.T) {
	fs := []Fandom{fandom("big", 5000), fandom("crumb", 10)}
	p := Allocate(fs, 400, 25, 200, 0)
	if _, ok := p.Quota["crumb"]; ok {
		t.Fatal("a fandom with 10 works was given a quota of 25")
	}
	if p.Skipped != 1 {
		t.Fatalf("skipped=%d, want 1 counted (not silently dropped)", p.Skipped)
	}
}

func TestAllocateStaysUnderTheTarget(t *testing.T) {
	var fs []Fandom
	for i := 0; i < 200; i++ {
		fs = append(fs, fandom(fmt.Sprintf("f%03d", i), 1000+i*10))
	}
	p := Allocate(fs, 2000, 25, 120, 0)
	if p.Total > 2000 {
		t.Fatalf("plan asks for %d works against a target of 2000", p.Total)
	}
	// Breadth defaults to target/(2*floor): 40 fandoms at 2000/25, so the average quota is twice
	// the floor and the sqrt split has something to distribute.
	if p.Coverage.FandomsPlanned != 40 {
		t.Fatalf("%d fandoms planned, want target/(2*floor) = 40", p.Coverage.FandomsPlanned)
	}
	if p.Total < 40*25 {
		t.Fatalf("total %d, below the floors promised to 40 fandoms", p.Total)
	}
}

// A taste match has to beat a merely popular work, or the harvest is a kudos crawl with extra
// steps. Both works here are the same size; one carries two favourite tags.
func TestScorePrefersTasteOverRawPopularity(t *testing.T) {
	taste := Taste{"dark grey mc": 1, "sociopath mc": 1, "multi-verse": 0.9}
	match := crawl.Blurb{ID: 1, Kudos: 5000, Hits: ptr(90000),
		Tags: []crawl.ParsedTag{{Name: "Dark Grey MC", Type: "freeforms"}, {Name: "sociopath mc", Type: "freeforms"}}}
	popular := crawl.Blurb{ID: 2, Kudos: 6000, Hits: ptr(100000),
		Tags: []crawl.ParsedTag{{Name: "fluff", Type: "freeforms"}}}
	if Score(match, taste) <= Score(popular, taste) {
		t.Fatalf("taste match scored %.2f, popular scored %.2f: kudos decides the harvest",
			Score(match, taste), Score(popular, taste))
	}
	// And with no taste at all the ranking must fall back to quality, not to a constant.
	if Score(match, Taste{}) == Score(popular, Taste{}) {
		t.Fatal("no-taste scoring gave two different works the same score")
	}
}

// The sort parameters are measured requirements: sort_column=kudos answers a syntax-error page and
// sort_column without sort_direction answers a Cloudflare 525.
func TestListingURLUsesTheParametersThatWork(t *testing.T) {
	u := listingURL("https://example.test", "Haikyuu%21%21", 2)
	for _, want := range []string{
		"/tags/Haikyuu%21%21/works?",
		"work_search%5Bsort_column%5D=kudos_count",
		"work_search%5Bsort_direction%5D=desc",
		"page=2",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("listing URL %q is missing %q", u, want)
		}
	}
}

// fakeFetcher serves pages by URL substring, and counts them.
type fakeFetcher struct {
	pages map[string]string
	calls []string
}

func (f *fakeFetcher) Fetch(_ context.Context, rawURL string) ([]byte, error) {
	f.calls = append(f.calls, rawURL)
	for k, v := range f.pages {
		if strings.Contains(rawURL, k) {
			return []byte(v), nil
		}
	}
	return nil, fmt.Errorf("no fixture for %s", rawURL)
}

func listingWith(ids ...int) string {
	var b strings.Builder
	b.WriteString(`<div id="main"><ol class="work index group">`)
	for _, id := range ids {
		fmt.Fprintf(&b, `<li id="work_%d" class="work blurb group"><h4 class="heading"><a href="/works/%d">W%d</a> by <a rel="author" href="/users/u">u</a></h4>
<h5 class="fandoms heading"><a class="tag" href="/tags/F/works">F</a></h5>
<ul class="tags commas"><li class='freeforms'><a class="tag" href="/tags/angst/works">angst</a></li></ul>
<dl class="stats"><dd class="words">1,000</dd><dd class="kudos">%d</dd></dl></li>`, id, id, id, 1000-id)
	}
	b.WriteString(`</ol></div>`)
	return b.String()
}

type memSink struct{ saved []int64 }

func (m *memSink) Save(_ context.Context, w *crawl.ParsedWork) error {
	m.saved = append(m.saved, w.ID)
	return nil
}
func (m *memSink) Close() error { return nil }

func TestRunSavesTheQuotaAndResumes(t *testing.T) {
	ctx := context.Background()
	fan := Fandom{Name: "F", Path: "F", Media: "Anime", Works: 5000}
	plan := Plan{Target: 3, Floor: 3, Quota: map[string]int{"F": 3}, Total: 3}
	f := &fakeFetcher{pages: map[string]string{"/tags/F/works": listingWith(11, 12, 13, 14, 15)}}
	sink := &memSink{}
	st := NewRunState()

	rep, err := Run(ctx, f, RunOptions{
		Base: "https://example.test", Plan: plan, ByName: map[string]Fandom{"F": fan},
		Sink: sink, State: st, WorkPages: false, MaxPages: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.saved) != 3 || rep.Saved != 3 {
		t.Fatalf("saved %d works, want the quota of 3", len(sink.saved))
	}
	if !st.Finished["F"] {
		t.Fatal("the fandom was not marked finished, so a second run would re-fetch it")
	}

	// Second run: nothing to do. The test that catches a resume bug is this one — a resume that
	// silently re-fetches still ends with 3 works in the mirror and looks correct in a report.
	before := len(f.calls)
	rep2, err := Run(ctx, f, RunOptions{
		Base: "https://example.test", Plan: plan, ByName: map[string]Fandom{"F": fan},
		Sink: sink, State: st, WorkPages: false, MaxPages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Saved != 0 {
		t.Fatalf("a resumed run saved %d more works", rep2.Saved)
	}
	if len(f.calls) != before {
		t.Fatalf("a resumed run made %d more requests", len(f.calls)-before)
	}
}

func ptr(n int64) *int64 { return &n }
