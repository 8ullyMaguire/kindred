package signal

import (
	"errors"
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// peer_rating is the arena's contribution to ranking, and SPEC §7.1 makes
// one property of it non-negotiable: if it is not contributing, the response
// has to say so in meta.degraded[]. The bug SPEC §1 records in kindling was an
// arena signal that was inert in production AND silent about it, which is
// indistinguishable from a signal that was never written. So the two tests
// that matter most here are the two about absence.

func cand(id int64) rank.Candidate { return rank.Candidate{ID: id} }

func TestPeerRatingName(t *testing.T) {
	if got := (PeerRating{}).Name(); got != "peer_rating" {
		t.Fatalf("Name() = %q, want peer_rating (the tune key is the name)", got)
	}
}

// An arena with no ratings must SKIP, not score zero. Skipping is what puts
// the name in meta.degraded[]; scoring zero would be a silent claim that the
// crowd rated everything average.
func TestPeerRatingSkipsWhenTheArenaIsEmpty(t *testing.T) {
	s := PeerRating{}
	_, _, err := s.Score(cand(1), nil, nil)
	if !errors.Is(err, rank.ErrSkip) {
		t.Fatalf("with no ratings, Score returned %v, want rank.ErrSkip so the "+
			"signal reports itself degraded", err)
	}
}

// A nil map is the same condition as an empty one, and serve.go sets it on
// the load-failure path.
func TestPeerRatingSkipsOnANilMap(t *testing.T) {
	s := PeerRating{Ratings: nil, Median: 1500}
	_, _, err := s.Score(cand(1), nil, nil)
	if !errors.Is(err, rank.ErrSkip) {
		t.Fatalf("a nil map returned %v, want rank.ErrSkip", err)
	}
}

// A rated arena with an UNRATED candidate is not a degraded signal. There is
// simply no evidence about this work, which is different from the evidence
// being absent for every work. Skipping here would report the whole signal
// degraded the first time an unrated work entered the pool, which is noise
// about a condition that is working.
func TestPeerRatingScoresZeroForAnUnratedCandidate(t *testing.T) {
	s := PeerRating{Ratings: map[int64]float64{1: 1700}, Median: 1500}
	score, _, err := s.Score(cand(99), nil, nil)
	if err != nil {
		t.Fatalf("an unrated candidate returned %v, want nil so the signal "+
			"stays un-degraded", err)
	}
	if score != 0 {
		t.Errorf("score = %v, want 0 for a work the arena has not rated", score)
	}
}

// Above the median scores positive, below it negative, and a work exactly at
// the median scores 0. A signal that cannot produce a negative score cannot
// express "the readers preferred the other one", which is most of what the
// arena knows.
func TestPeerRatingIsCentredOnTheMedian(t *testing.T) {
	s := PeerRating{
		Ratings: map[int64]float64{1: 1700, 2: 1500, 3: 1300},
		Median:  1500,
	}
	cases := []struct {
		id   int64
		want float64
	}{
		{1, (1700 - 1500) / 150},
		{2, 0},
		{3, (1300 - 1500) / 150},
	}
	for _, c := range cases {
		got, reason, err := s.Score(cand(c.id), nil, nil)
		if err != nil {
			t.Fatalf("work %d: %v", c.id, err)
		}
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("work %d score = %v, want %v", c.id, got, c.want)
		}
		if reason == "" {
			t.Errorf("work %d has no reason string; every scored candidate "+
				"must be explainable in the response", c.id)
		}
	}
}

// The score is clamped. A rating 2000 points above the median is a broken
// arena, not a reason for one candidate to dominate the weighted sum and
// reorder the whole list by itself.
func TestPeerRatingClamps(t *testing.T) {
	s := PeerRating{
		Ratings: map[int64]float64{1: 100000, 2: 0},
		Median:  1500,
	}
	high, _, err := s.Score(cand(1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if high != 1 {
		t.Errorf("score above the ceiling = %v, want 1", high)
	}
	low, _, err := s.Score(cand(2), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if low != -1 {
		t.Errorf("score below the floor = %v, want -1", low)
	}
}

// A zero Scale must not divide by zero. serve.go leaves it at the zero value,
// so this is the default path, not an edge case.
func TestPeerRatingDefaultsItsScale(t *testing.T) {
	s := PeerRating{Ratings: map[int64]float64{1: 1650}, Median: 1500}
	score, _, err := s.Score(cand(1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.IsNaN(score) || math.IsInf(score, 0) {
		t.Fatalf("score = %v with no Scale set; the default must be the "+
			"arena's own deviation, not a division by zero", score)
	}
	if score <= 0 {
		t.Errorf("score = %v, want positive for a work rated above the median", score)
	}
}
