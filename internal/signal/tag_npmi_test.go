package signal

import (
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// tagNPMIFixture is a consistent corpus: 1000 works, and no co-occurrence larger
// than either tag's own document frequency.
//
//	freq(1)=100  freq(2)=100  freq(3)=5  freq(4)=900
//	edges of tag 1: 2 x90, 3 x5, 4 x20
//
//	nPMI(1,2) = 0.9125   p_ab .09  against marginals .01
//	nPMI(1,3) = 0.4346   p_ab .005 against .0005
//	nPMI(1,4) = -0.455   less than chance -> dropped, so tag 4 has no association
//
// idfNorm: 3 -> 0.767, 4 -> 0.015, 2 -> 0.333.
func tagNPMIFixture(seeds ...int32) TagNPMI {
	freq := map[int32]float64{1: 100, 2: 100, 3: 5, 4: 900}
	if len(seeds) == 0 {
		seeds = []int32{1}
	}
	return TagNPMI{
		G: npmiTestGraph{
			freq: freq,
			edges: map[int32][]graph.ScoredNeighbour{
				1: {
					{TagID: 2, Count: 90},
					{TagID: 3, Count: 5},
					{TagID: 4, Count: 20},
				},
			},
		},
		DocFreq: freq, Total: 1000,
		SeedTags: seeds, NeighbourLimit: 8, MinCount: 1,
	}
}

func TestTagNPMIScoresStayInsideTheUnitInterval(t *testing.T) {
	s := tagNPMIFixture(1)
	for _, tags := range [][]int32{{3}, {4}, {2}, {3, 4, 2}, {2, 2, 2}} {
		got, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: tags}, nil, nil)
		if err != nil {
			t.Fatalf("tags %v: unexpected skip: %v", tags, err)
		}
		if math.IsNaN(got) || got < 0 || got > 1 {
			t.Fatalf("tags %v: score %v outside [0,1]", tags, got)
		}
	}
}

// The IDF property: a shared RARE tag must beat a shared UBIQUITOUS one. This is
// what idf_overlap was built for and TagNPMI must keep it.
//
// Both tags are in SeedTags on purpose. The direct term applies only to a tag the
// seeds also carry, so a fixture where they are merely neighbours would exercise
// the association half and pass while the IDF half was missing.
func TestTagNPMIPrefersASharedRareTagToASharedUniversalOne(t *testing.T) {
	s := tagNPMIFixture(1, 3, 4)
	rare, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{3}}, nil, nil)
	if err != nil {
		t.Fatalf("rare: %v", err)
	}
	universal, _, err := s.Score(rank.Candidate{ID: 2, TagIDs: []int32{4}}, nil, nil)
	if err != nil {
		t.Fatalf("universal: %v", err)
	}
	if rare <= universal {
		t.Fatalf("rare shared tag scored %.4f, universal %.4f — the IDF half is missing", rare, universal)
	}
	if universal > 0.05 {
		t.Fatalf("universal shared tag scored %.4f, want near 0 (idfNorm(900/1000) = 0.015)", universal)
	}
}

// The association property: a candidate sharing NO tag but sitting in the seed's
// neighbourhood must score above zero. idf_overlap scores this 0 by construction,
// which is the whole reason for the hybrid.
func TestTagNPMIScoresAnUnsharedAssociatedTag(t *testing.T) {
	s := tagNPMIFixture(1)
	got, reason, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{2}}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected skip: %v", err)
	}
	if got <= 0 {
		t.Fatalf("associated-but-unshared tag scored %v, want > 0 (reason %q)", got, reason)
	}
	if got < 0.5 {
		t.Fatalf("association scored %.4f, want ~0.91 for nPMI(1,2)", got)
	}
}

// Nothing shared and nothing associated is a real zero, not a skipped dimension: the
// candidate carries no evidence for these seeds. Skipping would let it inherit the
// other signals' ranking without being penalised.
func TestTagNPMIScoresZeroWhenThereIsNoEvidence(t *testing.T) {
	s := tagNPMIFixture(1)
	got, reason, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{999}}, nil, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil (a zero is a result)", err)
	}
	if got != 0 {
		t.Fatalf("score = %v, want 0", got)
	}
	if reason == "" {
		t.Fatal("a zero score must still explain itself")
	}
}

// No graph, no docfreq, no seeds: SKIP, never score 0. A skipped signal contributes
// no term and lands in meta.degraded, which is the only way "no data" stays
// distinguishable from "broken".
func TestTagNPMISkipsWithoutItsInputs(t *testing.T) {
	noGraph := TagNPMI{DocFreq: map[int32]float64{1: 100}, Total: 1000, SeedTags: []int32{1}}
	if _, _, err := noGraph.Score(rank.Candidate{TagIDs: []int32{1}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("no graph: err = %v, want ErrSkip", err)
	}
	noDF := TagNPMI{G: npmiTestGraph{}, Total: 1000, SeedTags: []int32{1}}
	if _, _, err := noDF.Score(rank.Candidate{TagIDs: []int32{1}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("no docfreq: err = %v, want ErrSkip", err)
	}
	noSeeds := tagNPMIFixture(1)
	noSeeds.SeedTags = nil
	if _, _, err := noSeeds.Score(rank.Candidate{TagIDs: []int32{1}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("no seed tags: err = %v, want ErrSkip", err)
	}
	noTags := tagNPMIFixture(1)
	if _, _, err := noTags.Score(rank.Candidate{TagIDs: nil}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("no candidate tags: err = %v, want ErrSkip", err)
	}
}

// One definition of "how rare is this tag, normalised": a tag on one work is 1, a
// tag on every work is 0, and a tag absent from the table is maximally rare (the
// same treatment idfOf gives a table-completeness gap).
func TestIDFNormEndpoints(t *testing.T) {
	if got := idfNorm(1, 1000); math.Abs(got-1) > 1e-9 {
		t.Fatalf("idfNorm(singleton) = %v, want 1", got)
	}
	if got := idfNorm(1000, 1000); got != 0 {
		t.Fatalf("idfNorm(universal) = %v, want 0", got)
	}
	if got := idfNorm(0, 1000); math.Abs(got-1) > 1e-9 {
		t.Fatalf("idfNorm(absent) = %v, want 1", got)
	}
	if got := idfNorm(100, 1); got != 0 {
		t.Fatalf("idfNorm with a corpus of one = %v, want 0", got)
	}
}

// Padding must not buy score: the denominator is every distinct tag the candidate
// carries, so twenty irrelevant tags dilute a real match instead of adding chances.
func TestTagNPMIDoesNotRewardPadding(t *testing.T) {
	s := tagNPMIFixture(1)
	focused, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{2}}, nil, nil)
	if err != nil {
		t.Fatalf("focused: %v", err)
	}
	padded := append([]int32{2}, 900, 901, 902, 903, 904, 905, 906, 907, 908, 909)
	paddedScore, _, err := s.Score(rank.Candidate{ID: 2, TagIDs: padded}, nil, nil)
	if err != nil {
		t.Fatalf("padded: %v", err)
	}
	if paddedScore >= focused {
		t.Fatalf("padding did not dilute: focused %.4f, padded %.4f", focused, paddedScore)
	}
}
