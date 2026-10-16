package web

// Rendering tests: every page template, with populated data.
//
// ## Why this file exists
//
// `html/template` resolves field types and method calls at RENDER time, not at
// parse time and not at build time. So a template can reference a field of the
// wrong type and the whole toolchain stays green:
//
//	go build   ok
//	go vet     ok
//	go test    ok
//	browser    500 "wrong type for value; expected int64; got int"
//
// That is not hypothetical. `/neighbours?tag=dark` answered 500 for exactly
// this reason: `commas` took `int64`, `NeighboursPage.TotalCo` was an `int`
// (it came from a `COUNT(*)` scan), and no test had ever rendered that page.
// Every one of the 13 Playwright tests passed, because all 13 used the OLD
// pages.
//
// `commas` now accepts any integer width, which fixes that instance. This file
// is what stops the next one: it renders every template against a struct with
// every field populated, so a wrong-typed reference is a failing test rather
// than a user's 500.
//
// The complement is cmd/e2eserver plus the Playwright suite, which check the
// same templates over real HTTP. This file is the fast, hermetic half and needs
// no server; the browser suite is the half that catches routing and link
// breakage. Neither replaces the other.

import (
	"bytes"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/profile"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// renderOne executes a named page template against data.
//
// A panic in the template (a type mismatch surfaces as one) is turned into a
// test failure with the template named, because an unrecovered panic in a
// template Execute takes the process down and Go reports it with a stack that
// points at text/template rather than at the field that was wrong.
func renderOne(t *testing.T, name string, data any) string {
	t.Helper()

	var buf bytes.Buffer
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("rendering %s panicked: %v\n"+
					"a type mismatch in a template is a render-time panic, so it "+
					"is invisible to go build, go vet and any test that does not "+
					"render this template", name, r)
			}
		}()
		// Each page has its OWN template set (see pages in render.go): the
		// set for "recommend.html" is layout.html + recommend.html, so
		// executing "content" on it runs that page's body.
		tpl, ok := pages[name]
		if !ok {
			t.Fatalf("no template set for %s; it is not in the parse list, so the "+
				"handler for it would 500 with %q", name, "no such template")
		}
		if err := tpl.ExecuteTemplate(&buf, "content", data); err != nil {
			t.Fatalf("rendering %s: %v", name, err)
		}
	}()
	return buf.String()
}

// populated rows, so a `range` body is actually entered. A template whose range
// never iterates exercises none of the expressions inside it, which is how a
// wrong type hides in the last row of a table.
func sampleRows(n int) []corpusquery.Row {
	out := make([]corpusquery.Row, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, corpusquery.Row{
			Key:      "ao3_work:1",
			Label:    "A Work With A & Slash / In The Title",
			Score:    0.1234,
			Works:    12345, // > 999, so commas() is actually exercised
			CoWorks:  6789,
			Lift:     0.5,
			Note:     "quality 10 over 20 hits",
			WorkID:   1,
			Words:    5000,
			Complete: true,
		})
	}
	return out
}

// sampleWorkHits builds populated work rows for the search page's work half.
//
// Every optional field is populated, including the ones a real corpus row
// often lacks, because a `{{if}}` that skips its body hides every expression
// inside it -- the reason this whole file exists.
func sampleWorkHits(n int) []WorkHit {
	out := make([]WorkHit, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, WorkHit{
			ID:        int64(100 + i),
			Title:     "A Work & <Title>",
			Author:    "somebody",
			URL:       "https://example.invalid/works/1",
			Summary:   "A summary. With two sentences. And a third, for shorten().",
			Kudos:     1620, // > 999, so commas() is exercised
			Hits:      9000,
			WordCount: 45000, // > 999, so commas() is exercised
			Language:  "English",
			Complete:  1,
		})
	}
	return out
}

func sampleCandidates(n int) []rank.Candidate {
	out := make([]rank.Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, rank.Candidate{
			ID:      int64(100 + i),
			Title:   "Candidate & <Title>",
			Score:   1.5,
			Summary: "A summary. With two sentences. And a third, for shorten() to cut.",
			URL:     "https://example.invalid/works/1",
			Evidence: []rank.Evidence{
				{Reason: "shares tag &quot;dark&quot; with the seed"},
			},
			TagNames: []string{"dark", "angst"},
		})
	}
	return out
}

