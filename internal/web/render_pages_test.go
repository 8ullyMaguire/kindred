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

	"git.polarisocial.xyz/kindred/kindred/internal/algo"
	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/profile"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"
	"time"
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
	// The empty render of each page whose populated render was already here.
	//
	// A page with only a populated case is checked for the marker's PRESENCE as
	// text (TestEveryListPageMarksItsEmptyState reads the template) and never as
	// a render, so a marker moved to an element that renders WITH rows survives
	// both checks. These five are the pages that were in that state; the
	// completeness check at the end of TestListPagesEmptyStateIsTwoSided keeps
	// the list from going stale.
	{
		tpl:  "author.html",
		name: "author.html with no matches",
		data: AuthorPage{Base: sweepBase, Query: "zzzznobody"},
		// The sentence has to name what was searched for and say that a
		// shorter name finds more, or "no rows" is indistinguishable from a
		// broken byline index.
		hasEmptyList:    true,
		wantContains:    []string{"no-author-works", "zzzznobody", "matched as substrings"},
		wantNotContains: []string{"Ask for", "<tbody"},
	},
	{
		tpl:          "fandoms.html",
		name:         "fandoms.html with nothing above the floor",
		data:         FandomsPage{Base: sweepBase, Gate: 20, Available: []string{"me", "you"}},
		hasEmptyList: true,
		// The claim is about the corpus at this threshold, not an error.
		wantContains:    []string{"evidence floor", "not an error"},
		wantNotContains: []string{"<tbody"},
	},
	{
		tpl:          "rank.html",
		name:         "rank.html for a work with no rating history",
		data:         RankPage{Base: sweepBase, WorkID: 7, Title: "Unrated Work"},
		hasEmptyList: true,
		wantContains: []string{"No rating periods yet"},
	},
	{
		tpl:          "underrated.html",
		name:         "underrated.html with no work matching",
		data:         UnderratedPage{Base: sweepBase, MinWords: 1000, Complete: true},
		hasEmptyList: true,
		// "no rows with no explanation" is the thing this page must not be.
		wantContains:    []string{"No work matched", "notes below"},
		wantNotContains: []string{"<tbody"},
	},
	{
		tpl:  "skipped.html",
		name: "skipped.html with rows",
		data: SkippedPage{
			Base:      sweepBase,
			Threshold: 3,
			Works: []SkippedWorkView{
				{ID: 5, Title: "Shown Four Times", Shows: 4, LastShown: "2026-10-22 10:00:00", Arms: "a"},
			},
		},
		// The threshold is printed, and the action names the mechanism rather
		// than promising a demotion the page cannot measure.
		wantContains: []string{"Shown Four Times", "shown 4 times", "Not interested", "rank <em>lower</em>"},
	},
	{
		tpl:  "skipped.html",
		name: "skipped.html before anything qualifies",
		data: SkippedPage{
			Base:      sweepBase,
			Threshold: 3,
			Empty:     "Nothing yet. A work has to be shown 3 separate times",
		},
		hasEmptyList: true,
		wantContains: []string{"Nothing yet", "3 separate times"},
	},
	{
		tpl:  "choose.html",
		name: "choose.html with the registry and a standing",
		data: ChoosePage{
			Base:   sweepBase,
			Kinds:  algo.List(),
			Chosen: map[arena.EntityKind]string{arena.KindWork: string(algo.Collab)},
			Arms:   map[arena.EntityKind]algo.ID{arena.KindWork: algo.Hybrid},
			Standing: []store.AlgoArmStat{
				{Kind: arena.KindWork, Algo: "hybrid", Arm: "default", Impressions: 12, Credits: 3, CreditRate: 0.25},
			},
			Focus: arena.KindWork,
			Saved: "Saved: co-bookmark CF for works.",
		},
		// The three claims the page makes: a default is named, a disabled
		// algorithm carries its reason, and the standings carry numbers.
		wantContains: []string{"default", "not available", "training pipeline", "25.0%", "Saved:"},
	},
	{
		tpl:  "choose.html",
		name: "choose.html before anything is served",
		data: ChoosePage{
			Base:   sweepBase,
			Kinds:  algo.List(),
			Focus:  arena.KindTag,
			NoData: "No lists have been served yet",
		},
		hasEmptyList: true,
		wantContains: []string{"No lists served for tags yet", "No lists have been served yet"},
	},
	{
		tpl:  "related.html",
		name: "related.html with rows",
		data: RelatedPage{
			Base:      sweepBase,
			Kind:      arena.KindTag,
			Seed:      61461,
			SeedLabel: "jasmine",
			Algo:      algo.Embed,
			Default:   algo.Embed,
			Choices:   algo.LiveIDs(arena.KindTag),
			Arm:       "default",
			Millis:    21,
			Items: []RelatedItemView{
				{ID: 2, Label: "aurora", Why: "0.94 cosine in the tag space", Score: 0.94},
			},
		},
		wantContains: []string{"jasmine", "aurora", "0.94 cosine", "default", "21 ms"},
	},
	{
		tpl:  "related.html",
		name: "related.html with a missing dimension",
		data: RelatedPage{
			Base:    sweepBase,
			Kind:    arena.KindAuthor,
			Seed:    42,
			Algo:    algo.CoBookAuthors,
			Default: algo.CoBookAuthors,
			Choices: algo.LiveIDs(arena.KindAuthor),
			Missing: []string{"readers"},
			Note:    "no reader of this author's works has read another work",
		},
		hasEmptyList: true,
		wantContains: []string{"Missing: readers", "no reader of this author"},
	},
	{
		tpl:  "new.html",
		name: "new.html after a second visit",
		data: NewPage{
			Base:      sweepBase,
			Requested: 40,
			Visit: &store.VisitCursor{
				PreviousAt: time.Date(2026, 10, 20, 14, 10, 0, 0, time.UTC),
				CurrentAt:  time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC),
				Visits:     2,
			},
			Works: []store.NewWork{
				{ID: 7, Title: "Arrived Tuesday", Authors: "someone", FirstSeen: "2026-10-21T00:00:00+00:00"},
			},
		},
		// The BOUNDARY is asserted, not just the rows: a list without the date it
		// was measured from cannot be told apart from an empty site.
		wantContains: []string{"Arrived Tuesday", "20 Oct 2026 14:10", "visit 2"},
	},
	{
		tpl:  "new.html",
		name: "new.html on a first visit",
		data: NewPage{
			Base:       sweepBase,
			Requested:  40,
			FirstVisit: true,
			Visit: &store.VisitCursor{
				CurrentAt: time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC),
				Visits:    1,
			},
		},
		hasEmptyList: true,
		wantContains: []string{"first-visit", "no “before” to compare against"},
	},
	{
		tpl:  "new.html",
		name: "new.html with nothing new",
		data: NewPage{
			Base:      sweepBase,
			Requested: 40,
			Visit: &store.VisitCursor{
				PreviousAt: time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC),
				CurrentAt:  time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC),
				Visits:     3,
			},
		},
		hasEmptyList: true,
		wantContains: []string{"nothing-new", "Nothing has arrived since then"},
	},
	{
		tpl:  "path.html",
		name: "path.html with a walk",
		data: PathPage{
			Base:   sweepBase,
			Steps:  10,
			LastID: 208,
			Path: &engine.ReadingPath{
				Seed:          engine.WorkRef{ID: 1, Title: "A Work"},
				Requested:     10,
				Returned:      2,
				DriftFromSeed: 2,
				Steps: []engine.PathStep{
					{
						WorkRef:        engine.WorkRef{ID: 207, Title: "Next Door"},
						Step:           1,
						SimToPrev:      0.81,
						SharedWithPrev: []string{"fluff"},
						SharesSeedTag:  true,
					},
					{
						WorkRef:       engine.WorkRef{ID: 208, Title: "Further Out"},
						Step:          2,
						SimToPrev:     0.42,
						SharesSeedTag: false,
					},
				},
			},
		},
		// The drift marker only renders on a step that shares nothing with the
		// seed, so a sweep with every step still on the seed's tags would not
		// execute that branch at all.
		wantContains: []string{"Next Door", "Further Out", "0.42", "left-the-seed", "2 of 10 steps"},
	},
	{
		tpl:  "path.html",
		name: "path.html whose walk took no step",
		data: PathPage{
			Base:  sweepBase,
			Steps: 10,
			Path: &engine.ReadingPath{
				Seed:      engine.WorkRef{ID: 1, Title: "A Work"},
				Requested: 10,
				Note:      "No step could be taken from this work.",
			},
		},
		hasEmptyList: true,
		wantContains: []string{"No step could be taken"},
	},
	{
		tpl:          "path.html",
		name:         "path.html with no work named",
		data:         PathPage{Base: sweepBase, Steps: 10},
		wantContains: []string{"/path/17249318", "A path starts from a work"},
	},
	{
		tpl:  "work.html",
		name: "work.html whose recommendations are empty but not failed",
		data: WorkPage{
			Base: sweepBase,
			Work: corpus.Entity{ID: 42, Title: "A Work", Kind: corpus.AO3Kind},
			// The distinctive block renders only when it has tags, so the
			// sweep has to carry them: a template referencing a field that
			// is never populated is exactly the 500-shaped hole this file
			// exists to catch.
			DNA:  []corpus.TagPair{{ID: 2, Name: "selkie au"}, {ID: 3, Name: "time travel"}},
			Tags: []corpus.TagPair{{ID: 1, Name: "fluff"}},
		},
		// This branch is reachable only when the recommender SUCCEEDED and
		// returned nothing (the case above has RecommendErr set), and the
		// sentence has to say that is a real answer: an empty "more like this"
		// that reads as a failure sends a reader to look for a bug.
		hasEmptyList: true,
		wantContains: []string{"Nothing scored above zero", "real answer, not a"},
	},
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
			// A populated author page renders the similar-author panel
			// too, so its populated direction has to be populated here:
			// TestListPagesEmptyStateIsTwoSided forbids any data-empty
			// marker in this case.
			Similar: []engine.SimilarAuthor{{
				Name:        "someone-else",
				SharedWorks: 3,
				Score:       0.4,
				Evidence: []rank.Evidence{{
					Signal: "author_tag_profile",
					Value:  0.4,
					Reason: `shares 2 distinguishing tags with "Deydralinne": slow burn, epistolary`,
				}},
			}},
		},
		wantContains: []string{"Deydralinne", "501", "/work/7",
			"Ask for 1000 instead", "Authors who write like this",
			"someone-else", "epistolary"},
	},
	{
		tpl:  "algo.html",
		name: "algo.html with two rankings",
		data: AlgoPage{
			Base:          sweepBase,
			Seed:          "ao3_work:20151202",
			A:             "default",
			B:             "recency",
			Choices:       []string{"default", "recency"},
			Left:          sampleCandidates(3),
			Right:         sampleCandidates(3),
			LeftWeights:   map[string]float64{"tag_overlap": 0.24},
			RightWeights:  map[string]float64{"tag_overlap": 0.10, "recency": 0.30},
			WeightsDiffer: []string{"recency", "tag_overlap"},
			Shared:        2,
		},
		// The delta line is the page's whole reason to exist: a reader is
		// asked to judge two lists and is told which signals moved.
		wantContains: []string{"Signals that differ", "recency", "2 of 3 works"},
	},
	{
		tpl:          "algo.html",
		name:         "algo.html untouched",
		hasEmptyList: true,
		data: AlgoPage{
			Base:    sweepBase,
			Choices: []string{"default", "recency"},
		},
		wantContains: []string{"Name a work and pick two algorithms"},
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
			SeedOrigin:      "No seed was given, so this starts from the 3 most-kudosed works in the mirror.",
			ShortfallReason: "The cap of 3 per fandom removed the rest.",
			// The calibration block, populated. Every field is set because a
			// template referencing a field that does not exist renders empty
			// and a sweep that never populated it would not notice.
			Calibration: CalibrationView{
				Sentence: "In the last evaluation, 40 readers' lists were right at: first three 18%.",
				Measured: true, Enough: true, Readers: 40, Needed: 20, RunID: 7,
				Bands: []CalibrationBand{
					{Label: "the first three", Hits: 14, Observed: 80, Percent: "18%", OneIn: "1 in 6"},
					{Label: "the next four", Hits: 6, Observed: 80, Percent: "8%", OneIn: "1 in 13"},
					{Label: "the rest", Hits: 0, Observed: 0},
				},
			},
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
			// The co-bookmark panel is populated too, or its empty marker
			// would appear on a page that has rows -- which is the two-sided
			// marker failure the sweep exists to catch.
			CoBookmarked: sampleCandidates(2),
			Bridges: []engine.Bridge{{
				Candidate:    sampleCandidates(1)[0],
				OtherFandoms: []string{"another fandom (anime)"},
			}},
			// The half-failure must be VISIBLE, or the page is a lie about
			// what the recommender can do.
			RecommendErr: "no co-occurrence index",
		},
		// 1,234,567 is the assertion that commas() ran on a map value.
		wantContains: []string{"1,234,567", "45,000", "dark", "no co-occurrence index",
			"Readers who bookmarked this", "Bridges into other fandoms"},
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
		// The merged list. The exposure sentence is the assertion that
		// matters: it is built by naming the two arms, and the first version
		// ranged over a map with no separator and rendered "default
		// contributed 3quality contributed 3" — visible only in a browser.
		tpl:  "algo.html",
		name: "algo.html merged list with both arms",
		data: AlgoPage{
			Base: sweepBase, Mode: "interleave", Seed: "ao3_work:1",
			A: "default", B: "quality",
			Merged: []rank.InterleavedItem{
				{Candidate: rank.Candidate{ID: 7, Title: "One", Score: 1.5}, Arm: "default", Slot: 0},
				{Candidate: rank.Candidate{ID: 8, Title: "Two", Score: 1.2}, Arm: "quality", Slot: 1},
			},
			Exposure: map[string]int{"default": 1, "quality": 1},
			// The standings populated too, so this case does not render a
			// data-empty marker: a case with rows must not claim emptiness,
			// and the empty standings branch has its own case below.
			Arms: []store.InterleaveArmStat{
				{Arm: "default", Impressions: 120, Credits: 9, CreditRate: 0.075},
				{Arm: "quality", Impressions: 118, Credits: 6, CreditRate: 0.0508},
			},
			Pairs: []store.InterleavePairStat{{
				ArmA: "default", ArmB: "quality",
				ImpressionsA: 120, ImpressionsB: 118, CreditsA: 9, CreditsB: 6,
				Difference: 0.075 - 0.0508,
			}},
		},
		wantContains: []string{
			"One list, both algorithms",
			`data-arm="default"`,
			`data-arm="quality"`,
			"contributed 1,", // the separator between the two arms
			"0.075",
		},
		// Two lists plus a merged one would ask the question twice.
		wantNotContains: []string{"algo-column", "3quality"},
	},
	{
		// A merged list whose standings are empty says so, and the marker is
		// where it belongs: on the empty branch, not on the list of works.
		tpl:          "algo.html",
		name:         "algo.html merged list with no standings yet",
		hasEmptyList: true,
		data: AlgoPage{
			Base: sweepBase, Mode: "interleave", Seed: "ao3_work:1",
			A: "default", B: "quality",
			Merged: []rank.InterleavedItem{
				{Candidate: rank.Candidate{ID: 7, Title: "One"}, Arm: "default", Slot: 0},
			},
			Exposure: map[string]int{"default": 1, "quality": 0},
		},
		wantContains: []string{"No impressions yet", `data-arm="default"`},
	},
	{
		tpl:  "eval.html",
		name: "eval.html with a stored run, votes and credits",
		data: EvalPage{
			Base: sweepBase,
			Runs: []store.EvalRun{{
				ID: 7, RanAt: "2026-10-22 19:00:00", K: 10, Mode: "full",
				Readers: 214, Bookmarks: 5100, Evaluated: 199, HeldOut: 1020,
				Hits: 143, Recall: 0.1402, NDCG: 0.0931, Coverage: 0.6120,
			}},
			// The slices section is below the runs table and renders only
			// when a run has them, so the sweep carries both axes.
			SliceRunID: 7,
			SliceMinN:  20,
			FandomSlices: []store.EvalSlice{
				{Axis: "fandom", Name: "rare fandom", N: 40, Hits: 2, HitRate: 0.05},
				{Axis: "fandom", Name: "big fandom", N: 300, Hits: 60, HitRate: 0.20},
			},
			TagSlices: []store.EvalSlice{
				{Axis: "tag", Name: "selkie au", N: 25, Hits: 1, HitRate: 0.04},
			},
			Algos: []store.AlgoStanding{{
				Algo: "default", Judged: 30, Wins: 17, Losses: 11, Ties: 2,
				P: 0.344928, Evidence: "none",
				// The real message for 17-11: past the floor, and the p-value
				// itself is what refuses. A fixture quoting the FLOOR message
				// here would have been a record the code cannot produce, and
				// would hide a wrong branch behind plausible text.
				EvidenceWhy: "no evidence that default is better or worse: 17 wins against 11 losses gives p = 0.345.",
			}},
			Arms: []store.InterleaveArmStat{{
				Arm: "default", Impressions: 200, Credits: 14, CreditRate: 0.07,
			}},
			// A young pair: 3 slots and 0 slots, refused by the floor. The
			// dashboard must render its verdict rather than the rate alone.
			Pairs: []store.InterleavePairStat{{
				ArmA: "default", ArmB: "hybrid", ImpressionsA: 3, ImpressionsB: 0,
				Difference: 0, P: 1, Underpowered: true,
				NoLeadReason: "too few slots to compare: 3 slots for default and 0 slots for hybrid, and 50 each are needed.",
			}},
			Works: 112935, Tags: 634231, WorkTags: 2884447, Edges: 2884447,
		},
		wantContains: []string{
			"#7", "0.1402", "0.0931", "0.6120", "default", "112,935",
			"held-out", "30 days",
			// The dashboard must say the rates are RAW when no pair has cleared
			// the test: a table of rates beside a heading is read as a result.
			"No result yet.", "no pair has enough",
			// The vote table's own verdict: 17-11 is a record, not a result.
			"not enough — no evidence that default is better or worse",
			// The pair's own row, with the floor's reason attached.
			"too few slots to compare", "interleaved-verdict",
		},
		wantNotContains: []string{"has cleared the test (exact"},
	},
	{
		// The other side of the same sentence: one pair with a named lead flips
		// the dashboard from "no result yet" to "at least one pair cleared it".
		tpl:  "eval.html",
		name: "eval.html with a pair that earned a winner",
		data: EvalPage{
			Base: sweepBase,
			// A run, so the page is genuinely populated: the empty-state test
			// requires that no data-empty marker survives a render with rows.
			Runs: []store.EvalRun{{
				ID: 9, RanAt: "2026-10-24 10:00:00", K: 10, Mode: "full",
				Readers: 40, Bookmarks: 900, Evaluated: 37, HeldOut: 370, Hits: 39,
				Recall: 0.1063, NDCG: 0.1888, Coverage: 0.6579, LengthFit: 0.713,
				LengthRuns: 37,
			}},
			Algos: []store.AlgoStanding{{Algo: "hybrid", Judged: 30, Wins: 17, Losses: 11, Ties: 2}},
			Arms: []store.InterleaveArmStat{{
				Arm: "hybrid", Impressions: 200, Credits: 40, CreditRate: 0.20,
			}},
			// Every section, because the two-sided empty-state test requires a
			// populated render to carry no data-empty marker at all -- one
			// unpopulated section would fail it for a reason unrelated to what
			// this case is about.
			SliceRunID: 9,
			SliceMinN:  20,
			FandomSlices: []store.EvalSlice{
				{Axis: "fandom", Name: "selkie au", N: 40, Hits: 12, HitRate: 0.30},
			},
			TagSlices: []store.EvalSlice{
				{Axis: "tag", Name: "slow burn", N: 25, Hits: 8, HitRate: 0.32},
			},
			Works: 112935, Tags: 634231, WorkTags: 2884447, Edges: 2884447,
			Pairs: []store.InterleavePairStat{{
				ArmA: "hybrid", ArmB: "tags", ImpressionsA: 200, ImpressionsB: 200,
				CreditsA: 40, CreditsB: 10, Difference: 0.15,
				P: 0.00001, Leading: "hybrid",
			}},
			AnyLead: true,
		},
		wantContains:    []string{"has cleared the test (exact", "p &lt; 0.05"},
		wantNotContains: []string{"No result yet."},
	},
	{
		tpl:          "eval.html",
		name:         "eval.html with no store and no corpus",
		hasEmptyList: true,
		data:         EvalPage{Base: sweepBase, Err: "no state database here"},
		wantContains: []string{"no state database here", "No offline runs recorded yet"},
		// A metric table with no corpus under it must not print zeros as if
		// they were measurements.
		wantNotContains: []string{"Co-occurrence edges"},
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
	{
		// The family intersection lives on the search page now; these cases
		// came across from browse.html with it.
		name: "search.html with a family match",
		tpl:  "search.html",
		data: SearchPage{
			// No Query: this is a FAMILY-only request, and the text
			// sections must stay off rather than claim an empty search.
			Base:            Base{Title: "T", Heading: "H"},
			FamSearched:     true,
			Universe:        "worm",
			Concept:         "isekai",
			UniverseOptions: []SortOption{{Value: "worm", Label: "Worm (Parahumans)"}},
			ConceptOptions:  []SortOption{{Value: "isekai", Label: "isekai"}},
			RecentDays:      365,
			N:               50,
			Fams: []WorkHit{
				{ID: 1, Title: "W1", Author: "A", Kudos: 10, Hits: 100,
					WordCount: 5000, Language: "en", Complete: 1,
					URL: "https://example.invalid/works/1"},
			},
			FamsMeta: tagfamily.Meta{
				UniverseWorks: 100, ConceptWorks: 20, Matched: 1, Returned: 1, RecentDays: 365,
			},
		},
		wantContains: []string{
			// The counts are on the page, because "1 result" and
			// "0 results" look identical until the reader can see
			// how large each pool was.
			"100", "20", "365",
			// The form is a GET to the SEARCH page -- /browse no longer
			// exists -- so the result is still a shareable URL.
			`action="/search"`, `name="universe"`, `name="concept"`,
			// And the row is the SHARED row template.
			"/work/1", "5,000 words",
		},
		// A populated render must NOT carry the empty marker.
		wantNotContains: []string{"data-empty"},
	},
	{
		// A family search that matched nothing says so, and names the pools
		// rather than claiming the fandom is empty.
		name:         "search.html with no family matches",
		tpl:          "search.html",
		hasEmptyList: true,
		data: SearchPage{
			// No Query: this is a FAMILY-only request, and the text
			// sections must stay off rather than claim an empty search.
			Base:            Base{Title: "T", Heading: "H"},
			FamSearched:     true,
			Universe:        "worm",
			Concept:         "isekai",
			UniverseOptions: []SortOption{{Value: "worm", Label: "Worm (Parahumans)"}},
			ConceptOptions:  []SortOption{{Value: "isekai", Label: "isekai"}},
			RecentDays:      365,
			N:               50,
			Fams:            nil,
			FamsMeta: tagfamily.Meta{
				UniverseWorks: 100, ConceptWorks: 20, Matched: 0, Returned: 0, RecentDays: 365,
			},
		},
		wantContains: []string{
			`data-empty="true"`,
			// The pools are still stated, so the reader can tell
			// "no overlap" from "no data".
			"100", "20",
		},
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

// TestTagPageShowsTheEmbeddingNeighbourhood covers the section #11 asked for.
//
// The page had two ways to be wrong that no other test would catch: rendering
// the section with no rows (an empty list that reads as "nothing sits near
// this tag" — the absence-as-answer failure this tree refuses) and rendering
// ids without names (a similarity nobody can click or act on). Both are
// asserted, and the absence case counts its subject first.
func TestTagPageShowsTheEmbeddingNeighbourhood(t *testing.T) {
	page := TagPage{
		Base: Base{Title: "coffee shop"},
		Tag:  TagInfo{ID: 1, Name: "coffee shop", WorkCount: 40},
		VectorNeighbours: []EmbedNeighbour{
			{ID: 840, Name: "barista", Sim: 0.812},
			{ID: 27, Name: "slow burn", Sim: 0.66},
		},
	}
	body := renderOne(t, "tag.html", page)

	if !strings.Contains(body, "Tags that sit near it") {
		t.Fatal("the embedding section is missing on a page that has neighbours")
	}
	// Count the links before asserting anything about their content: a
	// section that renders its heading and no rows would satisfy a bare
	// "contains the heading" check.
	if n := strings.Count(body, `href="/tag/`); n < 2 {
		t.Fatalf("the embedding section rendered %d tag links, want 2: "+
			"the heading renders without its rows", n)
	}
	for _, want := range []string{`href="/tag/840"`, "barista", "0.81", "slow burn"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the neighbourhood is missing %q", want)
		}
	}
}

// renderLayout executes a page through its LAYOUT, so assertions can see the
// <head> as well as the body. renderOne runs only "content": the layout is
// where <link rel="alternate"> lives, and a test that renders the body cannot
// see a discovery link at all.
func renderLayout(t *testing.T, name string, data any) string {
	t.Helper()
	tpl, ok := pages[name]
	if !ok {
		t.Fatalf("no template set for %s", name)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("rendering %s through its layout: %v", name, err)
	}
	return buf.String()
}

// TestTheWorkPageRendersAPairReport: #78's reader-facing half.
//
// The section is a FORM, not a link, because the other half of the pair is a
// number the reader supplies. A test that only asserted the heading would pass
// on a page that collects the number and does nothing with it, so this asserts
// the report's own numbers and names.
func TestTheWorkPageRendersAPairReport(t *testing.T) {
	page := WorkPage{
		Base: Base{Title: "A Work"},
		Work: corpus.Entity{ID: 1, Title: "A Work"},
		Compare: &engine.PairSimilarity{
			A:          engine.WorkRef{ID: 1, Title: "A Work"},
			B:          engine.WorkRef{ID: 2, Title: "The Other"},
			TagOverlap: 0.5,
			Score:      0.5,
			Missing:    []string{"embed_cosine"},
			SharedTags: []engine.SharedTag{
				{ID: 7, Name: "rare-tag", IDF: 4.5},
				{ID: 8, Name: "common-tag", IDF: 1.2},
			},
		},
	}
	body := renderOne(t, "work.html", page)
	for _, want := range []string{
		"How alike is another work?", `name="compare"`, "The Other",
		"0.50", "rare-tag", "common-tag", "embed_cosine",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the pair report is missing %q", want)
		}
	}
	// The axes that ran are named; the one that did not is explained rather
	// than rendered as a zero.
	if !strings.Contains(body, "Not every axis could be measured") {
		t.Fatal("the missing axis is not explained to the reader")
	}
}

