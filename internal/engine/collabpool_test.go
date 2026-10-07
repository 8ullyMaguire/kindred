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
