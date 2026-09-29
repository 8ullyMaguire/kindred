package signal

import (
	"context"
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

func TestTagOverlapScoresJaccard(t *testing.T) {
	s := TagOverlap{}
	cand := rank.Candidate{ID: 1, TagIDs: []int32{1, 2, 3, 4}}
	seeds := []rank.Candidate{{ID: 99, TagIDs: []int32{1, 2}}}
	v, reason, err := s.Score(cand, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v-2.0/4.0) > 1e-9 {
		t.Fatalf("score = %v, want 0.5 (2 shared of a union of 4)", v)
	}
	if reason == "" {
		t.Fatal("no reason; a ranking that cannot explain itself cannot be argued with")
	}
}

func TestTagOverlapSkipsWhenNothingIsShared(t *testing.T) {
	s := TagOverlap{}
	cand := rank.Candidate{ID: 1, TagIDs: []int32{1, 2}}
	seeds := []rank.Candidate{{ID: 99, TagIDs: []int32{7, 8}}}
	if _, _, err := s.Score(cand, seeds, nil); err != rank.ErrSkip {
		t.Fatalf("err = %v, want ErrSkip: no shared tag is an absence, not a zero", err)
	}
}

func TestTagOverlapSkipsOnEmptyInput(t *testing.T) {
	s := TagOverlap{}
	if _, _, err := s.Score(rank.Candidate{}, []rank.Candidate{{ID: 1, TagIDs: []int32{1}}}, nil); err != rank.ErrSkip {
		t.Fatalf("a candidate with no tags returned %v", err)
	}
	if _, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{1}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("no seeds returned %v", err)
	}
}

