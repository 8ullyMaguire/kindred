package signal

import (
	"errors"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// The owner-cohort signal is one person's preferences applied to every other
// reader, so the thing worth testing is the SHAPE of its answer: it scores the
// share of a candidate's taste mass the owner likes, it says why, and it
// distinguishes the two zeros that mean different things — "the owner has no
// opinion about this work" and "this deployment has no owner at all".

func TestOwnerTasteScoresTheShareOfTasteMassTheOwnerLikes(t *testing.T) {
	// Two liked, two disliked, equal magnitude: the share is exactly half.
	// A raw sum would answer 0 here and call the work uninteresting, which is
	// the answer to a different question.
	s := OwnerTaste{Weights: map[int32]float64{10: 2, 20: -2}}

	got, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{10, 20}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0.5 {
		t.Fatalf("score with one liked and one equally disliked tag = %v; want 0.5", got)
	}

	// Only liked tags: the whole mass is liked.
	got, _, err = s.Score(rank.Candidate{ID: 1, TagIDs: []int32{10}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1.0 {
		t.Fatalf("score with one liked tag = %v; want 1.0", got)
	}

	// Only disliked tags: nothing is liked, and that is a real 0 rather than
	// a skip, because the signal worked and the owner does have an opinion.
	got, _, err = s.Score(rank.Candidate{ID: 1, TagIDs: []int32{20}}, nil, nil)
	if err != nil {
		t.Fatalf("a disliked-only candidate must score, not skip: %v", err)
	}
	if got != 0 {
		t.Fatalf("score with one disliked tag = %v; want 0", got)
	}
}

// No overlap is a legitimate zero and must NOT be reported as a broken
// dimension. This is the distinction the whole ErrSkip convention exists for.
func TestOwnerTasteScoresZeroOnNoOverlapWithoutSkipping(t *testing.T) {
	s := OwnerTaste{Weights: map[int32]float64{10: 1}}

	got, reason, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{99}}, nil, nil)
	if err != nil {
		t.Fatalf("no overlap must not skip: %v", err)
	}
	if got != 0 {
		t.Fatalf("score with no shared tags = %v; want 0", got)
	}
	if reason == "" {
		t.Fatal("a zero from no overlap has no reason; it reads as silence")
	}
}

func TestOwnerTasteSkipsWhenThereIsNoOwner(t *testing.T) {
	// No weights: the deployment has no owner, which is the default state.
	// Skipping names the signal in meta.degraded[], so "no owner here" is
	// reported rather than inferred from a ranking that lost a dimension.
	_, _, err := (OwnerTaste{}).Score(rank.Candidate{ID: 1, TagIDs: []int32{10}}, nil, nil)
	if !errors.Is(err, rank.ErrSkip) {
		t.Fatalf("score with no owner vector returned %v; want rank.ErrSkip", err)
	}

	// Nil map and empty map are the same fact and must behave the same.
	_, _, err = (OwnerTaste{Weights: map[int32]float64{}}).Score(rank.Candidate{ID: 1, TagIDs: []int32{10}}, nil, nil)
	if !errors.Is(err, rank.ErrSkip) {
		t.Fatalf("score with an empty owner vector returned %v; want rank.ErrSkip", err)
	}
}

// A candidate with no tags is an absence of evidence about THAT candidate,
// not a missing dimension: it scores 0 and leaves the signal un-degraded.
// Skipping here would report the whole signal broken the first time an
// author or collection entered the pool.
func TestOwnerTasteScoresAnUntaggedCandidateZero(t *testing.T) {
	s := OwnerTaste{Weights: map[int32]float64{10: 1}}

	got, _, err := s.Score(rank.Candidate{ID: 1}, nil, nil)
	if err != nil {
		t.Fatalf("an untagged candidate must not skip the signal: %v", err)
	}
	if got != 0 {
		t.Fatalf("score for an untagged candidate = %v; want 0", got)
	}
}

func TestOwnerTasteBoundsTheTagMassItReads(t *testing.T) {
	// The bound is stated, so it is tested: a work's tag list is unbounded
	// in the mirror, and only the highest-weight tags can matter, so the
	// cap must actually stop the walk. Put the only liked tag past the cap.
	weights := map[int32]float64{500: 1}
	tags := make([]int32, 0, 12)
	for i := int32(1); i <= 12; i++ {
		tags = append(tags, i)
	}
	tags = append(tags, 500)

	s := OwnerTaste{Weights: weights, MaxTags: 12}
	got, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: tags}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("score = %v; the liked tag sits past MaxTags, so the cap did not bound the walk", got)
	}

	// And without the cap the same candidate scores, which is what proves
	// the earlier zero came from the cap rather than from a broken weight.
	s.MaxTags = 0 // package default
	got, _, err = s.Score(rank.Candidate{ID: 1, TagIDs: tags}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1.0 {
		t.Fatalf("score without the cap = %v; want 1.0", got)
	}
}

// The evidence line names the two heaviest contributors, and it is STABLE.
// Map iteration is random in Go, so a reason built by walking the map changes
// between two identical requests — indistinguishable from a bug, and it is the
// line a reader is supposed to be able to argue with.
func TestOwnerTasteReasonNamesTheHeaviestContributorsDeterministically(t *testing.T) {
	s := OwnerTaste{Weights: map[int32]float64{1: 0.1, 2: 5, 3: -9, 4: 0.2}}

	const want = "owner cohort: -tag:3 +tag:2"
	for i := 0; i < 20; i++ {
		_, reason, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{1, 2, 3, 4}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if reason != want {
			t.Fatalf("call %d: reason = %q; want %q", i, reason, want)
		}
	}
}

// A NaN weight must not poison the score: NaN compares false against
// everything, so a single one in the vector would make the candidate's score
// NaN and its position in the sort undefined rather than merely wrong.
func TestOwnerTasteIgnoresNonFiniteWeights(t *testing.T) {
	nan := 0.0
	nan = nan / nan
	s := OwnerTaste{Weights: map[int32]float64{10: nan, 20: 1}}

	got, _, err := s.Score(rank.Candidate{ID: 1, TagIDs: []int32{10, 20}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1.0 {
		t.Fatalf("score with a NaN weight in the vector = %v; want 1.0 (the finite half only)", got)
	}
}