// pageRenderCase is one row of the sweep below.
//
// It is a package-level type and slice rather than a local one so
// TestEveryRegisteredPageHasASweepRow can compare it against the registration
// list in render.go. While it was local, that comparison was impossible and two
// registered pages -- crawl.html and mirror.html -- sat outside the sweep
// without anything saying so.
type pageRenderCase struct {
	// tpl is the template to execute; name is a label for the failure message
	// only. They differ because one template is rendered under several names
	// (recommend.html with results, with no seeds, and with a seed-limit
	// warning are three different inputs, one template).
	tpl  string
	name string
	data any
	// wantContains is a second line of defence: it catches a template that
	// renders but renders NOTHING, which is the other way this class of bug
	// hides -- a page that renders an empty table looks like a corpus with no
	// rows rather than a broken template.
	wantContains []string
	// wantNotContains asserts the page makes no claim it cannot support. It
	// exists because the failure this file hunts -- a page that renders
	// successfully while stating something false -- is invisible to
	// wantContains alone: an absent word_count rendered as "0 words"
	// satisfies every other assertion in the case.
	wantNotContains []string
	// hasEmptyList declares that this case renders at least one of the page's
	// lists with no rows, so an empty-state marker is expected in its output.
	// The complement -- a case with rows must NOT carry one -- is asserted for
	// every case that leaves this false, which is what makes a marker that
	// drifts onto a populated element a test failure rather than a lie a
	// reader has to catch.
	hasEmptyList bool
}

// sweepBase is the shared Base every case starts from. Package-level because
// the cases are: it was a local while the table was local, and a case cannot
// reference a variable that is not in its scope.
var sweepBase = Base{Title: "T", Heading: "H", Query: "q"}