func TestTagOverlapTakesTheBestSeed(t *testing.T) {
	s := TagOverlap{}
	cand := rank.Candidate{ID: 1, TagIDs: []int32{1, 2, 3, 4}}
	seeds := []rank.Candidate{
		{ID: 10, TagIDs: []int32{9}},          // no overlap
		{ID: 11, TagIDs: []int32{1, 2, 3, 4}}, // perfect
	}
	v, _, err := s.Score(cand, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v < 0.99 {
		t.Fatalf("score = %v, want ~1 from the best-matching seed", v)
	}
}

func TestTagOverlapBoundsTagCount(t *testing.T) {
	// A work with thousands of tags is a fandom dump; intersecting all of
	// them per seed is quadratic for no signal.
	tags := make([]int32, 5000)
	for i := range tags {
		tags[i] = int32(i)
	}
	s := TagOverlap{MaxTags: 10}
	cand := rank.Candidate{ID: 1, TagIDs: tags}
	seeds := []rank.Candidate{{ID: 9, TagIDs: []int32{5000 - 1}}}
	// The overlap tag is beyond MaxTags, so the bound must exclude it.
	if _, _, err := s.Score(cand, seeds, nil); err != rank.ErrSkip {
		t.Fatalf("a tag past MaxTags was still matched; err = %v", err)
	}
}

// stubGraph is a tiny in-memory graph for the neighbourhood signal.
type stubGraph struct {
	neighbours map[int32][]graph.ScoredNeighbour
	names      map[int32]string
}

func (g stubGraph) Neighbours(int32) ([]int32, []float32) { return nil, nil }
func (g stubGraph) Frequency(int32) float64               { return 100 }
func (g stubGraph) NeighbourScore(q int32, limit int, _ float64) []graph.ScoredNeighbour {
	out := g.neighbours[q]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
func (g stubGraph) Name(id int32) string { return g.names[id] }

func TestNeighbourhoodScoresReachableTags(t *testing.T) {
	g := stubGraph{
		neighbours: map[int32][]graph.ScoredNeighbour{
			1: {{TagID: 10, PMI: 3.0, Count: 50}, {TagID: 11, PMI: 1.0, Count: 5}},
		},
		names: map[int32]string{10: "slow burn", 11: "fluff"},
	}
	s := Neighbourhood{G: g, SeedTags: []int32{1}, TotalWorks: 1000}
	cand := rank.Candidate{ID: 1, TagIDs: []int32{10, 99}}
	v, reason, err := s.Score(cand, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v <= 0 {
		t.Fatalf("score = %v, want > 0", v)
	}
	if reason == "" {
		t.Fatal("no reason")
	}
}

func TestNeighbourhoodSkipsWhenUnreachable(t *testing.T) {
	g := stubGraph{neighbours: map[int32][]graph.ScoredNeighbour{
		1: {{TagID: 10, PMI: 3.0}},
	}}
	s := Neighbourhood{G: g, SeedTags: []int32{1}, TotalWorks: 1000}
	if _, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{77}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("err = %v, want ErrSkip", err)
	}
}

func TestNeighbourhoodSkipsWithoutGraphOrSeedTags(t *testing.T) {
	s := Neighbourhood{}
	if _, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{1}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("no graph returned %v", err)
	}
	g := stubGraph{}
	s2 := Neighbourhood{G: g}
	if _, _, err := s2.Score(rank.Candidate{ID: 1, TagIDs: []int32{1}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("no seed tags returned %v", err)
	}
}

// TestQualityIgnoresNullBookmarks is the trap the real corpus sets: hits is
// populated for every work, bookmarks for almost none, so a quality signal
// built on bookmarks would rank on NULLs.
func TestQualityUsesKudosNotBookmarks(t *testing.T) {
	s := Quality{}
	cand := rank.Candidate{ID: 1, Stats: map[string]float64{
		"kudos": 300, "word_count": 50000, "has_bookmarks": 0,
	}}
	v, _, err := s.Score(cand, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v <= 0 {
		t.Fatalf("score = %v, want > 0 for a work with 300 kudos", v)
	}
	// A work with no kudos at all must skip rather than score zero: no
	// kudos is an absence of evidence, not evidence of quality.
	if _, _, err := s.Score(rank.Candidate{ID: 2, Stats: map[string]float64{
		"kudos": 0, "word_count": 50000,
	}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("a work with no kudos returned %v, want ErrSkip", err)
	}
}

func TestQualitySkipsShortWorks(t *testing.T) {
	s := Quality{}
	// Below 1000 words the per-1k rate is dominated by the denominator.
	if _, _, err := s.Score(rank.Candidate{ID: 1, Stats: map[string]float64{
		"kudos": 10, "word_count": 200,
	}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("a 200-word work returned %v, want ErrSkip", err)
	}
}

func TestQualityIsSquashed(t *testing.T) {
	s := Quality{}
	modest, _, err := s.Score(rank.Candidate{Stats: map[string]float64{"kudos": 10, "word_count": 10000}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	huge, _, err := s.Score(rank.Candidate{Stats: map[string]float64{"kudos": 100000, "word_count": 10000}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A 10,000x difference in rate must not produce a 10,000x score.
	if huge > 1 {
		t.Fatalf("score = %v, want <= 1", huge)
	}
	if huge/modest > 20 {
		t.Fatalf("score ratio %f is not squashed enough (%v vs %v)", huge/modest, huge, modest)
	}
}

func TestRecencyHalvesPerYear(t *testing.T) {
	s := Recency{Now: 20000} // epoch days
	fresh, _, err := s.Score(rank.Candidate{Stats: map[string]float64{"update_date": 19999}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	year, _, err := s.Score(rank.Candidate{Stats: map[string]float64{"update_date": 20000 - 365}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(year-0.5) > 0.01 {
		t.Fatalf("a year-old work scored %v, want ~0.5 on a 365-day halflife", year)
	}
	if fresh <= year {
		t.Fatalf("fresh %v <= year-old %v", fresh, year)
	}
}

func TestRecencyTreatsZeroAsUnknownNotAncient(t *testing.T) {
	// 0 means "unparseable or absent". Treating it as the epoch would rank
	// every undated work last, which is a claim the data does not support.
	s := Recency{Now: 20000}
	if _, _, err := s.Score(rank.Candidate{Stats: map[string]float64{}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("an undated work returned %v, want ErrSkip", err)
	}
}

func TestRecencyClampsFutureDates(t *testing.T) {
	s := Recency{Now: 20000}
	future, _, err := s.Score(rank.Candidate{Stats: map[string]float64{"update_date": 21000}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if future > 1.0001 {
		t.Fatalf("a future-dated work scored %v, want <= 1", future)
	}
}

func TestPopularityIsBoundedAndLogScaled(t *testing.T) {
	s := Popularity{}
	v, _, err := s.Score(rank.Candidate{Stats: map[string]float64{"hits": 1000000, "kudos": 50000}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v <= 0 || v > 1 {
		t.Fatalf("score = %v, want (0,1]", v)
	}
	if _, _, err := s.Score(rank.Candidate{Stats: map[string]float64{}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("a work with no hits or kudos returned %v", err)
	}
}

type stubEmbStore struct {
	vec []float32
	ok  bool
	err error
}

func (s stubEmbStore) Embedding(context.Context, string, int64) ([]float32, bool, error) {
	return s.vec, s.ok, s.err
}

func TestEmbeddingCosineSimilarity(t *testing.T) {
	store := stubEmbStore{vec: []float32{1, 0, 0}, ok: true}
	s := Embedding{Store: store, SeedVec: []float32{1, 0, 0}}
	v, _, err := s.Score(rank.Candidate{ID: 1, Kind: "k"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v-1) > 1e-6 {
		t.Fatalf("identical vectors scored %v, want 1", v)
	}
}

func TestEmbeddingSkipsOnAbsentOrMismatchedVectors(t *testing.T) {
	// A dimension mismatch must skip, not read past the end or produce a
	// cosine over the overlapping prefix.
	store := stubEmbStore{vec: []float32{1, 0, 0}, ok: true}
	s := Embedding{Store: store, SeedVec: []float32{1, 0}}
	if _, _, err := s.Score(rank.Candidate{ID: 1}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("a dimension mismatch returned %v", err)
	}
	absent := Embedding{Store: stubEmbStore{ok: false}, SeedVec: []float32{1}}
	if _, _, err := absent.Score(rank.Candidate{ID: 1}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("an absent embedding returned %v", err)
	}
}

func TestCosine(t *testing.T) {
	if got := Cosine([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("orthogonal vectors scored %v, want 0", got)
	}
	if got := Cosine([]float32{1, 0}, []float32{2, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("parallel vectors scored %v, want 1", got)
	}
	if got := Cosine(nil, []float32{1}); got != 0 {
		t.Fatalf("an empty vector scored %v", got)
	}
	if got := Cosine([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Fatalf("a zero vector scored %v; a zero magnitude is not a direction", got)
	}
	if got := Cosine([]float32{1}, []float32{1, 2}); got != 0 {
		t.Fatalf("mismatched lengths scored %v, want 0", got)
	}
}

func TestMeanVec(t *testing.T) {
	got := MeanVec([][]float32{{1, 0}, {3, 2}})
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("mean = %v, want [2 1]", got)
	}
	if MeanVec(nil) != nil {
		t.Fatal("an empty input must produce no vector")
	}
	// Vectors of differing length cannot be averaged together; skipping
	// them beats averaging a prefix.
	if got := MeanVec([][]float32{{1, 0, 0}, {3, 2}}); len(got) != 3 {
		t.Fatalf("mean of mismatched vectors = %v", got)
	}
}

func TestSortEvidencePutsTheBiggestContributionFirst(t *testing.T) {
	e := []rank.Evidence{
		{Signal: "a", Value: 0.1, Weight: 1},
		{Signal: "b", Value: 0.9, Weight: 1},
		{Signal: "c", Value: 0.5, Weight: 0.2},
	}
	SortEvidence(e)
	if e[0].Signal != "b" {
		t.Fatalf("evidence starts with %q, want the largest contribution first", e[0].Signal)
	}
}
