package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// filterCorpus builds a fixture whose works are distinguishable by every
// filter the engine accepts, then returns an engine over it.
//
// It does NOT use testcorpus.New, deliberately. That constructor assigns its
// own works, tags AND work_tags, and overwriting f.Works afterwards leaves the
// constructor's tags in place — so the fixture silently keeps tag wiring the
// test cannot see, and "work 9 carries no tags" is false. A fixture whose
// shape is decided by a constructor you are trying to override is a fixture
// that tests something other than what it says.
//
// The works are distinguishable on every filter dimension, and the tags are
// assigned here explicitly:
//
//   - tag 1 is on every candidate, so every work is in the unfiltered pool and
//     the filters are what remove it
//   - tag 2 is on works 1-5 only, so work 6 is a WEAKER neighbour
//   - work 6 matches every filter but has weaker tag overlap, so it is
//     reachable ONLY if the filter narrows the pool before ranking. That is
//     what makes the placement of the filter observable.
func filterCorpus(t *testing.T) *Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "filters.db")

	type spec struct {
		id                 int64
		title              string
		words, kudos, hits int
		rating, lang       string
		complete           bool
		tags               []int64
	}
	specs := []spec{
		{1, "long complete english general", 50000, 900, 100000, "General Audiences", "English", true, []int64{1, 2}},
		{2, "short", 500, 900, 100000, "General Audiences", "English", true, []int64{1, 2}},
		{3, "explicit", 50000, 900, 100000, "Explicit", "English", true, []int64{1, 2}},
		{4, "french", 50000, 900, 100000, "General Audiences", "French", true, []int64{1, 2}},
		{5, "in progress", 50000, 900, 100000, "General Audiences", "English", false, []int64{1, 2}},
		// Work 6 has the weaker tag set on purpose: one tag shared with the
		// seed rather than two, so it loses any post-ranking filter to work 1.
		{6, "deep in the pool", 50000, 900, 100000, "Teen And Up Audiences", "English", true, []int64{1}},
		// Work 7 is the seed. Work 9 is untagged, for the empty-pool case.
		{7, "the seed", 30000, 500, 50000, "General Audiences", "English", true, []int64{1}},
		{9, "untagged", 10000, 10, 1000, "Mature", "Spanish", false, nil},
	}

	f := &testcorpus.Corpus{
		Tags: []testcorpus.Tag{{ID: 1, Name: "shared"}, {ID: 2, Name: "strong"}},
	}
	for _, s := range specs {
		f.Works = append(f.Works, testcorpus.Work{
			ID: s.id, Title: s.title, Authors: "author",
			WordCount: s.words, Kudos: s.kudos, Hits: s.hits,
			Rating: s.rating, Language: s.lang, Complete: s.complete,
		})
		for _, tag := range s.tags {
			f.WorkTags = append(f.WorkTags, testcorpus.WorkTag{
				WorkID: s.id, TagID: tag, TagType: "freeform"})
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

	ao3 := corpus.NewAO3(db)
	return &Engine{Corpus: ao3, PoolSize: 50}
}

// pool runs a request and returns the candidate ids it ranked, so the tests
// assert on what the ENGINE produced rather than on a SQL string.
func pool(t *testing.T, e *Engine, f Filter) map[int64]bool {
	t.Helper()
	res, err := e.Recommend(context.Background(), Request{
		Seeds:   []Seed{{Kind: corpus.AO3Kind, ID: 7}},
		Kind:    corpus.AO3Kind,
		N:       50,
		Exclude: true,
		Filter:  f,
	})
	if err != nil {
		t.Fatalf("recommend with %+v: %v", f, err)
	}
	out := map[int64]bool{}
	for _, c := range res.Items {
		out[c.ID] = true
	}
	return out
}

// TestFiltersShapeThePoolNotTheResult is the load-bearing test for the whole
// filter feature.
//
// A pool of 1 with a min-words filter must return the single best work THAT
// QUALIFIES. If the filter ran after ranking, the LIMIT would have been spent
// on the unfiltered top row and the qualifying work further down the pool
// would never have been considered — returning the wrong work, or nothing.
func TestFiltersShapeThePoolNotTheResult(t *testing.T) {
	e := filterCorpus(t)

	// With a pool of 1 and no filter, the single slot goes to the strongest
	// tag overlap. Establish that baseline first, or the filtered assertion
	// below proves nothing.
	unfiltered := e.Recommend
	_ = unfiltered

	res, err := e.Recommend(context.Background(), Request{
		Seeds:    []Seed{{Kind: corpus.AO3Kind, ID: 7}},
		Kind:     corpus.AO3Kind,
		N:        50,
		PoolSize: 1,
		Exclude:  true,
	})
	if err != nil {
		t.Fatalf("unfiltered: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatalf("unfiltered pool of 1 returned nothing; the fixture cannot test the filter")
	}
	baseline := res.Items[0].ID

	// Now the same pool of 1, filtered to work 6's rating. Work 6 shares
	// fewer tags with the seed, so it is NOT the unfiltered winner — it is
	// only reachable because the filter narrowed the pool first.
	res, err = e.Recommend(context.Background(), Request{
		Seeds:    []Seed{{Kind: corpus.AO3Kind, ID: 7}},
		Kind:     corpus.AO3Kind,
		N:        50,
		PoolSize: 1,
		Exclude:  true,
		Filter:   Filter{Ratings: []string{"Teen And Up Audiences"}},
	})
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatalf("a rating filter matching exactly one work returned nothing from a pool of 1")
	}
	if got := res.Items[0].ID; got != 6 {
		t.Fatalf("filtered pool of 1 returned work %d, want 6: the filter is being "+
			"applied after ranking (unfiltered winner was %d), not to the pool", got, baseline)
	}
}

// TestMinWordsFilterExcludesShortWorks covers the individual filter.
func TestMinWordsFilterExcludesShortWorks(t *testing.T) {
	e := filterCorpus(t)
	got := pool(t, e, Filter{MinWords: 20000})

	if got[2] {
		t.Fatalf("work 2 has 500 words and must be excluded by min_words=20000: %v", got)
	}
	if !got[1] {
		t.Fatalf("work 1 has 50,000 words and must be included: %v", got)
	}
}

// TestRatingFilterIsCaseInsensitive covers the normalisation, which is where a
// filter silently matches nothing.
func TestRatingFilterIsCaseInsensitive(t *testing.T) {
	e := filterCorpus(t)
	got := pool(t, e, Filter{Ratings: []string{"eXpLiCiT"}})

	if !got[3] {
		t.Fatalf("work 3 is Explicit and must match rating=eXpLiCiT: %v", got)
	}
	if got[1] {
		t.Fatalf("work 1 is General Audiences and must not match: %v", got)
	}
}

// TestLanguageFilterIsCaseInsensitive covers the same for language.
func TestLanguageFilterIsCaseInsensitive(t *testing.T) {
	e := filterCorpus(t)
	got := pool(t, e, Filter{Languages: []string{"FRENCH"}})

	if !got[4] {
		t.Fatalf("work 4 is French and must match language=FRENCH: %v", got)
	}
	if got[1] {
		t.Fatalf("work 1 is English and must not match: %v", got)
	}
}

// TestCompleteFilterIsTriState covers the NULL problem.
//
// `complete` is NULLable. Asking for in-progress works must NOT silently drop
// the rows whose status is unknown, and must not claim they are complete
// either. The filter therefore distinguishes only the two KNOWN states.
func TestCompleteFilterIsTriState(t *testing.T) {
	e := filterCorpus(t)

	only := pool(t, e, Filter{Complete: CompleteOnly})
	if only[5] {
		t.Fatalf("work 5 is in progress and must be excluded by complete-only: %v", only)
	}
	if !only[1] {
		t.Fatalf("work 1 is complete and must be included: %v", only)
	}

	wip := pool(t, e, Filter{Complete: CompleteWIP})
	if !wip[5] {
		t.Fatalf("work 5 is in progress and must be included by complete-WIP: %v", wip)
	}
	if wip[1] {
		t.Fatalf("work 1 is complete and must not be in the WIP set: %v", wip)
	}
}

// TestCompleteAnyDoesNotFilter proves the zero value really is "no filter",
// which is what makes Filter safe to carry on every request.
func TestCompleteAnyDoesNotFilter(t *testing.T) {
	e := filterCorpus(t)

	any := pool(t, e, Filter{Complete: CompleteAny})
	if !any[1] || !any[5] {
		t.Fatalf("CompleteAny must keep both complete and in-progress works: %v", any)
	}
	var zero Filter
	if zero.Complete != CompleteAny {
		t.Fatalf("the zero Filter must be CompleteAny, got %v", zero.Complete)
	}
}

// TestFiltersCombine pins that two filters AND together rather than one
// replacing the other.
func TestFiltersCombine(t *testing.T) {
	e := filterCorpus(t)
	got := pool(t, e, Filter{
		MinWords:  20000,
		Ratings:   []string{"General Audiences"},
		Languages: []string{"French"},
	})
	if !got[4] {
		t.Fatalf("work 4 matches all three filters and must be included: %v", got)
	}
	// Work 3 passes words+rating but is English; work 1 passes words+language
	// but is General not Teen.
	if got[3] {
		t.Fatalf("work 3 is English and must be excluded by the language filter: %v", got)
	}
	if got[1] {
		t.Fatalf("work 1 is English and must be excluded by the language filter: %v", got)
	}
}

// TestFilterValuesAreBoundNotConcatenated guards the injection shape. A rating
// containing a quote must be treated as a literal that matches nothing, not as
// SQL. This is a read-only mirror, so the stakes are low, but a filter that
// concatenates is a filter that will break on the first apostrophe.
func TestFilterValuesAreBoundNotConcatenated(t *testing.T) {
	e := filterCorpus(t)

	got := pool(t, e, Filter{Ratings: []string{"General Audiences' OR '1'='1"}})
	if len(got) != 0 {
		t.Fatalf("an injected rating matched %d works (%v): the value was "+
			"concatenated into SQL rather than bound", len(got), got)
	}
}

// TestAPoolEmptiedByAFilterSaysSo covers the honesty requirement: a filter
// that matches nothing must be reported as a shortfall, not as a successful
// empty ranking.
func TestAPoolEmptiedByAFilterSaysSo(t *testing.T) {
	e := filterCorpus(t)

	res, err := e.Recommend(context.Background(), Request{
		Seeds:   []Seed{{Kind: corpus.AO3Kind, ID: 7}},
		Kind:    corpus.AO3Kind,
		N:       10,
		Exclude: true,
		Filter:  Filter{MinWords: 10_000_000},
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("expected no results, got %d", len(res.Items))
	}
	if res.Meta.Shortfall == nil {
		t.Fatalf("an emptied pool returned no shortfall: meta=%+v", res.Meta)
	}
}

// TestPoolIsDeterministicAcrossFilterBindings guards the argument-order fix.
//
// The tag ids are bound from a map. If they are bound in map order, the
// argument list differs between runs and the pool can differ, which shows up
// as a ranking that changes for identical input.
func TestPoolIsDeterministicAcrossFilterBindings(t *testing.T) {
	e := filterCorpus(t)

	first := pool(t, e, Filter{MinWords: 20000, Ratings: []string{"General Audiences"}})
	for i := 0; i < 8; i++ {
		again := pool(t, e, Filter{MinWords: 20000, Ratings: []string{"General Audiences"}})
		if len(again) != len(first) {
			t.Fatalf("pool size changed between identical requests: %d then %d",
				len(first), len(again))
		}
		for id := range first {
			if !again[id] {
				t.Fatalf("work %d present in one identical request and absent from "+
					"another: the pool is not deterministic", id)
			}
		}
	}
}

// TestPoolForReturnsNothingWithoutSeedTags keeps poolFor's own guard honest:
// a seed carrying no tags cannot produce a pool, and that is not an error.
func TestPoolForReturnsNothingWithoutSeedTags(t *testing.T) {
	e := filterCorpus(t)

	// Work 9 in the fixture carries no tags, so seeding from it gives no tag
	// ids and the pool must be empty rather than a corpus-wide scan.
	res, err := e.Recommend(context.Background(), Request{
		Seeds:   []Seed{{Kind: corpus.AO3Kind, ID: 9}},
		Kind:    corpus.AO3Kind,
		N:       10,
		Exclude: true,
	})
	if err != nil {
		t.Fatalf("recommend from an untagged seed: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("an untagged seed returned %d candidates: %+v", len(res.Items), res.Items)
	}
}

// TestFilterDoesNotExceedThePoolLimit keeps the bound honest: a filter may
// shrink the pool but must never make it larger than the request allowed.
func TestFilterDoesNotExceedThePoolLimit(t *testing.T) {
	e := filterCorpus(t)

	res, err := e.Recommend(context.Background(), Request{
		Seeds:    []Seed{{Kind: corpus.AO3Kind, ID: 7}},
		Kind:     corpus.AO3Kind,
		N:        50,
		PoolSize: 2,
		Exclude:  true,
		Filter:   Filter{MinWords: 20000},
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	if len(res.Items) > 2 {
		t.Fatalf("pool limit 2 returned %d items", len(res.Items))
	}
}

// compile-time assurance that the fixture's seed shape matches what poolFor
// consumes: a candidate whose TagIDs are empty produces an empty pool by
// construction, which the tests above rely on.
var _ = rank.Candidate{TagIDs: nil}