// TestTheWorkPageKeepsTheTypedIDOnError: a mistyped id must not empty the field
// the reader has to correct.
func TestTheWorkPageKeepsTheTypedIDOnError(t *testing.T) {
	page := WorkPage{
		Base:       Base{Title: "A Work"},
		Work:       corpus.Entity{ID: 1, Title: "A Work"},
		CompareID:  "12x",
		CompareErr: "The other work's id must be a number.",
	}
	body := renderOne(t, "work.html", page)
	if !strings.Contains(body, `value="12x"`) {
		t.Fatal("the mistyped id was emptied out of the form")
	}
	if !strings.Contains(body, "must be a number") {
		t.Fatal("the error is not rendered")
	}
}

// TestTagPageOffersItsFeed: a subscription is a thing a reader does once, so
// the page both DISCOVERS the feed (the layout's rel=alternate, which is what
// makes a browser offer it) and links it in words. Both halves matter: the head
// link is invisible, and the visible link is what a reader who does not know
// the scheme can find.
func TestTagPageOffersItsFeed(t *testing.T) {
	page := TagPage{
		Base: Base{Title: "coffee shop", FeedPath: "/feed/tag/1"},
		Tag:  TagInfo{ID: 1, Name: "coffee shop", WorkCount: 40},
	}
	body := renderLayout(t, "tag.html", page)

	if !strings.Contains(body, `type="application/rss+xml"`) {
		t.Fatal("the page head carries no rel=alternate link, so a browser cannot " +
			"offer the feed")
	}
	if !strings.Contains(body, `href="/feed/tag/1"`) {
		t.Fatalf("the feed path is missing from the page; got:\n%s", body)
	}
	if !strings.Contains(body, "Follow this tag") {
		t.Fatal("the visible subscription link is missing: the head link is invisible")
	}
}

