package signal

import (
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// pmiGraph is a one-hop graph: every seed tag reaches exactly one neighbour
// with a fixed PMI vote, so a candidate's raw score is predictable from the
// number of its tags that appear in that map.
type pmiGraph struct {
	neighbour int32
	pmi       float32
}

func (pmiGraph) Neighbours(int32) ([]int32, []float32) { return nil, nil }
func (pmiGraph) Frequency(int32) float64               { return 1 }
func (pmiGraph) Name(id int32) string                  { return "tag" }
func (g pmiGraph) NeighbourScore(query int32, limit int, total float64) []graph.ScoredNeighbour {
	out := make([]graph.ScoredNeighbour, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, graph.ScoredNeighbour{TagID: g.neighbour + int32(i), PMI: float64(g.pmi)})
	}
	return out
}

// TestNeighbourhoodIsOnTheSameScaleAsEveryOtherSignal is the reason this
// change exists. The raw score is a SUM of PMI votes and ran from 20 to 130 on
// the live mirror against signals that are already in [0,1]; with weight 0.32
// it contributed 39.5 of every item's 39.7 total, so no tune could change the
// ranking. Three evaluations with different tunes came back byte-identical.
func TestNeighbourhoodIsOnTheSameScaleAsEveryOtherSignal(t *testing.T) {
	s := Neighbourhood{
		G:          pmiGraph{neighbour: 100, pmi: 40},
		SeedTags:   []int32{1},
		TotalWorks: 1000,
	}
	sig := s
	// A candidate with one matching tag scores raw 40/sqrt(3) ≈ 23, which
	// used to be returned as 23 and now is on [0,1).
	c := rank.Candidate{ID: 1, TagIDs: []int32{100, 101, 102}}
	got, _, err := sig.Score(c, nil, nil)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if got >= 1 || got <= 0 {
		t.Fatalf("score %v is not inside (0,1); the signal is still on its own scale", got)
	}
	// 69/(69+100) ≈ 0.41 — the observed live range maps into a range a weight
	// of 0.32 can compare with tag_overlap's 0-1.
	if math.Abs(got-0.409) > 0.02 {
		t.Errorf("score = %.4f, want ~0.41 for raw ~69", got)
	}
}

// TestNeighbourhoodSaturatesRatherThanGrowingWithoutBound pins the saturation
// curve's shape: more evidence always scores higher, but a candidate with
// twice the raw evidence must not score twice as much — otherwise the signal
// is back to dominating by construction.
func TestNeighbourhoodSaturatesRatherThanGrowingWithoutBound(t *testing.T) {
	mk := func(pmi float32, seed int32) Neighbourhood {
		return Neighbourhood{
			G: pmiGraph{neighbour: 100, pmi: pmi}, SeedTags: []int32{seed},
			TotalWorks: 1000, Saturation: 100,
		}
	}
	small, _, err := mk(10, 9101).Score(rank.Candidate{ID: 1, TagIDs: []int32{100}}, nil, nil)
	if err != nil {
		t.Fatalf("small: %v", err)
	}
	large, _, err := mk(20, 9102).Score(rank.Candidate{ID: 1, TagIDs: []int32{100}}, nil, nil)
	if err != nil {
		t.Fatalf("large: %v", err)
	}
	if large <= small {
		t.Errorf("doubling the evidence lowered the score (%.4f -> %.4f)", small, large)
	}
	if large >= 2*small {
		t.Errorf("doubling the evidence more than doubled the score (%.4f -> %.4f); the curve does not saturate", small, large)
	}
	// And it cannot reach 1 no matter how large the evidence.
	huge, _, err := mk(10000, 9103).Score(rank.Candidate{ID: 1, TagIDs: []int32{100}}, nil, nil)
	if err != nil {
		t.Fatalf("huge: %v", err)
	}
	if huge >= 1 {
		t.Errorf("score %v reached 1; the signal must stay inside [0,1)", huge)
	}
}
