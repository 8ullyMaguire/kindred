package engine

import (
	"context"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// latentCorpus is the fixture for the embedding enumerator: a work that
// shares NO tag with the seed but sits next to it in the embedding space.
//
// That is the only case an embedding source exists for. A work that shares a
// tag is already found by the tag query, so a test built on one would pass
// against an implementation whose embedding source returns nothing at all —
// the tautology shape this repo has been bitten by before.
func latentCorpus(t *testing.T) (*Engine, int64) {
	t.Helper()
	c := testcorpus.New(60)
	const bridgeTag = 40
	const bridgeWork = 999
	c.Tags = append(c.Tags, testcorpus.Tag{ID: bridgeTag, Name: "a tag the seed never carries"})
	c.Works = append(c.Works, testcorpus.Work{
		ID: bridgeWork, Title: "The Latent Bridge", Authors: "author9",
		Summary: "Shares no tag with the seed.", WordCount: 40_000,
		Hits: 1000, Kudos: 100, Rating: "Teen And Up Audiences",
		Language: "English", Complete: true, UpdateDate: "15 Jan 2026",
	})
	c.WorkTags = append(c.WorkTags, testcorpus.WorkTag{
		WorkID: bridgeWork, TagID: bridgeTag, TagType: "freeforms",
	})

	e := newFixtureEngineFrom(t, c)

	// Every tag in the corpus points along one axis, the bridge tag included,
	// so the bridge is near the seed's centroid while sharing nothing with
	// it. If the source were actually reading tag co-occurrence, the bridge
	// would stay unreachable — which is the property under test.
	vecs := map[int32][]float32{}
	for id := int32(1); id <= 12; id++ {
		vecs[id] = []float32{1, float32(id) / 100, 0, 0}
	}
	vecs[bridgeTag] = []float32{1, 0.5, 0, 0}
	putTagVecs(t, e, 4, vecs)
	return e, bridgeWork
}

func poolOf(t *testing.T, e *Engine, seedID int64, mode PoolMode, n int) ([]int64, *rank.PoolSources) {
	t.Helper()
	ctx := context.Background()
	ents, err := e.Corpus.CandidateRows(ctx, []int64{seedID})
	if err != nil {
		t.Fatalf("load seeds: %v", err)
	}
	seeds, err := e.toCandidates(ctx, ents)
	if err != nil {
		t.Fatalf("load seed tags: %v", err)
	}
	pool, _, src, err := e.poolFor(ctx, seeds, n, true, []int64{seedID},
		Request{PoolMode: mode})
	if err != nil {
		t.Fatalf("poolFor(%s): %v", mode, err)
	}
	return pool, src
}

// TestEmbedPoolReachesAWorkTheTagPoolCannot is the whole point of the source.
func TestEmbedPoolReachesAWorkTheTagPoolCannot(t *testing.T) {
	e, bridge := latentCorpus(t)

	plain, srcPlain := poolOf(t, e, 1, PoolTags, 400)
	for _, id := range plain {
		if id == bridge {
			t.Fatalf("work %d shares no tag with the seed but is in the TAG pool: "+
				"the first half of this test proves nothing", bridge)
		}
	}
	if srcPlain.Embedding != 0 {
		t.Errorf("tags-only mode reported %d embedding candidates", srcPlain.Embedding)
	}

	withEmbed, src := poolOf(t, e, 1, PoolTagsEmbed, 400)
	var found bool
	for _, id := range withEmbed {
		if id == bridge {
			found = true
		}
	}
	if !found {
		t.Fatalf("work %d is nearest the seeds in embedding space but the "+
			"tags+embed pool (of %d) does not hold it; sources=%+v", bridge, len(withEmbed), src)
	}
	if src.Embedding < 1 {
		t.Fatalf("pool_sources.embedding = %d with the source on and a reachable work", src.Embedding)
	}
	if src.Tags+src.Collab+src.Embedding != len(withEmbed) {
		t.Fatalf("sources %+v do not add up to a pool of %d: tags+collab+embedding "+
			"must equal the candidate set, or the breakdown is a decoration",
			src, len(withEmbed))
	}
	if src.Overlap == 0 {
		t.Fatal("two sources that propose overlapping works reported zero overlap")
	}
}

// TestPoolSourcesAddUpWithoutTheEmbeddingSource: the instrumentation must be
// correct on the path every existing deployment already takes, not only on
// the new one.
func TestPoolSourcesAddUpWithoutTheEmbeddingSource(t *testing.T) {
	e := newFixtureEngine(t)
	pool, src := poolOf(t, e, 1, PoolTags, 200)
	if src == nil {
		t.Fatal("poolFor returned no source breakdown")
	}
	if src.Tags != len(pool) || src.Collab != 0 || src.Embedding != 0 {
		t.Fatalf("tags-only sources = %+v for a pool of %d; want tags=%d only",
			src, len(pool), len(pool))
	}
	if src.Overlap != 0 {
		t.Fatalf("tags-only sources report %d overlaps; there is no second source to overlap", src.Overlap)
	}
}

// TestEmbedPoolHonoursFiltersInTheQuery: a filter applied after the embedding
// source's LIMIT would return fewer candidates than asked for even when
// qualifying works exist further down, which is the defect poolFor's doc
// comment describes at length. The assertion is on the pool, not the results.
func TestEmbedPoolHonoursFiltersInTheQuery(t *testing.T) {
	e, bridge := latentCorpus(t)
	ctx := context.Background()
	ents, err := e.Corpus.CandidateRows(ctx, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	seeds, err := e.toCandidates(ctx, ents)
	if err != nil {
		t.Fatal(err)
	}
	// The bridge work is 40,000 words; ask for shorter than that.
	pool, _, _, err := e.poolFor(ctx, seeds, 400, true, []int64{1}, Request{
		PoolMode: PoolTagsEmbed,
		Filter:   Filter{MaxWords: 20_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range pool {
		if id == bridge {
			t.Fatalf("work %d is 40k words and the request capped at 20k, but it is in the pool", bridge)
		}
	}
}

// TestPoolSourcesReportedOnTheResult is the wiring half: the breakdown must
// reach the response metadata, or an operator cannot see it.
func TestPoolSourcesReportedOnTheResult(t *testing.T) {
	e, _ := latentCorpus(t)
	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 10,
		PoolMode: PoolTagsEmbed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.PoolSources == nil {
		t.Fatal("meta.pool_sources is missing from a recommendation")
	}
	if res.Meta.PoolSources.Embedding < 1 {
		t.Fatalf("meta.pool_sources.embedding = %d; the source ran and found a work",
			res.Meta.PoolSources.Embedding)
	}
	if res.Meta.PoolSources.Tags+res.Meta.PoolSources.Collab+
		res.Meta.PoolSources.Embedding != res.Meta.PoolSize {
		t.Fatalf("meta.pool_sources %+v does not add up to pool_size %d",
			res.Meta.PoolSources, res.Meta.PoolSize)
	}
}

// TestTheReserveDoesNotShortenThePool: a reserve that is not filled must not
// cost the pool slots.
//
// Measured on the live mirror before the top-up: pool_mode=all returned 848
// candidates against 1000 for tags-only, because the tag half was capped at
// 750 and the extra sources found 98 between them. The recall figure went UP
// (better candidates), so no recall assertion would ever have reported the 152
// missing slots -- which is exactly why they need their own check.
func TestTheReserveDoesNotShortenThePool(t *testing.T) {
	e := newFixtureEngine(t)
	const limit = 10

	// The premise: with the extras enabled the tag half is capped BELOW the
	// limit, so an implementation without the top-up cannot fill it.
	only, _ := poolOf(t, e, 1, PoolTags, limit)
	if len(only) != limit {
		t.Fatalf("tags-only pool is %d for a limit of %d; the fixture cannot "+
			"exercise a full pool", len(only), limit)
	}

	withExtras, src := poolOf(t, e, 1, PoolTagsCollabEmbed, limit)
	if len(withExtras) != limit {
		t.Fatalf("tags+collab+embed pool is %d for a limit of %d (sources %+v): "+
			"the reserve shortened the pool instead of being topped up",
			len(withExtras), limit, src)
	}
	if src.Tags+src.Collab+src.Embedding != len(withExtras) {
		t.Fatalf("sources %+v do not add up to %d", src, len(withExtras))
	}
	// And no id appears twice, which the old union allowed.
	seen := map[int64]bool{}
	for _, id := range withExtras {
		if seen[id] {
			t.Fatalf("work %d is in the pool twice", id)
		}
		seen[id] = true
	}
}
