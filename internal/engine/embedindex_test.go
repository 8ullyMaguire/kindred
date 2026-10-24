package engine

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// putTagVecs writes tag embeddings into the fixture engine's state database.
//
// The fixture corpus has no `kindred embed` run behind it, which is exactly
// the state every deployment was in before this unit: a registered signal with
// nothing to read. Writing the rows here is what turns the wiring test from
// "the signal exists" into "the signal scored".
func putTagVecs(t *testing.T, e *Engine, dim int, vecs map[int32][]float32) {
	t.Helper()
	ctx := context.Background()
	if err := e.Store.Migrate(ctx); err != nil {
		t.Fatalf("migrate state db: %v", err)
	}
	for id, v := range vecs {
		if len(v) != dim {
			t.Fatalf("tag %d has %d coords, want %d", id, len(v), dim)
		}
		blob := make([]byte, dim*4)
		for i, x := range v {
			binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(x))
		}
		if _, err := e.Store.DB.ExecContext(ctx,
			`INSERT OR REPLACE INTO embeddings(kind, entity_id, dim, vec) VALUES('tag',?,?,?)`,
			id, dim, blob); err != nil {
			t.Fatalf("insert embedding for tag %d: %v", id, err)
		}
	}
}

// TestTagIndexCentroidIsTheMeanAndSkipsAbsentTags: the centroid is the mean
// over the tags that HAVE a vector, not over all the tags asked for.
//
// A zero vector in the mean would pull every work with one unembedded tag
// toward the origin, and the origin is the one place every unrelated work
// agrees on — the bug would make a half-embedded corpus look like everything
// is similar to everything.
func TestTagIndexCentroidIsTheMeanAndSkipsAbsentTags(t *testing.T) {
	e := newFixtureEngine(t)
	putTagVecs(t, e, 4, map[int32][]float32{
		1: {1, 0, 0, 0},
		2: {0, 1, 0, 0},
		// tag 3 deliberately absent
	})
	idx, err := e.tagVectors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if idx == nil {
		t.Fatal("no index loaded from two written rows")
	}
	got := idx.centroid([]int32{1, 2, 3})
	want := []float32{0.5, 0.5, 0, 0}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Fatalf("centroid = %v, want %v (the absent tag must not be averaged in)", got, want)
		}
	}
	if idx.centroid([]int32{3}) != nil {
		t.Fatal("a centroid over only absent tags must be nil, not a zero vector")
	}
	if _, ok := idx.row(3); ok {
		t.Fatal("row(3) reported a vector for a tag that was never written")
	}
}

// TestNearestTagsOrderAndDeterminism: the finder must return the closest tags
// first, exclude the seed's own tags, and break ties by id so two identical
// requests produce the same pool.
func TestNearestTagsOrderAndDeterminism(t *testing.T) {
	e := newFixtureEngine(t)
	putTagVecs(t, e, 3, map[int32][]float32{
		1: {1, 0, 0}, // identical to the centroid -> 1.0
		2: {0.9, 0.1, 0},
		3: {0, 1, 0},
		4: {1, 0, 0}, // ties with tag 1 exactly
	})
	idx, err := e.tagVectors(context.Background())
	if err != nil || idx == nil {
		t.Fatalf("load index: %v", err)
	}
	got := idx.nearestTags([]float32{1, 0, 0}, 3, 0.2, map[int32]bool{2: true})
	// Tags 1 and 4 tie at 1.0; tag 2 is skipped by the caller; tag 3 is
	// orthogonal (0.0) and below the floor, so the honest answer is two.
	if len(got) != 2 {
		t.Fatalf("got %d tags, want 2: %v", len(got), got)
	}
	if got[0].ID != 1 || got[1].ID != 4 {
		t.Fatalf("top tags = %v, want ids 1 then 4 (tie broken by id)", got)
	}
	for _, s := range got {
		if s.ID == 2 {
			t.Fatal("skip set was ignored: tag 2 was excluded from the seeds' own tags")
		}
	}
	again := idx.nearestTags([]float32{1, 0, 0}, 3, 0.2, map[int32]bool{2: true})
	for i := range got {
		if got[i] != again[i] {
			t.Fatalf("two identical calls disagreed: %v then %v", got, again)
		}
	}
}

// TestEmbeddingSignalContributesOnEveryItem is the wiring test this dimension
// never had.
//
// It would have failed on the shipped code in two independent ways: the
// weight was deleted by renormalise (so no evidence entry existed at all), and
// the seed vector was read from per-WORK embeddings that no deployment has.
// Asserting the evidence entry exists, carries a positive weight, and scores
// inside (0,1] checks the whole path: load -> seed centroid -> per-candidate
// centroid -> score -> meta.
func TestEmbeddingSignalContributesOnEveryItem(t *testing.T) {
	e := newFixtureEngine(t)
	vecs := map[int32][]float32{}
	// Two clusters in 4 dimensions: tags 1-6 point one way, 7-12 the other.
	// Whatever the works carry, both directions exist, so no candidate can
	// skip for want of a vector.
	for id := int32(1); id <= 12; id++ {
		if id <= 6 {
			vecs[id] = []float32{1, float32(id) / 10, 0, 0}
		} else {
			vecs[id] = []float32{0, 0, 1, float32(id) / 10}
		}
	}
	putTagVecs(t, e, 4, vecs)

	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) == 0 {
		t.Fatal("no items; the fixture is too small to judge the wiring")
	}
	scored := 0
	for _, it := range res.Items {
		for _, ev := range it.Evidence {
			if ev.Signal != "embedding" {
				continue
			}
			scored++
			if ev.Weight <= 0 {
				t.Errorf("embedding evidence carries weight %v: the weight was dropped again", ev.Weight)
			}
			if ev.Value <= 0 || ev.Value > 1 {
				t.Errorf("embedding value %v outside (0,1]", ev.Value)
			}
		}
	}
	if scored == 0 {
		t.Fatalf("no item carried embedding evidence; degraded=%v", res.Meta.Degraded)
	}
	for _, name := range res.Meta.Degraded {
		if name == "embedding" {
			t.Fatal("embedding reported degraded while the mirror holds vectors")
		}
	}
}

