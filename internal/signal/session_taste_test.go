package signal

import (
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// The session_taste signal reads the reader's own learned tag preferences.
// These tests pin the three properties that matter:
//
//  1. It SKIPS rather than scoring zero when it has nothing to say. A zero
//     is a claim that the reader dislikes the work; a skip is a gap.
//  2. It DAMPENS by evidence count, so a weight learned from one comparison
//     cannot outvote a weight learned from many.
//  3. It reports WHICH tags matched, so a recommendation can be argued with.

func TestSessionTasteSkipsWithNoWeights(t *testing.T) {
	s := SessionTasteFrom(nil, nil)
	_, _, err := s.Score(rank.Candidate{TagIDs: []int32{1, 2, 3}}, nil, nil)
	if err != rank.ErrSkip {
		t.Fatalf("err = %v, want rank.ErrSkip", err)
	}
}

func TestSessionTasteSkipsWhenNoCandidateTagMatches(t *testing.T) {
	// The reader likes tag 10. The candidate carries tags 1 and 2. That is
	// no evidence, not negative evidence: the reader has never said they
	// dislike tags 1 and 2.
	s := SessionTasteFrom(map[int32]float64{10: 1.0}, map[int32]float64{10: 5})
	_, _, err := s.Score(rank.Candidate{TagIDs: []int32{1, 2}}, nil, nil)
	if err != rank.ErrSkip {
		t.Fatalf("err = %v, want rank.ErrSkip", err)
	}
}

func TestSessionTasteScoresAMatch(t *testing.T) {
	s := SessionTasteFrom(map[int32]float64{10: 1.0}, map[int32]float64{10: 4})
	v, reason, err := s.Score(rank.Candidate{TagIDs: []int32{10, 11}}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v <= 0 {
		t.Fatalf("value = %v, want > 0 for a liked tag", v)
	}
	if reason == "" {
		t.Fatal("reason is empty; a ranking that cannot explain itself is not one anyone can argue with")
	}
}

func TestSessionTasteDampensByEvidenceCount(t *testing.T) {
	// Two candidates, both matching the reader's liked tag. The first is
	// backed by one comparison, the second by forty. The well-evidenced
	// match must score higher, or a single impulse outweighs a
	// corroborated preference.
	one := SessionTasteFrom(map[int32]float64{10: 1.0}, map[int32]float64{10: 1})
	many := SessionTasteFrom(map[int32]float64{10: 1.0}, map[int32]float64{10: 40})

	cand := rank.Candidate{TagIDs: []int32{10}}

	vOne, _, err := one.Score(cand, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vMany, _, err := many.Score(cand, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vMany <= vOne {
		t.Fatalf("well-evidenced weight scored %v, single-comparison scored %v; want the evidenced one higher",
			vMany, vOne)
	}
}

func TestSessionTasteKeepsNegativeWeightsNegative(t *testing.T) {
	// A disliked tag must not be clamped to neutral. A reader who has
	// rejected a tag ten times deserves to see that reflected.
	s := SessionTasteFrom(map[int32]float64{10: -1.0}, map[int32]float64{10: 10})
	v, _, err := s.Score(rank.Candidate{TagIDs: []int32{10}}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v >= 0 {
		t.Fatalf("value = %v, want < 0 for a disliked tag", v)
	}
}

func TestSessionTasteMatchesMoreOfALikedTagSetBetter(t *testing.T) {
	// A candidate carrying three of the reader's liked tags should beat one
	// carrying a single liked tag, all else equal.
	s := SessionTasteFrom(
		map[int32]float64{10: 1.0, 11: 1.0, 12: 1.0},
		map[int32]float64{10: 4, 11: 4, 12: 4})

	three, _, err := s.Score(rank.Candidate{TagIDs: []int32{10, 11, 12}}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	one, _, err := s.Score(rank.Candidate{TagIDs: []int32{10, 99, 98}}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if three <= one {
		t.Fatalf("three matched tags scored %v, one scored %v; want the fuller match higher", three, one)
	}
}

func TestSessionTasteNameIsTheTuneKey(t *testing.T) {
	// The name IS the key a tune's weight vector uses. If it drifts, the
	// signal is registered but never weighted, and it would be absent
	// without saying so -- the exact failure this repo has found five times.
	if got := (SessionTaste{}).Name(); got != "session_taste" {
		t.Fatalf("Name() = %q, want %q", got, "session_taste")
	}
}
