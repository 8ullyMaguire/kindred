package signal

// The latent signal (#89), on two hand-built factor rows.
//
// The properties that matter are the ones a reader could check from the evidence line:
// it scores the BEST seed rather than the average, it never scores below zero, it skips
// (rather than zeroes) when there is no model or no factor row, and its reason names the
// seed it scored against.

import (
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

func latentModel() map[int64][]float32 {
	return map[int64][]float32{
		1: {1, 0, 0, 0},
		2: {0.9, 0.1, 0, 0}, // close to seed 1
		3: {0, 1, 0, 0},     // orthogonal
		4: {-1, 0, 0, 0},    // opposite
	}
}

func TestLatentScoresTheBestSeedNotTheAverage(t *testing.T) {
	s := Latent{Load: latentModel}
	seeds := []rank.Candidate{{ID: 1}, {ID: 3}}
	// Candidate 4 is opposite to seed 1 and orthogonal to seed 3: the best score is 0.
	if v, _, err := s.Score(rank.Candidate{ID: 4}, seeds, nil); err != nil || v != 0 {
		t.Fatalf("opposite candidate scored %v (err %v), want 0", v, err)
	}
	// Candidate 2 is close to seed 1: the score must be its similarity to THAT seed, not
	// the mean of a high and a zero.
	v, reason, err := s.Score(rank.Candidate{ID: 2}, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v-0.9938) > 0.001 {
		t.Fatalf("candidate 2 scored %v, want ~0.9938 (its cosine to seed 1)", v)
	}
	if reason == "" {
		t.Fatal("no reason: the evidence line would be blank")
	}
}

// A candidate or seed with no factor row is a skip, not a zero. The distinction is the
// package's oldest rule: a zero is a claim and a skip is a gap.
func TestLatentSkipsWithoutFactors(t *testing.T) {
	// No model at all.
	none := Latent{Load: func() map[int64][]float32 { return nil }}
	if _, _, err := none.Score(rank.Candidate{ID: 1}, []rank.Candidate{{ID: 2}}, nil); err != rank.ErrSkip {
		t.Fatalf("no model gave err %v, want ErrSkip", err)
	}
	// No loader.
	if _, _, err := (Latent{}).Score(rank.Candidate{ID: 1}, []rank.Candidate{{ID: 2}}, nil); err != rank.ErrSkip {
		t.Fatalf("no loader gave err %v, want ErrSkip", err)
	}
	// Candidate outside the graph.
	s := Latent{Load: latentModel}
	if _, _, err := s.Score(rank.Candidate{ID: 999}, []rank.Candidate{{ID: 1}}, nil); err != rank.ErrSkip {
		t.Fatalf("unknown candidate gave err %v, want ErrSkip", err)
	}
	// Seeds outside the graph: nothing to compare against.
	if _, _, err := s.Score(rank.Candidate{ID: 2}, []rank.Candidate{{ID: 999}}, nil); err != rank.ErrSkip {
		t.Fatalf("unknown seeds gave err %v, want ErrSkip", err)
	}
}

// The loader must be called at most once per Score invocation, and the signal must work
// with more seeds than MaxSeeds allows.
func TestLatentRespectsMaxSeeds(t *testing.T) {
	calls := 0
	model := latentModel()
	s := Latent{Load: func() map[int64][]float32 { calls++; return model }, MaxSeeds: 1}
	// Candidate 1 is identical to seed 1, which sits beyond the cap; the only seed inside
	// it (3) is orthogonal to it.
	seeds := []rank.Candidate{{ID: 3}, {ID: 1}}
	if v, _, _ := s.Score(rank.Candidate{ID: 1}, seeds, nil); v != 0 {
		t.Fatalf("scored %v against a seed beyond MaxSeeds: the cap is not applied", v)
	}
	// With no cap the same call finds the identical seed and scores 1.
	all := Latent{Load: func() map[int64][]float32 { return model }}
	if v, _, _ := all.Score(rank.Candidate{ID: 1}, seeds, nil); math.Abs(v-1) > 1e-9 {
		t.Fatalf("uncapped score %v, want 1 for an identical seed", v)
	}
	if calls != 1 {
		t.Fatalf("the loader ran %d times in one Score", calls)
	}
}
