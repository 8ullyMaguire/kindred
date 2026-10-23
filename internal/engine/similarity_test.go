package engine

// Work-pair similarity (#78).
//
// The tests are built around falsifiability, which for this endpoint means two
// things a plausible implementation gets wrong:
//
//   - **The IDF weighting must actually be there.** A plain Jaccard index over
//     tag sets gives two works sharing "fluff" the same score as two sharing a
//     tag only they carry. The fixture therefore has one RARE tag (on two works)
//     and one COMMON tag (on ten), with pairs that share exactly one tag each and
//     unions of the same size — so the only difference between the two pairs is
//     the rarity of what they share.
//
//   - **A missing axis must be named, not scored as zero.** Without embeddings
//     the score has to be the tag overlap alone and `missing` has to say
//     `embed_cosine`; otherwise every pair on a mirror that was never embedded
//     looks half as alike as the same pair on one that was.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// similarityCorpus is the fixture described above.
//
//	11, 12   share the RARE tag (df 2), plus one unique filler each
//	13, 14   share the COMMON tag (df 10), plus one unique filler each
//	15..22   carry the COMMON tag and nothing else, which is what makes it common
func similarityCorpus() *testcorpus.Corpus {
	tags := []testcorpus.Tag{
		{ID: 1, Name: "rare-tag"},
		{ID: 2, Name: "common-tag"},
		{ID: 3, Name: "filler-a"},
		{ID: 4, Name: "filler-b"},
		{ID: 5, Name: "filler-c"},
		{ID: 6, Name: "filler-d"},
	}
	c := &testcorpus.Corpus{Tags: tags}
	add := func(id int64, tagIDs ...int64) {
		c.Works = append(c.Works, testcorpus.Work{
			ID: id, Title: fmt.Sprintf("work %d", id), Authors: "author", WordCount: 1000, Kudos: 10,
			Rating: "General Audiences", Language: "English", Complete: true,
		})
		for _, tid := range tagIDs {
			c.WorkTags = append(c.WorkTags, testcorpus.WorkTag{WorkID: id, TagID: tid, TagType: "freeforms"})
		}
	}
	add(11, 1, 3)
	add(12, 1, 4)
	add(13, 2, 5)
	add(14, 2, 6)
	for id := int64(15); id <= 22; id++ {
		add(id, 2)
	}
	// 23 and 24 share TWO tags of different rarity, for the ordering test. They
	// are a separate pair because giving 13 or 14 a second shared tag would
	// change the unions the weighting test compares.
	add(23, 1, 2)
	add(24, 1, 2)
	return c
}

// similarityEngine builds the fixture AND the document-frequency table it
// depends on.
//
// The table is what makes the IDF weighting run at all. Without it the engine
// falls back to weighing every tag the same and names `tag_idf` in Missing — a
// behaviour whose test then reads as "the weighting is missing" for a reason
// that has nothing to do with the code under test. Production loads this table
// on start, so the fixture has to as well.
func similarityEngine(t *testing.T) *Engine {
	t.Helper()
	c := similarityCorpus()
	e := newFixtureEngineFrom(t, c)
	byTag := map[int32]float64{}
	for _, wt := range c.WorkTags {
		byTag[int32(wt.TagID)]++
	}
	e.DocFreq = &corpus.DocFreq{Total: float64(len(c.Works)), ByTag: byTag}
	return e
}

// TestARareSharedTagCountsForMoreThanACommonOne is the weighting test: both
// pairs share exactly one tag and their unions are the same size, so a plain
// Jaccard index would score them EQUAL and only the IDF weighting separates
// them.
func TestARareSharedTagCountsForMoreThanACommonOne(t *testing.T) {
	e := similarityEngine(t)
	ctx := context.Background()

	rare, err := e.Similarity(ctx, 11, 12)
	if err != nil {
		t.Fatal(err)
	}
	common, err := e.Similarity(ctx, 13, 14)
	if err != nil {
		t.Fatal(err)
	}
	if len(rare.SharedTags) != 1 || len(common.SharedTags) != 1 {
		t.Fatalf("pairs share %d and %d tags; the fixture is supposed to give each pair "+
			"exactly one, or the comparison is between different things",
			len(rare.SharedTags), len(common.SharedTags))
	}
	if rare.TagOverlap <= common.TagOverlap {
		t.Fatalf("a shared tag carried by 2 works scores %.4f and one carried by 10 "+
			"scores %.4f: the IDF weighting is not in the score, and a plain Jaccard "+
			"index would call these pairs equally alike",
			rare.TagOverlap, common.TagOverlap)
	}
	if rare.SharedTags[0].Name != "rare-tag" {
		t.Fatalf("the shared tag is named %q", rare.SharedTags[0].Name)
	}
}

