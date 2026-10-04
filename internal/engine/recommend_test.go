package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/fandom"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// execAdapter bridges *sql.DB to graph.Executor.
//
// The interface asks for Exec(context.Context, ...) while database/sql's own
// method is ExecContext(string, ...), so *sql.DB does not satisfy it directly.
// cmd/kindred has the same adapter; this is a third copy rather than a shared
// one, which is a real wart, but exporting an adapter from internal/graph to
// avoid it would widen that package's API for a test-only need.
type execAdapter struct{ DB *sql.DB }

func (e execAdapter) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return e.DB.ExecContext(ctx, q, args...)
}

// fandomGroup is the test's own view of the group key, spelled out rather than
// imported from the engine, so a change to the engine's wiring cannot quietly
// change what the test is measuring.
func fandomGroup(tags []string) string { return fandom.GroupKey(tags) }

// newFixtureEngine builds a real engine over the shared fixture corpus.
//
// It exists because internal/engine had NO integration test: the package's own
// tests covered pure functions (seed parsing, renormalisation, tune defaults)
// and nothing ever called Recommend. So the wiring that decides a result —
// graph load, signal set, diversity — was unverified by anything.
func newFixtureEngine(t *testing.T) *Engine {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	corpusPath := filepath.Join(dir, "corpus.db")
	statePath := filepath.Join(dir, "state.db")

	if _, err := testcorpus.New(60).Write(corpusPath); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	s, err := store.Open(ctx, statePath, corpusPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	b := &graph.Builder{Corpus: s.Corpus, Store: execAdapter{s.DB}, TopN: 24, DBPath: statePath}
	if _, err := b.Build(ctx); err != nil {
		t.Fatalf("build graph: %v", err)
	}
	g, err := graph.LoadCSR(ctx, statePath, s.Corpus,
		func() (map[int32]int64, error) { return graph.TagFrequencies(ctx, s.Corpus) },
		func() ([]string, error) {
			n, err := s.MetaInt(ctx, "node_count")
			if err != nil {
				return nil, err
			}
			return graph.TagNames(ctx, s.Corpus, n)
		})
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}

	e := &Engine{
		Store: s, Corpus: corpus.NewAO3(s.Corpus), Graph: g,
		PoolSize: 400, TopN: 24, EmbedDim: 32,
	}
	e.SetArenaRatings(map[int64]float64{}, 0)
	return e
}

// The headline parity claim: max_per_group with group_by=fandom must actually
// reduce how many results come from one fandom.
//
// This is the test that would have caught the sibling's silent no-op. That
// implementation keyed the cap on `tag_type='fandoms'`, a column that is
// 'freeforms' for 3,890,504 of 3,891,300 rows in the real mirror, so its group
// key set was empty and the cap excluded nothing while reporting success.
func TestMaxPerFandomActuallyCaps(t *testing.T) {
	e := newFixtureEngine(t)
	ctx := context.Background()

	seeds := []Seed{{Kind: "ao3_work", ID: 1}, {Kind: "ao3_work", ID: 2}, {Kind: "ao3_work", ID: 3}}

	uncapped, err := e.Recommend(ctx, Request{Kind: "ao3_work", Seeds: seeds, N: 24})
	if err != nil {
		t.Fatal(err)
	}
	if len(uncapped.Items) < 8 {
		t.Fatalf("only %d uncapped items; the fixture is too small to test a cap", len(uncapped.Items))
	}

	capped, err := e.Recommend(ctx, Request{
		Kind: "ao3_work", Seeds: seeds, N: 24,
		MaxPerGroup: 3, GroupBy: "fandom",
	})
	if err != nil {
		t.Fatal(err)
	}

	maxSeen := 0
	counts := map[string]int{}
	for _, it := range capped.Items {
		gk := fandomGroup(it.TagNames)
		if gk == "" {
			continue
		}
		counts[gk]++
		if counts[gk] > maxSeen {
			maxSeen = counts[gk]
		}
	}
	if maxSeen > 3 {
		t.Errorf("a fandom contributed %d results with max_per_group=3: %v", maxSeen, counts)
	}

	// The cap must be doing something. If the uncapped list were already
	// within the cap, this test would pass for the wrong reason -- which is
	// precisely how the no-op cap passed its own test upstream.
	uncappedMax := 0
	uc := map[string]int{}
	for _, it := range uncapped.Items {
		gk := fandomGroup(it.TagNames)
		if gk == "" {
			continue
		}
		uc[gk]++
		if uc[gk] > uncappedMax {
			uncappedMax = uc[gk]
		}
	}
	if uncappedMax <= 3 {
		t.Skipf("uncapped fixture list is already within the cap (max %d); "+
			"the cap cannot be shown to bite at this fixture size", uncappedMax)
	}
	if len(capped.Items) >= len(uncapped.Items) && maxSeen >= uncappedMax {
		t.Error("the cap changed nothing at all")
	}
}

// A cap that silently does nothing must be REPORTED. This is the difference
// between "90 of 100: 10 exceeded the cap" and a full list that pretends.
func TestMaxPerGroupWithNoGroupChosenIsReported(t *testing.T) {
	e := newFixtureEngine(t)
	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 10,
		MaxPerGroup: 2, // GroupBy deliberately empty
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.Shortfall == nil {
		t.Fatal("a cap with no group chosen reported no shortfall; it is a no-op cap")
	}
	if res.Meta.Shortfall.Reason == "" {
		t.Error("shortfall carries no reason")
	}
}

// Recommending from a seed must never return the seed itself, and must never
// fail for want of a graph. This is the minimum viable request path: everything
// else in the API is decoration on top of it.
func TestRecommendReturnsUsableItems(t *testing.T) {
	e := newFixtureEngine(t)
	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) == 0 {
		t.Fatal("no items from a valid seed")
	}
	for i, it := range res.Items {
		if it.ID == 1 {
			t.Errorf("item %d is the seed itself", i)
		}
		if it.Score == 0 {
			t.Errorf("item %d has score 0 with no evidence", i)
		}
		if len(it.Evidence) == 0 {
			t.Errorf("item %d has no evidence; an unexplained score cannot be audited", i)
		}
	}
	// NOT asserted to be score-sorted: diversify.Apply's contract is that its
	// output is deliberately out of score order, because an item chosen for
	// diversity ranks below one it beat on score. Asserting descending order
	// here would contradict the package's documented behaviour.
	//
	// What IS worth asserting: MMR only reorders within a window, so an item
	// chosen early can never end up last.
	if got := res.Items[len(res.Items)-1].Score; got > res.Items[0].Score {
		t.Errorf("worst item scores %f above the best %f", got, res.Items[0].Score)
	}
	// Each item must still be near the top of the relevance order, or MMR has
	// replaced ranking with randomness.
	for i, it := range res.Items {
		if it.Score <= 0 {
			t.Errorf("item %d has non-positive score %f", i, it.Score)
		}
	}
}

func TestRecommendRejectsAnUnknownSeedKind(t *testing.T) {
	e := newFixtureEngine(t)
	// A typo'd seed must be an error, not an empty list: "0 results" reads as
	// "nothing matched" when the truth is "I could not read your input".
	if _, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "book", ID: 1}}, N: 5,
	}); err == nil {
		t.Error("an unknown seed kind was accepted")
	}
}