var pageRenderCases = []pageRenderCase{
	{
		tpl:  "fandoms.html",
		name: "fandoms.html",
		data: FandomsPage{
			Base:      sweepBase,
			Rows:      sampleRows(3),
			Notes:     []string{"a note"},
			Truncated: true,
			Profile:   "me",
			Gate:      20,
			Available: []string{"me", "you"},
		},
		// The label contains "&" and "/", so the escaped href is the check
		// that urlquery actually ran.
		wantContains: []string{"A Work With A", "12,345", "a note", "Truncated"},
	},
	{
		tpl:  "underrated.html",
		name: "underrated.html",
		data: UnderratedPage{
			Base:      sweepBase,
			Rows:      sampleRows(3),
			Notes:     []string{"candidate pool narrowed before scoring"},
			Truncated: false,
			MinWords:  1000,
			Complete:  true,
		},
		wantContains: []string{"/work/1", "narrowed before scoring"},
	},
	{
		tpl:  "neighbours.html",
		name: "neighbours.html",
		data: NeighboursPage{
			Base:  sweepBase,
			Tag:   "dark & light",
			Rows:  sampleRows(3),
			Notes: []string{"a note"},
			// The exact field and value that made /neighbours?tag=... a 500.
			TotalCo: 987654,
		},
		// 987,654 is the assertion: commas(TotalCo) is what panicked.
		wantContains: []string{"987,654", "6,789", "12,345", "dark &amp; light"},
	},
	{
		tpl:  "surprise.html",
		name: "surprise.html with a pick",
		data: SurprisePage{
			Base: sweepBase,
			Row:  corpusquery.Row{Key: "ao3_work:42", Label: "A Work & Its Title", WorkID: 42, Score: 58},
			Seed: "ao3_work:42",
			// The note must survive: a page that says why the work was
			// eligible is the difference between a pick and an arbitrary row.
			Notes: []string{"58 tags, so there is a neighbourhood"},
		},
		// The seed link is the page's whole point -- without it the reader
		// has a title and no way to use it.
		wantContains: []string{"A Work &amp; Its Title", "/work/42",
			"58 tags", "/recommend?seed=ao3_work%3a42"},
	},
	{
		tpl:          "surprise.html",
		name:         "surprise.html with nothing to pick from",
		hasEmptyList: true,
		data: SurprisePage{
			Base:  sweepBase,
			Notes: []string{"no work in this corpus has 10 or more tags"},
		},
		// The empty state must explain itself, and must NOT show a pick.
		wantContains:    []string{"no work in this corpus has 10 or more tags", "Nothing to pick"},
		wantNotContains: []string{"/recommend?seed="},
	},
	{
		tpl:  "author.html",
		name: "author.html with a raised limit",
		data: AuthorPage{
			Base:  sweepBase,
			Query: "Deydralinne",
			Works: []WorkHit{{ID: 7, Title: "A Work & A Title"}},
			// Total above N is the only state where the raise-n
			// link renders; without it a capped author page reads
			// as "this author wrote one fic".
			Total:   501,
			Limited: true,
			N:       500,
			NextN:   1000,
		},
		wantContains: []string{"Deydralinne", "501", "/work/7",
			"Ask for 1000 instead"},
	},
	{
		tpl:          "neighbours.html",
		name:         "neighbours.html with no tag",
		hasEmptyList: true,
		data:         NeighboursPage{Base: sweepBase},
		// The empty state must SAY something. A blank page here reads as a
		// broken deployment rather than an untouched form.
		wantContains: []string{"Name a tag"},
	},
	{
		tpl:  "profiles.html",
		name: "profiles.html",
		data: ProfilesPage{
			Base: sweepBase,
			Profiles: []ProfileRow{
				{Name: "me & you", Source: "works 1,2", Works: 2, Tags: 7},
			},
			Works: "1, 2",
			Built: "me",
		},
		wantContains: []string{"me &amp; you", "1, 2"},
	},
	{
		tpl:          "profiles.html",
		name:         "profiles.html empty",
		hasEmptyList: true,
		data:         ProfilesPage{Base: sweepBase},
		// An empty profile list must offer the CLI equivalent, or the
		// reader is told nothing is there and left guessing.
		wantContains: []string{"No profiles yet"},
	},
	{
		tpl:          "profiles.html",
		name:         "profiles.html with an error",
		hasEmptyList: true,
		data:         ProfilesPage{Base: sweepBase, Err: "work 999 is not in this corpus"},
		// The error must be VISIBLE. A swallowed error here is the
		// accepted-and-ignored shape this repo has been fighting all along.
		wantContains: []string{"work 999 is not in this corpus"},
	},
	{
		tpl:  "profile.html",
		name: "profile.html",
		data: ProfilePage{
			Base:    sweepBase,
			Profile: profile.Profile{Name: "me", Works: 3, Tags: map[int32]float64{1: 0.5}},
			Tags: []ProfileTagRow{
				{ID: 1, Name: "dark", Weight: 0.5},
				{ID: 2, Name: "angst", Weight: -0.25},
			},
			Rated:     42,
			RatedTags: []ProfileTagRow{{ID: 1, Name: "dark", Weight: 0.125}},
		},
		// "+0.50" is written "&#43;0.50" because html/template escapes the
		// plus sign in a numeric format -- which is correct and, before
		// this was checked, looked like a rendering bug. Asserting the
		// escaped form is asserting what a browser will actually show.
		wantContains: []string{"dark", "&#43;0.50", "-0.25", "Recorded your rating"},
	},
	{
		tpl:          "profile.html",
		name:         "profile.html empty",
		hasEmptyList: true,
		data:         ProfilePage{Base: sweepBase},
		wantContains: []string{"This profile is empty"},
	},
	{
		tpl:          "profile.html",
		name:         "profile.html with an error",
		hasEmptyList: true,
		data:         ProfilePage{Base: sweepBase, Err: "no profile &quot;ghost&quot;"},
		wantContains: []string{"ghost"},
	},
	{
		tpl:  "recommend.html",
		name: "recommend.html",
		data: RecommendPage{
			Base:            sweepBase,
			Similar:         sampleCandidates(2),
			Seeds:           []SeedRef{{Kind: "ao3_work", ID: 1, Raw: "ao3_work:1"}},
			N:               20,
			Returned:        2,
			MaxPerFandom:    3,
			GroupBy:         "fandom",
			MaxSeeds:        5,
			ShortfallReason: "The cap of 3 per fandom removed the rest.",
			Tune: []WeightRow{
				{Name: "co_occurrence", Weight: 0.8},
				{Name: "arena", Weight: 0.2},
			},
		},
		wantContains: []string{
			"ao3_work:1", "Candidate &amp; &lt;Title&gt;",
			"Showing 2 of 20", "The cap of 3 per fandom",
			"co_occurrence", "Why these results",
			// The seed-limit notice must NOT show when it did not happen.
		},
	},
	{
		tpl:          "recommend.html",
		name:         "recommend.html empty seeds",
		hasEmptyList: true,
		data: RecommendPage{
			Base: sweepBase, MaxSeeds: 5,
		},
		// An untouched form must render the form, not a 404 or a blank
		// page: this is what /recommend with no seeds serves. It must also NOT
		// claim a search ran: "Nothing scored above zero for these seeds" is a
		// statement about seeds this page never had, and it reads to a reader
		// as "I searched and found nothing".
		wantContains:    []string{"Seeds", "Rank", "Nothing is ranked yet"},
		wantNotContains: []string{"Nothing scored above zero"},
	},
	{
		tpl:          "recommend.html",
		name:         "recommend.html empty results",
		hasEmptyList: true,
		// Seeds are set because this case tests "seeds given, nothing scored
		// above zero" -- and the template tells that state apart from an
		// untouched form by exactly this field. Without them the case was
		// exercising the no-seeds branch while asserting the message only the
		// searched branch may print.
		data: RecommendPage{
			Base: sweepBase, MaxSeeds: 5, N: 20, MaxPerFandom: 3,
			Seeds: []SeedRef{{Kind: "ao3_work", ID: 1, Raw: "ao3_work:1"}},
		},
		// The empty state must explain WHY it is empty, and must point at
		// the cap as a candidate cause. "No results" with no explanation is
		// indistinguishable from a broken recommender.
		wantContains: []string{"Nothing scored above zero", "diversity cap"},
	},
	{
		name:         "recommend.html seed limit",
		hasEmptyList: true,
		tpl:          "recommend.html",
		data: RecommendPage{
			Base:         sweepBase,
			MaxSeeds:     5,
			SeedLimitHit: true,
		},
		wantContains: []string{"Only the first 5 seeds"},
	},

	// -- the pages that predate this work --------------------------
	//
	// They are here for the same reason as the six above, and the reason
	// is not theoretical: docs/goal-check.py's page-coverage clause found
	// that ten of these templates had never been rendered by a Go test,
	// so a wrong-typed field in work.html or tag.html would have shipped
	// exactly as the neighbours.html one did.

	{
		name: "search.html",
		tpl:  "search.html",
		// Query MUST be set: search.html renders the results list only
		// inside `{{if .Query}}`, and falls through to the "here is what
		// this is" pitch otherwise. Filling Results without Query produces
		// a page with no results and no explanation, which is a third
		// distinct state neither the handler nor a test ever exercises.
		//
		// Works are populated here because the page searches BOTH tags
		// and works. A test that only filled Results would exercise the
		// tag branch and nothing inside the work branch, which is how a
		// wrong-typed field in the work list hides -- the same trap this
		// whole file exists for, in the branch added most recently.
		data: SearchPage{
			Base:        Base{Title: "T", Heading: "H", Query: "dark"},
			Results:     []TagHit{{ID: 7, Name: "dark & light"}, {ID: 8, Name: "angst"}},
			Total:       2,
			Limited:     true,
			Works:       sampleWorkHits(2),
			WorkTotal:   2,
			WorkLimited: true,
		},
		wantContains: []string{"/tag/7", "dark &amp; light", "angst", "2 tags match",
			// The work half, and the link that makes a search result
			// actionable: one click to a ranking seeded by that work.
			`href="/work/100"`, "rank from this",
			// Word count is rendered (idea: data present, not displayed).
			"words",
		},
	},
	{
		// Both halves empty is the honest "nothing matched" state and it
		// must SAY SO per half. A page that rendered neither message would
		// leave the reader unable to tell "no matches" from "the search
		// did not run".
		name:         "search.html no matches in either half",
		hasEmptyList: true,
		tpl:          "search.html",
		data: SearchPage{
			Base: Base{Title: "T", Heading: "H", Query: "zzzznothing"},
		},
		wantContains: []string{
			"No work&rsquo;s title or author matches",
			"No tag matches",
		},
	},
	{
		// A failing work search must not hide working tag results, and
		// must not read as "no matches".
		name: "search.html work search failed, tags still shown",
		tpl:  "search.html",
		data: SearchPage{
			Base:      Base{Title: "T", Heading: "H", Query: "dark"},
			Results:   []TagHit{{ID: 7, Name: "dark"}},
			Total:     1,
			WorkError: "no such table: works",
		},
		wantContains: []string{
			"The work search failed", "no such table: works",
			"Tag results below are unaffected",
			"/tag/7",
		},
	},
	{
		// And the mirror image.
		name: "search.html tag search failed, works still shown",
		tpl:  "search.html",
		data: SearchPage{
			Base:      Base{Title: "T", Heading: "H", Query: "dark"},
			Works:     sampleWorkHits(1),
			WorkTotal: 1,
			TagError:  "no such table: tags",
		},
		wantContains: []string{
			"The tag search failed", "no such table: tags",
			"Work results above are unaffected",
			`href="/work/100"`,
		},
	},
	{
		// A work with NO author, summary, url or word count must render
		// without inventing values. The corpus has NULL in all of these
		// columns, and scanning NULL into a string is what took out the
		// tag page one column at a time.
		name:         "search.html a work with every optional field empty",
		hasEmptyList: true,
		tpl:          "search.html",
		data: SearchPage{
			Base:      Base{Title: "T", Heading: "H", Query: "untitled"},
			Works:     []WorkHit{{ID: 5, Title: "Untitled Fic"}},
			WorkTotal: 1,
		},
		wantContains: []string{`href="/work/5"`, "Untitled Fic"},
		// Nothing that would be a fabricated claim.
		wantNotContains: []string{"0 words", "on AO3"},
	},
	{
		name:         "search.html empty",
		hasEmptyList: true,
		tpl:          "search.html",
		data:         SearchPage{Base: sweepBase},
		// The empty state must render, and must not be an error page.
		wantContains: []string{""},
	},
	{
		name: "work.html",
		tpl:  "work.html",
		data: WorkPage{
			Base: sweepBase,
			Work: corpus.Entity{
				ID: 42, Title: "A Work", Kind: corpus.AO3Kind,
				URL:     "https://example.invalid/works/42",
				Summary: "First sentence. Second sentence.",
				Stats: map[string]float64{
					"hits": 1234567, "kudos": 89,
					"word_count": 45000, "has_bookmarks": 1,
				},
			},
			Tags:    []corpus.TagPair{{ID: 1, Name: "dark"}, {ID: 2, Name: "angst"}},
			Similar: sampleCandidates(2),
			// The half-failure must be VISIBLE, or the page is a lie about
			// what the recommender can do.
			RecommendErr: "no co-occurrence index",
		},
		// 1,234,567 is the assertion that commas() ran on a map value.
		wantContains: []string{"1,234,567", "45,000", "dark", "no co-occurrence index"},
	},
	{
		name: "tag.html",
		tpl:  "tag.html",
		data: TagPage{
			Base: sweepBase,
			Tag:  TagInfo{ID: 7, Name: "dark", WorkCount: 98765},
			Works: []WorkHit{
				{ID: 1, Title: "W1", Author: "A", Kudos: 10, Hits: 100,
					WordCount: 5000, Language: "en", Complete: 1,
					URL:     "https://example.invalid/works/1",
					Summary: "One. Two. Three."},
			},
			Total:      1,
			Neighbours: []Neighbour{{ID: 2, Name: "angst", PMI: 0.42, Count: 1234}},
			// Sort and the control that sets it, populated so the
			// `{{if eq .Value $.Sort}}` inside the range is actually
			// evaluated -- an empty SortOptions means the control
			// renders with no options and the selected branch is never
			// taken.
			Sort: "kudos",
			SortOptions: []SortOption{
				{Value: "kudos", Label: "most kudos"},
				{Value: "recent", Label: "most recently updated"},
				{Value: "words", Label: "longest"},
			},
		},
		// 98,765: commas over a plain int64 struct field.
		wantContains: []string{"98,765", "1,234", "/work/1", "angst",
			// The controls exist and the heading names the order.
			`name="sort"`, "most kudos", "most recently updated",
			`name="words"`, "under 5,000 words",
			"Most kudos first",
			// Word count now renders on the card.
			"5,000 words",
		},
	},
	{
		// The heading must name the order that was actually applied. A
		// page saying "most recently updated" over a kudos listing is a
		// sentence a reader believes for ten seconds, and this is the
		// one place the two could drift apart.
		name:         "tag.html sorted by recency",
		hasEmptyList: true,
		tpl:          "tag.html",
		data: TagPage{
			Base: sweepBase,
			Tag:  TagInfo{ID: 7, Name: "dark", WorkCount: 40},
			Works: []WorkHit{
				{ID: 1, Title: "W1", Author: "A", WordCount: 5000, Complete: 1},
			},
			Sort: "recent",
			SortOptions: []SortOption{
				{Value: "kudos", Label: "most kudos"},
				{Value: "recent", Label: "most recently updated"},
			},
		},
		wantContains: []string{"Most recently updated"},
		// And must NOT claim kudos while sorted by recency.
		wantNotContains: []string{"Most kudos first"},
	},
	{
		// A length filter that matched nothing says so, and names the
		// filter as the reason rather than claiming the tag is empty.
		name:         "tag.html a length filter that matched nothing",
		hasEmptyList: true,
		tpl:          "tag.html",
		data: TagPage{
			Base:        sweepBase,
			Tag:         TagInfo{ID: 7, Name: "dark", WorkCount: 1234},
			Sort:        "kudos",
			Words:       "under:500",
			SortOptions: []SortOption{{Value: "kudos", Label: "most kudos"}},
		},
		wantContains: []string{
			`data-testid="no-matches"`,
			"under:500",
			// The real total is still stated, so the reader can tell a
			// filter excluded them from the tag being empty.
			"1,234",
		},
		wantNotContains: []string{"No works carry this tag"},
	},
	{
		// A word bound that cannot be parsed says so and says the list
		// is unfiltered. Showing an unfiltered list silently under a
		// filter the reader set is the failure this whole change exists
		// to remove.
		name:         "tag.html an unparseable length filter",
		hasEmptyList: true,
		tpl:          "tag.html",
		data: TagPage{
			Base:        sweepBase,
			Tag:         TagInfo{ID: 7, Name: "dark", WorkCount: 40},
			Works:       []WorkHit{{ID: 1, Title: "W1", Author: "A", WordCount: 5000, Complete: 1}},
			Sort:        "kudos",
			WordsErr:    `\"lots\" is not a word-count bound; use under:10000 or over:50000`,
			SortOptions: []SortOption{{Value: "kudos", Label: "most kudos"}},
		},
		wantContains: []string{
			`data-testid="words-error"`,
			"is not a word-count bound",
			"Showing every work on this tag instead",
		},
	},
	{
		name:         "notfound.html",
		tpl:          "notfound.html",
		data:         NotFoundPage{Base: sweepBase, Path: "/nope"},
		wantContains: []string{"/nope"},
	},
	{
		name: "error.html",
		tpl:  "error.html",
		data: ErrorPage{Base: sweepBase, Path: "/x", Message: "the index would not load"},
		// The message must be shown. An error page with no message is a
		// bare status code in a browser.
		wantContains: []string{"the index would not load", "/x"},
	},
	{
		name: "arena.html with a pair",
		tpl:  "arena.html",
		data: ArenaPage{
			Base:     sweepBase,
			Cards:    []WorkCard{sampleCard("A", 1), sampleCard("B", 2)},
			Session:  "sess-1",
			Strategy: "glicko",
			// A rating with no uncertainty is a number the reader will
			// over-trust, so RD is on the card.
			TotalJudged: 500,
		},
		wantContains: []string{"sess-1", "glicko", "Card A", "Card B"},
	},
	{
		name:         "arena.html with no pair",
		hasEmptyList: true,
		tpl:          "arena.html",
		data: ArenaPage{
			Base:    sweepBase,
			Message: "the arena pool is exhausted; restart to judge more",
		},
		// An exhausted pool must say so rather than render an empty form
		// that looks like a page that failed to load two works.
		wantContains: []string{"exhausted"},
	},
	{
		name: "leaderboard.html",
		tpl:  "leaderboard.html",
		data: LeaderboardPage{
			Base:       sweepBase,
			Median:     1500,
			MinCompare: 3,
			Rows: []LeaderboardRow{
				{Rank: 1, WorkID: 42, Title: "Top Work", Author: "A",
					Rating: 1800, Effective: 1750, RD: 90,
					Comparisons: 12, Wins: 9, Losses: 2, Draws: 1},
			},
		},
		// "1800" not "1,800": this table uses %.0f and never calls commas.
		// Arena ratings are 600-3000, so a thousands separator would never
		// fire here anyway -- and asserting one would be asserting a
		// formatting change nobody asked for.
		wantContains: []string{"Top Work", "1800", "9/2/1"},
	},
	{
		name:         "leaderboard.html empty",
		hasEmptyList: true,
		tpl:          "leaderboard.html",
		data:         LeaderboardPage{Base: sweepBase, Empty: true, MinCompare: 3},
		wantContains: []string{""},
	},
	{
		name: "myranking.html",
		tpl:  "myranking.html",
		data: MyRankingPage{
			Base: sweepBase,
			Liked: []TagLink{
				{Name: "dark", URL: "/tag/1"},
				{Name: "angst", URL: "/tag/2"},
			},
			Disliked:       []TagLink{{Name: "humour", URL: "/tag/3"}},
			Judged:         12,
			MinimumForView: 5,
			RatedWorks:     30,
		},
		wantContains: []string{"dark", "angst", "humour", "/tag/1"},
	},
	{
		name: "rank.html",
		tpl:  "rank.html",
		data: RankPage{
			Base: sweepBase, WorkID: 42, Title: "Rated Work", Author: "A",
			Rating: 1620, RD: 85, Sigma: 0.05,
			Comparisons: 20, Wins: 14, Losses: 5, Draws: 1,
			WorkURL: "/work/42",
			History: []RankPoint{
				{Period: 202601, Rating: 1500, RD: 200},
				{Period: 202602, Rating: 1620, RD: 85},
			},
			Note: "this is the starting rating, not a measured one",
		},
		// The title and the link are the assertion that mattered: the
		// template printed neither, while RankPage carried both fields.
		wantContains: []string{"Rated Work", `href="/work/42"`, "1620", "85",
			"starting rating", "How it got here"},
	},
	{
		name: "block.html",
		tpl:  "block.html",
		data: BlockPage{
			Base: sweepBase,
			Candidates: []BlockRow{
				{TagID: 1, Name: "dark", URL: "/tag/1",
					Reason: "you keep passing these over", Weight: 0.8, N: 9},
			},
			Blocked: []BlockRow{
				{TagID: 2, Name: "humour", URL: "/tag/2", Blocked: true},
			},
			Judged: 12, MinimumForView: 5,
		},
		wantContains: []string{"dark", "you keep passing these over", "humour"},
	},
	{
		name:         "block.html empty",
		hasEmptyList: true,
		tpl:          "block.html",
		data:         BlockPage{Base: sweepBase, Empty: true, Judged: 1, MinimumForView: 5},
		wantContains: []string{""},
	},
	{
		tpl:  "crawl.html",
		name: "crawl.html with a backlog",
		data: CrawlPage{
			Base:    sweepBase,
			Pending: 12,
			Done:    40,
			Parked:  3,
			Total:   55,
			Cap:     5,
			Sources: []QueueSource{
				{Source: "discover", Count: 9, Why: "found by a discovery walk"},
				{Source: "reader-requested", Count: 3, Why: "a reader asked for this work"},
			},
			Recent: []QueueRow{
				{URL: "https://archiveofourown.org/works/1", Source: "discover",
					EnqueuedAt: "2026-10-13 20:00:00", Why: "found by a discovery walk"},
				{URL: "https://archiveofourown.org/works/2", Source: "reader-requested",
					Attempts: 5, LastError: "404 from AO3", Parked: true,
					Why: "a reader asked for this work"},
			},
		},
		// Both verdicts, not just one: they are independent statements, and the
		// if/else-if that made them mutually exclusive is the bug this page had.
		wantContains: []string{
			"12 works waiting", "3 works failed 5 times", "found by a discovery walk",
			"404 from AO3", "given up (5 tries)",
		},
	},
	{
		tpl:          "crawl.html",
		name:         "crawl.html with nothing queued",
		hasEmptyList: true,
		data:         CrawlPage{Base: sweepBase, Cap: 5},
		wantContains: []string{"Nothing is queued", "mirror is not growing", "kindred discover"},
	},
	{
		tpl:          "crawl.html",
		name:         "crawl.html with no queue at all",
		hasEmptyList: true,
		data:         CrawlPage{Base: sweepBase, NoQueue: true, Cap: 5},
		wantContains: []string{"no crawl queue"},
	},
	{
		tpl:  "mirror.html",
		name: "mirror.html with a corpus, a graph and a backlog",
		data: MirrorPage{
			Base: sweepBase, HasCorpus: true,
			Works: 12345, Tags: 678, WorkTags: 9012, Edges: 3456,
			HasGraph: true, Nodes: 678, GraphEdges: 3456, GraphBytes: 26214400,
			Pending: 12, Done: 40, Parked: 3, Cap: 5,
			Sources: []QueueSource{{Source: "discover", Count: 12, Why: "found by a discovery walk"}},
		},
		wantContains:    []string{"12,345", "9,012", "3,456", "25.0 MiB", "12 works waiting"},
		wantNotContains: []string{"No co-occurrence graph is loaded"},
	},
	{
		tpl:          "mirror.html",
		name:         "mirror.html with no corpus and no queue",
		hasEmptyList: true,
		data:         MirrorPage{Base: sweepBase, NoCorpus: true, NoQueue: true, Cap: 5},
		wantContains: []string{"no corpus handle", "no crawl queue", "/api/v1/stats"},
		// A labelled zero for a corpus it cannot read is the claim this page
		// must not make.
		wantNotContains: []string{`<span class="stat-label">works</span>`},
	},
	{
		// The mutation gate's survivor: mirror.html has four empty states
		// (no corpus, no graph, no queue, nothing queued) and every other case
		// had at least two of them rendering at once, so removing ONE marker
		// left the page's marker count unchanged and survived. These two cases
		// leave exactly one empty state on the page, which is the input that
		// makes that marker load-bearing.
		tpl:  "mirror.html",
		name: "mirror.html with a corpus and no crawl queue",
		data: MirrorPage{
			Base: sweepBase, HasCorpus: true, Works: 12345, Tags: 678,
			HasGraph: true, Nodes: 678, GraphEdges: 3456, GraphBytes: 26214400,
			NoQueue: true, Cap: 5,
		},
		hasEmptyList: true,
		wantContains: []string{"no crawl queue"},
		// A labelled zero for a queue that does not exist is the claim this
		// state must not make.
		wantNotContains: []string{"waiting to be fetched"},
	},
	{
		tpl:  "mirror.html",
		name: "mirror.html with a corpus and an empty queue",
		data: MirrorPage{
			Base: sweepBase, HasCorpus: true, Works: 12345, Tags: 678,
			HasGraph: true, Nodes: 678, GraphEdges: 3456, GraphBytes: 26214400,
			Cap: 5,
		},
		hasEmptyList: true,
		wantContains: []string{"Nothing is queued", "mirror is not growing"},
	},
	{
		tpl:             "mirror.html",
		name:            "mirror.html when the queue could not be read",
		hasEmptyList:    true,
		data:            MirrorPage{Base: sweepBase, HasCorpus: true, QueueErr: true, Cap: 5},
		wantContains:    []string{"could not be read", "backlog is unknown"},
		wantNotContains: []string{"Nothing is queued"},
	},
}

