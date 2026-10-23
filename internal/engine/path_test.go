package engine

// Reading paths (#40).
//
// What these tests have to prove, because a walk is easy to fake:
//
//   - **Each step is measured from the PREVIOUS work, not from the seed.** That
//     is the whole feature; a walk that keeps scoring against the seed is the
//     "more like this" list with extra words. The fixture is built so the two
//     answers differ by a wide margin, and the test computes the expected cosine
//     itself from the vectors it wrote.
//
//   - **No work appears twice.** Greedy nearest-neighbour without that rule
//     returns A, B, A, B and every hop after the second is a lie about having
//     moved.
//
//   - **A path says where it left the seed's tags**, and says so when it cannot
//     walk at all.

import (
	"context"
	"errors"
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// pathCorpus is a line of works that walks AWAY from the seed's tag:
//
//	100 {1}       the seed
//	101 {1,2}     still shares the seed's tag - the closest first hop
//	102 {2}
//	103 {2,3}     no longer shares the seed's tag
//	104 {3}
//	105 {5}       reachable only through the embedding (tag 5 is near tag 2, but
//	              no work in the fixture carries both) — the hop that proves the
//	              walk is not just following tags
func pathCorpus() *testcorpus.Corpus {
	c := &testcorpus.Corpus{
		Tags: []testcorpus.Tag{
			{ID: 1, Name: "seed-tag"}, {ID: 2, Name: "second"}, {ID: 3, Name: "third"},
			{ID: 4, Name: "elsewhere"}, {ID: 5, Name: "unrelated"},
		},
	}
	add := func(id int64, tagIDs ...int64) {
		c.Works = append(c.Works, testcorpus.Work{
			ID: id, Title: "work", Authors: "author", WordCount: 900, Kudos: 5,
			Rating: "General Audiences", Language: "English", Complete: true,
		})
		for _, tid := range tagIDs {
			c.WorkTags = append(c.WorkTags, testcorpus.WorkTag{WorkID: id, TagID: tid, TagType: "freeforms"})
		}
	}
	add(100, 1)
	add(101, 1, 2)
	add(102, 2)
	add(103, 2, 3)
	add(104, 3)
	add(105, 5)
	return c
}

// pathVectors puts the tags on a two-dimensional arc, so a hop's direction
// change is arithmetic the test can redo by hand.
func pathVectors(t *testing.T, e *Engine) {
	t.Helper()
	putTagVecs(t, e, 2, map[int32][]float32{
		1: {1, 0},
		2: {0.8, 0.6},
		3: {0, 1},
		4: {-1, 0},
		5: {0.6, 0.8},
	})
}

func pathEngine(t *testing.T) *Engine {
	t.Helper()
	return newFixtureEngineFrom(t, pathCorpus())
}

// meanCosineOfWorks is the test's own arithmetic: the cosine between the mean
// vectors of two tag sets, computed from the fixture's numbers rather than from
// the engine's answer.
func meanCosineOfWorks(t *testing.T, a, b []int32, vec map[int32][2]float64) float64 {
	t.Helper()
	mean := func(ids []int32) [2]float64 {
		var out [2]float64
		var n int
		for _, id := range ids {
			v, ok := vec[id]
			if !ok {
				continue
			}
			out[0] += v[0]
			out[1] += v[1]
			n++
		}
		if n > 0 {
			out[0] /= float64(n)
			out[1] /= float64(n)
		}
		return out
	}
	ma, mb := mean(a), mean(b)
	return (ma[0]*mb[0] + ma[1]*mb[1]) / (math.Hypot(ma[0], ma[1]) * math.Hypot(mb[0], mb[1]))
}

// TestEachStepIsMeasuredFromThePreviousWork is the feature.
func TestEachStepIsMeasuredFromThePreviousWork(t *testing.T) {
	e := pathEngine(t)
	pathVectors(t, e)
	ctx := context.Background()

	p, err := e.ReadingPath(ctx, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) < 3 {
		t.Fatalf("the path has %d steps; the fixture is built to allow three", len(p.Steps))
	}
	if p.Steps[0].ID != 101 {
		t.Fatalf("the first hop is work %d, want 101 (the work nearest the seed's tag)",
			p.Steps[0].ID)
	}

	vec := map[int32][2]float64{1: {1, 0}, 2: {0.8, 0.6}, 3: {0, 1}, 4: {-1, 0}, 5: {0.6, 0.8}}
	byID := map[int64][]int32{100: {1}, 101: {1, 2}, 102: {2}, 103: {2, 3}, 104: {3}, 105: {5}}

	for i := 1; i < len(p.Steps); i++ {
		prev, cur := p.Steps[i-1], p.Steps[i]
		want := meanCosineOfWorks(t, byID[prev.ID], byID[cur.ID], vec)
		if math.Abs(cur.SimToPrev-want) > 1e-6 {
			t.Fatalf("step %d reports %.4f against the previous step %d, but the cosine "+
				"between those two works is %.4f. Scored against the SEED instead, this "+
				"step would read %.4f — which is the difference between a walk and a fan",
				cur.Step, cur.SimToPrev, prev.ID, want,
				meanCosineOfWorks(t, byID[100], byID[cur.ID], vec))
		}
	}
}

// TestNoWorkAppearsTwice: without the visited set the walk returns to the work
// it came from, because that work is by definition nearest to itself.
func TestNoWorkAppearsTwice(t *testing.T) {
	e := pathEngine(t)
	pathVectors(t, e)
	p, err := e.ReadingPath(context.Background(), 100, 8)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]int{}
	for _, s := range p.Steps {
		seen[s.ID]++
		if seen[s.ID] > 1 {
			t.Fatalf("work %d appears %d times in the path: every hop after the first "+
				"repeat claims to have moved and has not", s.ID, seen[s.ID])
		}
		if s.ID == 100 {
			t.Fatal("the seed appears in its own path")
		}
	}
	if p.Returned != len(p.Steps) {
		t.Fatalf("returned = %d but the list holds %d", p.Returned, len(p.Steps))
	}
	if p.Returned > p.Requested {
		t.Fatalf("returned %d of %d requested", p.Returned, p.Requested)
	}
}