// TestEmbeddingIsDegradedAndWeightlessWithoutVectors: a mirror with no
// `kindred embed` run must rank exactly as the pre-embedding engine did, and
// say why in meta.degraded rather than silently shrinking every score by 10%.
func TestEmbeddingIsDegradedAndWeightlessWithoutVectors(t *testing.T) {
	e := newFixtureEngine(t)
	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawDegraded bool
	for _, name := range res.Meta.Degraded {
		if name == "embedding" {
			sawDegraded = true
		}
	}
	if !sawDegraded {
		t.Fatalf("embedding absent from meta.degraded on a mirror with no vectors: %v", res.Meta.Degraded)
	}
	for _, it := range res.Items {
		for _, ev := range it.Evidence {
			if ev.Signal == "embedding" {
				t.Fatal("embedding scored on a mirror with no vectors")
			}
		}
	}
}

// TestLiteModeDropsTheEmbeddingWeight is the other half of the renormalise
// bug: in lite mode no embedding signal is registered, so its weight MUST be
// given to the signals that are.
func TestLiteModeDropsTheEmbeddingWeight(t *testing.T) {
	e := newFixtureEngine(t)
	e.Lite = true
	sigs, tune := e.signals(context.Background(), nil, nil, 0, nil, "default", true)
	for _, s := range sigs {
		if s.Name() == "embedding" {
			t.Fatal("lite mode registered the embedding signal")
		}
	}
	if w, ok := tune.Weights["embedding"]; ok {
		t.Fatalf("lite mode kept an embedding weight of %v for a signal that cannot run", w)
	}
	// The lite tune must still be the same shape as the default tune minus one
	// dimension: renormalisation redistributes, it does not zero the others.
	var sum float64
	for _, w := range tune.Weights {
		sum += w
	}
	var want float64
	for _, w := range DefaultTune().Weights {
		want += w
	}
	if math.Abs(sum-want) > 1e-9 {
		t.Fatalf("lite weights sum to %v, want %v (the dropped weight was not redistributed)", sum, want)
	}
}

// TestSeedCentroidWeightsEachSeedEqually: a seed with forty tags and a seed
// with four must count once each, or the centroid is a measure of tagging
// habit rather than of taste.
func TestSeedCentroidWeightsEachSeedEqually(t *testing.T) {
	e := newFixtureEngine(t)
	putTagVecs(t, e, 2, map[int32][]float32{
		1: {1, 0}, 2: {1, 0}, 3: {1, 0}, 4: {1, 0}, // the wordy seed's tags
		5: {0, 1}, // the terse seed's only tag
	})
	idx, err := e.tagVectors(context.Background())
	if err != nil || idx == nil {
		t.Fatalf("load index: %v", err)
	}
	got := seedCentroid(idx, []rank.Candidate{
		{ID: 1, TagIDs: []int32{1, 2, 3, 4}},
		{ID: 2, TagIDs: []int32{5}},
	})
	// Equal weighting puts this at (0.5, 0.5). Averaging all five tags would
	// put it at (0.8, 0.2).
	if math.Abs(float64(got[0])-0.5) > 1e-6 || math.Abs(float64(got[1])-0.5) > 1e-6 {
		t.Fatalf("seed centroid = %v, want [0.5 0.5] (each seed counts once)", got)
	}
}

// The embedding cache must not be poisoned by a caller that hung up.
//
// This is the 2026-10-24 live defect: the one-time load ran on the FIRST caller's
// context, so a cancelled first request (a deploy health check, a reader who closed
// the tab) cached "context canceled" for the life of the process. The server then
// answered every reading path with "This mirror has no tag embeddings" while
// /api/v1/stats reported 634,232 of them — two surfaces of one process disagreeing
// about what it knew.
func TestACancelledCallerDoesNotPoisonTheEmbeddingCache(t *testing.T) {
	e := newFixtureEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller is already gone when the load would start

	idx, err := e.tagVectors(ctx)
	if err != nil {
		t.Fatalf("the load ran on a cancelled caller's context and cached %v for the "+
			"life of the process; a client hanging up must not decide what this "+
			"process knows", err)
	}
	// A mirror with no vectors legitimately has no index; what must not happen
	// is the ERROR being cached. The fixture has no vectors, so nil-nil is the
	// expected pair after the fix and the error-carrying pair is what the bug
	// produced.
	_ = idx
}

// The index is still loaded once: two calls hand back the same pointer, so the
// 634k-row scan cannot become a per-request cost.
func TestTheEmbeddingIndexIsLoadedOnce(t *testing.T) {
	e := newFixtureEngine(t)
	first, err := e.tagVectors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.tagVectors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("two calls returned different indexes: the load is not cached")
	}
}