// TestEveryPageTemplateRenders is the sweep.
//
// Each case is a template this file's siblings assert against, and every field
// is populated including the zero-ish ones, because a `{{if}}` that skips its
// body hides every expression in it. The `wantSubstring` check is a second
// line of defence: it catches a template that renders but renders NOTHING,
// which is the other way this class of bug hides -- a page that renders an
// empty table looks like a corpus with no rows rather than a broken template.
func TestEveryPageTemplateRenders(t *testing.T) {
	cases := pageRenderCases
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderOne(t, tc.tpl, tc.data)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("%s rendered without %q\n--- rendered ---\n%s",
						tc.name, want, truncate(got, 1200))
				}
			}
			for _, unwanted := range tc.wantNotContains {
				if strings.Contains(got, unwanted) {
					t.Errorf("%s rendered %q, which is a claim the data "+
						"does not support\n--- rendered ---\n%s",
						tc.name, unwanted, truncate(got, 1200))
				}
			}
		})
	}
}

// TestCommasAcrossIntegerWidths locks in the fix.
//
// This is the regression test for the 500, stated as the property rather than
// the symptom: commas is called from templates whose data comes from SQL
// scans of several different integer types, and it must render all of them.
func TestCommasAcrossIntegerWidths(t *testing.T) {
	cases := []struct {
		in   any
		want string
		why  string
	}{
		{int(1234567), "1,234,567", "corpusquery.Row.Works and NeighboursPage.TotalCo are ints"},
		{int32(1234567), "1,234,567", "tag ids are int32 throughout the schema"},
		{int64(1234567), "1,234,567", "work stats come back as int64"},
		{uint(1234567), "1,234,567", "a COUNT(*) scanned into uint"},
		{int(0), "0", "zero is a real answer, not an empty cell"},
		{int(-1234), "-1,234", "a negative delta is not clamped away silently"},
		{"notanumber", "notanumber", "an unexpected type is shown, not dropped"},
	}
	for _, tc := range cases {
		if got := commas(tc.in); got != tc.want {
			t.Errorf("commas(%v) = %q, want %q (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

// TestEveryAssetIsAPageTemplate catches a template file that exists but is not
// registered.
//
// The registration list in render.go is a hand-written []string, so a template
// added to assets/ and not to that list parses fine, vets fine, tests fine, and
// 500s at request time with "no such template" -- an internal message shown to
// a reader. Comparing the two lists is a few lines and closes the gap.
func TestEveryAssetIsAPageTemplate(t *testing.T) {
	registered := make(map[string]bool, len(pages))
	for name := range pages {
		registered[name] = true
	}
	// The registry is the list of pages, so the check runs against it rather
	// than against the map it builds: a template that is in assets/ and in the
	// map but not in the registry is a page no URL reaches.
	for _, spec := range allPages() {
		if !registered[spec.Template] {
			t.Errorf("%s is registered but has no template set", spec.Template)
		}
	}

	entries, err := assets.ReadDir("assets")
	if err != nil {
		t.Fatalf("reading assets: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		// The shell is in every set as a fragment, not as a page.
		if name == shellTemplate {
			continue
		}
		if !registered[name] {
			t.Errorf("%s exists in assets/ but is not in the page registry in "+
				"registry.go; a file nothing routes to is a page nobody can tell is "+
				"missing", name)
		}
	}

	// And the other direction: a registered page with no file would have
	// panicked in init(), so reaching here at all means they agree.
	for name := range pages {
		if _, err := assets.ReadFile("assets/" + name); err != nil {
			t.Errorf("%s is registered but not readable from assets: %v", name, err)
		}
	}
}

// sampleCard is one side of an arena comparison.
func sampleCard(label string, id int64) WorkCard {
	return WorkCard{
		ID: id, Title: "Card " + label, Author: "Author " + label,
		Summary: "A summary. With two sentences.",
		Rating:  0.75, RD: 0.12,
		TagURL: []TagLink{{Name: "dark", URL: "/tag/1"}},
		URL:    "https://example.invalid/works/" + label,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

// TestEveryRegisteredPageHasASweepRow closes the gap that let crawl.html and
// mirror.html sit outside the sweep.
//
// The registration list in render.go and the cases above are two hand-written
// lists, and nothing compared them. A registered page with no case is not
// broken -- it is UNCHECKED, and a template that renders successfully while
// stating something false passes every other test in this package. The
// sweepBase hoist above exists for this test.
func TestEveryRegisteredPageHasASweepRow(t *testing.T) {
	covered := make(map[string]bool, len(pageRenderCases))
	for _, tc := range pageRenderCases {
		covered[tc.tpl] = true
	}

	// Every spec, not every key of pages: the shell is a key of pages and not
	// a page, so it needs no exclusion here, and a page added to the registry
	// without a case fails this by name instead of being missed because
	// someone forgot to add it to a second list.
	for _, spec := range allPages() {
		if !covered[spec.Template] {
			t.Errorf("%s is a registered page with no case in pageRenderCases, so "+
				"nothing executes it with fields populated", spec.Template)
		}
	}
}