// TestThePathSaysWhereItLeftTheSeedsTags: the honest form of the drift a fandom
// cap could not enforce on this corpus.
func TestThePathSaysWhereItLeftTheSeedsTags(t *testing.T) {
	e := pathEngine(t)
	pathVectors(t, e)
	p, err := e.ReadingPath(context.Background(), 100, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) < 3 {
		t.Fatalf("only %d steps", len(p.Steps))
	}
	if !p.Steps[0].SharesSeedTag {
		t.Fatal("the first hop does not share the seed's tag, but work 101 carries tag 1")
	}
	// Work 103 carries {2,3} and no tag 1, so by step 2 the walk has left the
	// seed's vocabulary behind.
	if p.DriftFromSeed == 0 {
		t.Fatalf("drift_from_seed is unset, but steps %v include a work with no tag "+
			"in common with the seed", stepIDs(p))
	}
	if p.DriftFromSeed != 2 {
		t.Fatalf("drift_from_seed = %d, want 2", p.DriftFromSeed)
	}
	if p.Steps[1].SharesSeedTag {
		t.Fatalf("step 2 shares a seed tag according to the report, but work %d carries "+
			"only tags the seed does not have", p.Steps[1].ID)
	}
}

// TestAPathWithoutEmbeddingsSaysSoRatherThanInventingOne: no index, no space.
// Returning the ordinary recommendations here would be another feature wearing
// this one's name.
func TestAPathWithoutEmbeddingsSaysSoRatherThanInventingOne(t *testing.T) {
	e := pathEngine(t)
	p, err := e.ReadingPath(context.Background(), 100, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("a mirror with no embeddings produced %d steps", len(p.Steps))
	}
	if !containsString(p.Missing, "embed_cosine") {
		t.Fatalf("missing = %v, want embed_cosine named", p.Missing)
	}
	if p.Note == "" {
		t.Fatal("no note: a reader gets an empty list with no reason for it")
	}
}

func TestAPathNeedsARealSeed(t *testing.T) {
	e := pathEngine(t)
	pathVectors(t, e)
	if _, err := e.ReadingPath(context.Background(), 424242, 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown seed returned %v, want ErrNotFound", err)
	}
}

// TestTheStepRulePrefersADeparture pins the selection rule itself.
//
// Plain nearest-neighbour would take the work at 0.90 from the previous step and
// 0.95 from the SEED — the "more like this" list in sequence, which is what the
// live mirror produced before this weight existed. The rule has to prefer the
// work at 0.85/0.40, and the assertion is stated as that preference rather than
// as a constant, so retuning the weight cannot silently invert it.
func TestTheStepRulePrefersADeparture(t *testing.T) {
	nearSeed := pathRank(0.90, 0.95)
	departure := pathRank(0.85, 0.40)
	if departure <= nearSeed {
		t.Fatalf("a work 0.85 from the previous step and 0.40 from the seed ranks "+
			"%.4f, and one 0.90/0.95 ranks %.4f: the rule prefers staying near the "+
			"seed, which is the failure this feature exists to avoid",
			departure, nearSeed)
	}
	// The other side: a work that is barely reachable must not win on distance
	// alone. 0.20 from the previous step and 0.05 from the seed is not a hop.
	if pathRank(0.20, 0.05) >= pathRank(0.85, 0.40) {
		t.Fatal("a nearly unreachable work outranked a real hop")
	}
}

func stepIDs(p *ReadingPath) []int64 {
	out := make([]int64, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.ID)
	}
	return out
}
