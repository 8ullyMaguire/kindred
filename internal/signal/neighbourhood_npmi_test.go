package signal

import (
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// npmiTestGraph is a graph with real counts and frequencies, which pmiGraph does
// not have (it fixes Frequency at 1 and leaves Count at 0, so every nPMI it could
// produce would be 0).
//
// 1000 works. Seed tag 1 is on 100 works and co-occurs with tag 2 on 90 and with
// tags 3 and 4 on 12 each:
//
//	nPMI(1,2) = 0.9125   (p_ab .09 against marginals .01 -> ratio 9)
//	nPMI(1,3) = 0.0412   (p_ab .012 against .01 -> ratio 1.2)
type npmiTestGraph struct {
	freq  map[int32]float64
	edges map[int32][]graph.ScoredNeighbour
}

func newNPMITestGraph() npmiTestGraph {
	return npmiTestGraph{
		freq: map[int32]float64{1: 100, 2: 100, 3: 100, 4: 100},
		edges: map[int32][]graph.ScoredNeighbour{
			1: {
				{TagID: 2, Count: 90},
				{TagID: 3, Count: 12},
				{TagID: 4, Count: 12},
			},
		},
	}
}

func (g npmiTestGraph) Neighbours(id int32) ([]int32, []float32) {
	nbs := g.edges[id]
	ids := make([]int32, 0, len(nbs))
	ws := make([]float32, 0, len(nbs))
	for _, nb := range nbs {
		ids = append(ids, nb.TagID)
		ws = append(ws, float32(nb.Count))
	}
	return ids, ws
}

func (g npmiTestGraph) Frequency(id int32) float64 { return g.freq[id] }

func (g npmiTestGraph) Name(int32) string { return "tag" }

func (g npmiTestGraph) NeighbourScore(q int32, limit int, total float64) []graph.ScoredNeighbour {
	nbs := g.edges[q]
	out := make([]graph.ScoredNeighbour, 0, len(nbs))
	for _, nb := range nbs {
		out = append(out, graph.ScoredNeighbour{
			TagID: nb.TagID,
			PMI:   graph.PMI(g.freq[q], g.freq[nb.TagID], nb.Count, total),
			Count: nb.Count,
		})
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// The votes must be nPMI, and the score must be the MEAN over the candidate's tags,
// because a vote sum would have re-introduced exactly the scale problem nPMI exists
// to remove.
func TestNeighbourhoodNPMIPrefersOneStrongAssociationToWeakOnes(t *testing.T) {
	s := Neighbourhood{
		G: newNPMITestGraph(), SeedTags: []int32{1}, TotalWorks: 1000,
		NeighbourLimit: 8, Metric: "npmi",
	}
	strong, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{2}}, nil, nil)
	if err != nil {
		t.Fatalf("strong: %v", err)
	}
	weak, _, err := s.Score(rank.Candidate{ID: 2, TagIDs: []int32{3, 4}}, nil, nil)
	if err != nil {
		t.Fatalf("weak: %v", err)
	}
	if strong <= weak {
		t.Fatalf("strong association %.4f <= weak %.4f", strong, weak)
	}
	if strong > 1 || weak < 0 {
		t.Fatalf("nPMI metric left [0,1]: strong %.4f weak %.4f", strong, weak)
	}
}

// More tags must not buy score: the mean denominator is the whole point, and it is
// the property a sum silently lacks.
func TestNeighbourhoodNPMIDoesNotRewardPadding(t *testing.T) {
	s := Neighbourhood{
		G: newNPMITestGraph(), SeedTags: []int32{1}, TotalWorks: 1000,
		NeighbourLimit: 8, Metric: "npmi",
	}
	focused, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{2}}, nil, nil)
	if err != nil {
		t.Fatalf("focused: %v", err)
	}
	padded := []int32{2, 500, 501, 502, 503, 504, 505, 506, 507, 508}
	paddedScore, _, err := s.Score(rank.Candidate{ID: 2, TagIDs: padded}, nil, nil)
	if err != nil {
		t.Fatalf("padded: %v", err)
	}
	if paddedScore >= focused {
		t.Fatalf("padding did not dilute the score: focused %.4f, padded %.4f", focused, paddedScore)
	}
	if math.Abs(paddedScore-focused/10) > 1e-9 {
		t.Fatalf("padded score %.6f, want focused/10 = %.6f", paddedScore, focused/10)
	}
}

// The legacy metric must be untouched, byte for byte: the same candidate through
// Metric "" and Metric "pmi" must agree, and both must reproduce the saturated
// formula. This is the guard that lets an nPMI arm be compared with the shipped one.
func TestNeighbourhoodLegacyMetricIsUnchanged(t *testing.T) {
	mk := func(metric string) Neighbourhood {
		return Neighbourhood{
			G: pmiGraph{neighbour: 9200, pmi: 40}, SeedTags: []int32{9200},
			TotalWorks: 1000, Saturation: 100, Metric: metric,
		}
	}
	c := rank.Candidate{ID: 1, TagIDs: []int32{9200, 9201, 9202}}
	empty, _, err := mk("").Score(c, nil, nil)
	if err != nil {
		t.Fatalf("empty metric: %v", err)
	}
	explicit, _, err := mk("pmi").Score(c, nil, nil)
	if err != nil {
		t.Fatalf("pmi metric: %v", err)
	}
	if empty != explicit {
		t.Fatalf(`Metric "" = %.6f but Metric "pmi" = %.6f; the empty value must mean pmi`,
			empty, explicit)
	}
	raw := 3 * 40 / math.Sqrt(3)
	if want := raw / (raw + 100); math.Abs(explicit-want) > 1e-9 {
		t.Fatalf("legacy formula changed: got %.6f want %.6f", explicit, want)
	}
}

// Without a graph the signal skips rather than scoring zero, and an nPMI vote whose
// co-occurrence count is zero is not evidence (nPMI returns 0 there).
func TestNeighbourhoodNPMISkipsWithoutAGraph(t *testing.T) {
	s := Neighbourhood{SeedTags: []int32{9300}, Metric: "npmi"}
	if _, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{9301}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("no graph: err = %v, want rank.ErrSkip", err)
	}
}