// TestApageWithoutAFeedEmitsNoFeedLink: the layout must not advertise a feed
// every page lacks — a rel=alternate pointing at a 404 is worse than none.
func TestAPageWithoutAFeedEmitsNoFeedLink(t *testing.T) {
	body := renderLayout(t, "tag.html", TagPage{
		Base: Base{Title: "coffee shop"},
		Tag:  TagInfo{ID: 1, Name: "coffee shop"},
	})
	if strings.Contains(body, "application/rss+xml") {
		t.Fatal("a page with no feed still emitted a rel=alternate link")
	}
}

// TestTagPageHidesTheEmbeddingSectionWhenThereIsNothingToSay: a deployment
// with no embeddings must not render a heading over an empty list.
func TestTagPageHidesTheEmbeddingSectionWhenThereIsNothingToSay(t *testing.T) {
	page := TagPage{
		Base: Base{Title: "coffee shop"},
		Tag:  TagInfo{ID: 1, Name: "coffee shop", WorkCount: 40},
	}
	body := renderOne(t, "tag.html", page)
	if !strings.Contains(body, "work") {
		t.Fatal("the page did not render at all; an absence assertion here is vacuous")
	}
	if strings.Contains(body, "Tags that sit near it") {
		t.Fatal("the embedding section rendered with no neighbours, which reads as " +
			"\"nothing sits near this tag\" rather than \"this host has no embeddings\"")
	}
}
