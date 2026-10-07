package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/collab"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// collabCorpus builds a fixture where the tag pool and the collab votes
// disagree, which is the only way to tell union from replacement.
//
// The shape is deliberate and it encodes the whole argument for PoolMode:
//
//   - the SEED carries tag 1 and nothing else
//   - works 1 and 2 carry tag 1, so they enter a TAG pool
//   - works 3 and 4 carry NO tag the seed has, so they can ONLY enter via
//     co-bookmark evidence — these are the works that exist for the sake of
//     the feature, the ones a tag pool structurally cannot reach
//   - five users bookmark the seed together with works 3 and 4
//
// If poolFor REPLACED the tag pool with collab neighbours, works 1 and 2
// would vanish. If it UNIONed, all four appear. Both behaviours look
// plausible and only one of them is the documented contract, so the test
// has to be able to fail.
func collabCorpus(t *testing.T) *Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "collabpool.db")

	f := &testcorpus.Corpus{
		Tags: []testcorpus.Tag{{ID: 1, Name: "shared"}},
	}
	add := func(id int64, title string, tags ...int64) {
		f.Works = append(f.Works, testcorpus.Work{
			ID: id, Title: title, Authors: "author",
			WordCount: 20000, Kudos: 100, Hits: 1000,
			Rating: "General Audiences", Language: "English", Complete: true,
		})
		for _, tag := range tags {
			f.WorkTags = append(f.WorkTags, testcorpus.WorkTag{
				WorkID: id, TagID: tag, TagType: "freeform"})
		}
	}
	add(10, "the seed", 1)
	add(1, "tag reachable", 1)
	add(2, "also tag reachable", 1)
	add(3, "collab only", 2)
	add(4, "collab only too", 2)

	// Filler works carrying the seed's tag, and deliberately NO bookmarks.
	//
	// These exist to make the budget split OBSERVABLE, and their count is the
	// point. With only works 1 and 2 reachable by tag, a pool of 4 was never
	// full — `room := limit - len(out)` stayed positive even with the tag half
	// capped at the FULL budget, so reverting that cap changed nothing and
	// every test passed. Measured: with 4 tag-reachable works, `PoolSize=4`
	// returned [1 3 4 2] under both the correct code and the shipped bug.
	//
	// Twenty filler works make any small pool completely full from tags alone,
	// so the reserve is the only thing that can let a collab-only work in. A
	// fixture smaller than the pool under test cannot distinguish "the budget
	// was split" from "there was room anyway".
	for i := int64(100); i < 120; i++ {
		add(i, "tag filler", 1)
	}

	// Five users co-bookmark the seed with both collab-only works. Five is
	// above collab.FallbackMinCoUsers so the pairs are indexed at any gate.
	for u := int64(1); u <= 5; u++ {
		f.Users = append(f.Users, testcorpus.User{ID: u, Username: "reader"})
		for _, w := range []int64{10, 3, 4} {
			f.Interactions = append(f.Interactions, testcorpus.Interaction{
				UserID: u, WorkID: w, InteractionType: "bookmarked"})
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
	idx, _, err := (&collab.Builder{Corpus: db}).Build(context.Background())
	if err != nil {
		t.Fatalf("build collab index: %v", err)
	}
	// Sanity: the fixture must actually produce collab evidence, or the test
	// below would pass for the reason that matters most of all — that there
	// was nothing to union.
	if len(idx.NeighboursFor([]int64{10}, collab.DefaultNeighbours, collab.Options{})) == 0 {
		t.Fatal("the fixture produced no collab neighbours for the seed, so " +
			"the union cannot be observed")
	}
	return &Engine{Corpus: ao3, Collab: idx, PoolSize: 50}
}

// TestTagPoolAndCollabVotesAreUnionedNotReplaced is the load-bearing test
// for PoolTagsOrCollab.
//
// The mutation check is what makes it worth having: rewriting the union as
// a replacement (`out = collabCandidates(...)`) left the whole package's
// tests green, because nothing else in the suite pins the difference. A
// documented design decision that no test can falsify is a comment, not a
// contract.
func TestTagPoolAndCollabVotesAreUnionedNotReplaced(t *testing.T) {
	e := collabCorpus(t)

	req := Request{
		Seeds:   []Seed{{Kind: corpus.AO3Kind, ID: 10}},
		Kind:    corpus.AO3Kind,
		N:       50,
		Exclude: true,
	}

	t.Run("tags only", func(t *testing.T) {
		res, err := e.Recommend(context.Background(), req)
		if err != nil {
			t.Fatalf("tags only: %v", err)
		}
		got := map[int64]bool{}
		for _, c := range res.Items {
			got[c.ID] = true
		}
		if !got[1] || !got[2] {
			t.Fatalf("the tag pool lost the tag-sharing works: %v", got)
		}
		if got[3] || got[4] {
			t.Fatalf("the tag pool returned the collab-only works, which share "+
				"no tag with the seed: %v", got)
		}
	})

	t.Run("tags or collab", func(t *testing.T) {
		req.PoolMode = PoolTagsOrCollab
		res, err := e.Recommend(context.Background(), req)
		if err != nil {
			t.Fatalf("tags or collab: %v", err)
		}
		got := map[int64]bool{}
		for _, c := range res.Items {
			got[c.ID] = true
		}
		for _, id := range []int64{1, 2, 3, 4} {
			if !got[id] {
				t.Fatalf("work %d is missing from the unioned pool (%v): the tag "+
					"half and the collab half must both survive", id, got)
			}
		}
		if got[10] {
			t.Fatalf("the seed itself was recommended: %v", got)
		}
	})
}

// TestCollabDoesNotResurrectASeed is the exclusion invariant on the collab
// path. The tag pool filters seeds in SQL; the collab half is a vote map
// assembled elsewhere, so seed exclusion has to hold there independently.
func TestCollabDoesNotResurrectASeed(t *testing.T) {
	e := collabCorpus(t)

	res, err := e.Recommend(context.Background(), Request{
		Seeds:    []Seed{{Kind: corpus.AO3Kind, ID: 10}},
		Kind:     corpus.AO3Kind,
		N:        50,
		Exclude:  true,
		PoolMode: PoolTagsOrCollab,
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	for _, c := range res.Items {
		if c.ID == 10 {
			t.Fatalf("the seed came back through the collab half: %v", res.Items)
		}
	}
}

// TestBlocksApplyToTheCollabHalf pins that blocking is honoured on BOTH
// halves of the union.
//
// collabCandidates has its own SQL path with no filterSQL/blockSQL of the
// pool's, so a block wired only into poolFor would leave works re-entering
// through collaborative filtering — the reader blocks a tag, sees it gone
// from the tag half, and has no way to know it is still in the list.
func TestBlocksApplyToTheCollabHalf(t *testing.T) {
	e := collabCorpus(t)

	// Tag 2 is on works 3 and 4 — exactly the collab-only half.
	res, err := e.Recommend(context.Background(), Request{
		Seeds:         []Seed{{Kind: corpus.AO3Kind, ID: 10}},
		Kind:          corpus.AO3Kind,
		N:             50,
		Exclude:       true,
		PoolMode:      PoolTagsOrCollab,
		BlockedTagIDs: map[int64]bool{2: true},
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	for _, c := range res.Items {
		if c.ID == 3 || c.ID == 4 {
			t.Fatalf("work %d carries blocked tag 2 and re-entered through the "+
				"collab half: %v", c.ID, res.Items)
		}
	}
	// The tag half must survive the same block.
	sawTagHalf := false
	for _, c := range res.Items {
		if c.ID == 1 || c.ID == 2 {
			sawTagHalf = true
		}
	}
	if !sawTagHalf {
		t.Fatalf("blocking the collab half also removed the tag half: %v", res.Items)
	}
}

// TestCollabActuallyGetsPoolSlots is the test whose absence let a no-op ship.
//
// The tag pool was capped at the FULL budget, so it filled every slot and the
// union's `room := limit - len(out)` was always zero. Every test in this file
// passed, because they asserted on WHICH works appeared, not on the pool
// being big enough for both halves: with 4 tag-reachable works in a
// 50-slot pool, both halves fit either way.
//
// It was caught by running the real mirror, where `pool_mode=tags` and
// `pool_mode=tags+collab` returned byte-identical lists and none of the
// seed's known co-bookmarked neighbours appeared in either.
//
// So this pins the pool SHAPE: a small explicit pool must contain BOTH
// halves, which is only true when the budget was actually split.
func TestCollabActuallyGetsPoolSlots(t *testing.T) {
	e := collabCorpus(t)

	// Pool 8 against 23 tag-reachable works, so the tag half FILLS its budget
	// and the reserved room is what admits the collab half.
	//
	// The seed is excluded from the pool, so the tag half can only ever fill
	// `limit-1` slots — which is why `PoolSize=4` cannot distinguish the two
	// implementations (measured: correct code added 2 collab works, the
	// full-budget bug added 1, and both returned the same two ids because
	// there was room either way). At 8 the tag half fills its budget and the
	// difference is the reserve alone.
	const poolSize = 8
	res, err := e.Recommend(context.Background(), Request{
		Seeds:    []Seed{{Kind: corpus.AO3Kind, ID: 10}},
		Kind:     corpus.AO3Kind,
		N:        50,
		PoolSize: poolSize,
		Exclude:  true,
		PoolMode: PoolTagsOrCollab,
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	got := map[int64]bool{}
	for _, c := range res.Items {
		got[c.ID] = true
	}
	// The tag half must survive: a reserve equal to the budget would make
	// PoolTagsOrCollab mean "collab only" under a name that promises
	// otherwise.
	if !got[1] && !got[2] {
		t.Fatalf("no tag-reachable work in a pool of %d under PoolTagsOrCollab: "+
			"the collab half consumed the whole budget. got %v", poolSize, got)
	}
	// The collab half must have got AT LEAST the reserved share. With 2
	// collab-only works and a reserve of 1, both fit; the assertion is that
	// they arrived at all, which requires room the tag half would otherwise
	// have taken.
	if !got[3] && !got[4] {
		t.Fatalf("no collab-only work in a pool of %d under PoolTagsOrCollab: "+
			"the union contributed nothing, so the mode is a no-op. got %v",
			poolSize, got)
	}
}

// TestTagsOnlyLeavesTheWholeBudgetToTags pins the other side: the reserve is
// applied ONLY in collab mode. Without this, a fix that reserved room
// unconditionally would shrink every default request's pool.
func TestTagsOnlyLeavesTheWholeBudgetToTags(t *testing.T) {
	e := collabCorpus(t)

	res, err := e.Recommend(context.Background(), Request{
		Seeds:    []Seed{{Kind: corpus.AO3Kind, ID: 10}},
		Kind:     corpus.AO3Kind,
		N:        50,
		PoolSize: 8,
		Exclude:  true,
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	for _, c := range res.Items {
		if c.ID == 3 || c.ID == 4 {
			t.Fatalf("tags-only mode returned the collab-only work %d", c.ID)
		}
	}
}

// TestCollabReserveNeverStarvesTheTagHalf pins the arithmetic, including the
// small-pool cases where the share cannot be taken.
//
// A reserve equal to the budget would make the tag half query zero rows and
// the mode would be "collab only" under a name that promises otherwise.
func TestCollabReserveNeverStarvesTheTagHalf(t *testing.T) {
	for _, limit := range []int{0, 1, 2, 3, 4, 7, 8, 9, 200, 1000} {
		got := collabReserve(limit)
		if got < 0 {
			t.Fatalf("collabReserve(%d) = %d, negative", limit, got)
		}
		if got > limit-1 && limit > 1 {
			t.Fatalf("collabReserve(%d) = %d leaves no slot for the tag half",
				limit, got)
		}
	}
	// A pool too small to divide gives tags everything, rather than
	// coin-flipping the one slot.
	for _, limit := range []int{1, 2} {
		if got := collabReserve(limit); got != 0 {
			t.Fatalf("collabReserve(%d) = %d, want 0: a pool this small "+
				"cannot be split", limit, got)
		}
	}
	// And it does actually reserve something on a pool big enough to share.
	if collabReserve(200) == 0 {
		t.Fatal("collabReserve(200) = 0: a 200-pool reserves nothing")
	}
	if collabReserve(8) != 1 {
		t.Fatalf("collabReserve(8) = %d, want 1", collabReserve(8))
	}
}

// TestACollabOnlySeedStillRecommends is the case the early return in poolFor
// used to make unreachable.
//
// A seed with no tags has an empty tag id set, and poolFor returned nil
// immediately in that case. A tagless seed is precisely the case
// collaborative filtering exists for — a work sharing no tag with anything
// the seed set already covers — so that early return made the whole feature
// unreachable for exactly the readers it was built for.
func TestACollabOnlySeedStillRecommends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tagless.db")
	f := &testcorpus.Corpus{}
	add := func(id int64, title string) {
		f.Works = append(f.Works, testcorpus.Work{
			ID: id, Title: title, Authors: "author",
			WordCount: 20000, Kudos: 100, Hits: 1000,
			Rating: "General Audiences", Language: "English", Complete: true,
		})
	}
	add(20, "tagless seed")
	add(21, "co-bookmarked")
	for u := int64(1); u <= 5; u++ {
		f.Users = append(f.Users, testcorpus.User{ID: u, Username: "reader"})
		for _, w := range []int64{20, 21} {
			f.Interactions = append(f.Interactions, testcorpus.Interaction{
				UserID: u, WorkID: w, InteractionType: "bookmarked"})
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
	idx, _, err := (&collab.Builder{Corpus: db}).Build(context.Background())
	if err != nil {
		t.Fatalf("build collab: %v", err)
	}
	e := &Engine{Corpus: ao3, Collab: idx, PoolSize: 50}

	res, err := e.Recommend(context.Background(), Request{
		Seeds:    []Seed{{Kind: corpus.AO3Kind, ID: 20}},
		Kind:     corpus.AO3Kind,
		N:        50,
		Exclude:  true,
		PoolMode: PoolTagsOrCollab,
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	found := false
	for _, c := range res.Items {
		if c.ID == 21 {
			found = true
		}
	}
	if !found {
		t.Fatalf("a tagless seed recommended nothing even though work 21 is "+
			"co-bookmarked with it: the tagless early return is still in place. "+
			"got %v", res.Items)
	}
}

// TestThePoolIsSplitByBudgetNotByLuck pins the arithmetic that the live
// mirror exposed, and it is the assertion that makes the two implementations
// distinguishable at any pool size.
//
// The tag half is capped at `limit - collabReserve(limit)`, so it can NEVER
// return more than that. Reverting the cap to `limit` is the shipped bug; it
// is invisible whenever the tag pool happens to be thin, which is why the
// first version of this test passed against it. Asserting the CAP rather
// than the resulting ids removes the dependency on how many tag-reachable
// works the fixture happens to have.
//
// It goes through poolFor rather than Recommend because Recommend drops
// zero-scoring candidates, so the pool's size is not observable from the
// result list.
func TestThePoolIsSplitByBudgetNotByLuck(t *testing.T) {
	e := collabCorpus(t)
	ctx := context.Background()

	seedEnts, err := e.Corpus.CandidateRows(ctx, []int64{10})
	if err != nil {
		t.Fatalf("load seeds: %v", err)
	}
	seeds, err := e.toCandidates(ctx, seedEnts)
	if err != nil {
		t.Fatalf("load seed tags: %v", err)
	}
	seedIDs := []int64{10}
	req := Request{PoolMode: PoolTagsOrCollab}

	for _, limit := range []int{8, 16, 30} {
		req.PoolSize = limit
		pool, _, err := e.poolFor(ctx, seeds, limit, true, seedIDs, req)
		if err != nil {
			t.Fatalf("poolFor(limit=%d): %v", limit, err)
		}
		tagBudget := limit - collabReserve(limit)

		// Count how many pool entries are collab-only works (3 and 4 are the
		// only ones reachable exclusively through bookmarks).
		collabOnly := 0
		for _, id := range pool {
			if id == 3 || id == 4 {
				collabOnly++
			}
		}
		if collabOnly == 0 {
			t.Errorf("limit=%d: the pool contains no collab-only work at all, so "+
				"PoolTagsOrCollab admitted nothing from collaborative filtering. "+
				"pool=%v", limit, pool)
		}
		// The cap IS the contract, so assert it directly instead of
		// inferring it from the resulting ids.
		//
		// Counting tag-reachable works in the pool is the honest check, and
		// it is what fails: with `exclude` on, the seed is filtered in SQL so
		// the tag half tops out at tagBudget-1 whenever the fixture has
		// enough tag works, and at min(tagBudget, available) otherwise.
		// Either way it never EXCEEDS tagBudget, and with the full-budget bug
		// it reaches limit-1 — so the bound that separates the two
		// implementations is `tagBudget`, not `limit`.
		tagReachable := 0
		for _, id := range pool {
			if id != 3 && id != 4 {
				tagReachable++
			}
		}
		if tagReachable > tagBudget {
			t.Errorf("limit=%d: %d tag-reachable works in the pool, above the "+
				"budget of %d reserved for them. The tag half is capped at the "+
				"FULL pool, which is what makes the collab union unreachable. "+
				"pool=%v", limit, tagReachable, tagBudget, pool)
		}
		if collabOnly > collabReserve(limit)+1 {
			t.Errorf("limit=%d: %d collab-only works in the pool, more than the "+
				"reserved share of %d could admit", limit, collabOnly,
				collabReserve(limit))
		}
	}
}