// TestTheEmbeddingAxisIsNamedWhenItCannotRun: no embeddings written, so the
// score must be the tag overlap alone.
func TestTheEmbeddingAxisIsNamedWhenItCannotRun(t *testing.T) {
	e := similarityEngine(t)
	res, err := e.Similarity(context.Background(), 11, 12)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(res.Missing, "embed_cosine") {
		t.Fatalf("missing = %v, want embed_cosine named: a mirror with no embeddings "+
			"must not report the axis as a zero similarity", res.Missing)
	}
	if res.EmbedCosine != 0 {
		t.Fatalf("embed cosine is %.3f with no index loaded", res.EmbedCosine)
	}
	if diff := res.Score - res.TagOverlap; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("score %.6f is not the tag overlap %.6f when the other axis is absent: "+
			"the score must be renormalised over what ran", res.Score, res.TagOverlap)
	}
}

// TestTheEmbeddingAxisScoresWhenTheIndexExists: the same pair, now embedded.
// The two works' centroids share the rare tag's direction, so the cosine is
// positive and the axis is no longer named as missing.
func TestTheEmbeddingAxisScoresWhenTheIndexExists(t *testing.T) {
	e := similarityEngine(t)
	// Tags 1 and 3 point the same way; 4 is orthogonal-ish. Works 11 and 12
	// therefore share the direction of tag 1 through their centroids.
	putTagVecs(t, e, 4, map[int32][]float32{
		1: {1, 0, 0, 0},
		2: {0, 1, 0, 0},
		3: {1, 0, 0, 0},
		4: {1, 0, 0, 0},
		5: {0, 1, 0, 0},
		6: {0, 1, 0, 0},
	})
	res, err := e.Similarity(context.Background(), 11, 12)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(res.Missing, "embed_cosine") {
		t.Fatalf("embed_cosine is named missing with an index loaded: %v", res.Missing)
	}
	if res.EmbedCosine < 0.99 {
		t.Fatalf("embed cosine = %.4f for two works whose centroids are the same "+
			"direction, want ~1", res.EmbedCosine)
	}
	if res.Score <= res.TagOverlap {
		t.Fatalf("score %.4f did not include the embedding axis (tag overlap %.4f): "+
			"the pair scored higher on tags alone, so the second axis was dropped",
			res.Score, res.TagOverlap)
	}
}

// TestAPairNeedsTwoRealWorks: a typo must not read as "nothing in common".
func TestAPairNeedsTwoRealWorks(t *testing.T) {
	e := similarityEngine(t)
	ctx := context.Background()
	if _, err := e.Similarity(ctx, 11, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown second work returned %v, want ErrNotFound", err)
	}
	if _, err := e.Similarity(ctx, 999999, 11); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown first work returned %v, want ErrNotFound", err)
	}
	if _, err := e.Similarity(ctx, 11, 11); err == nil {
		t.Fatal("a work compared with itself returned a report; it has no second side")
	}
}

// TestTheSharedTagsAreOrderedByRarity: the first row is the explanation, so the
// rarest shared tag must be first — not whichever the join happened to yield.
func TestTheSharedTagsAreOrderedByRarity(t *testing.T) {
	e := similarityEngine(t)
	ctx := context.Background()
	res, err := e.Similarity(ctx, 23, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SharedTags) < 2 {
		t.Fatalf("only %d shared tags", len(res.SharedTags))
	}
	if res.SharedTags[0].ID != 1 {
		t.Fatalf("the first shared tag is %q (idf %.3f) and the second is %q (idf %.3f): "+
			"the rarest must come first, because the order IS the explanation",
			res.SharedTags[0].Name, res.SharedTags[0].IDF,
			res.SharedTags[1].Name, res.SharedTags[1].IDF)
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
