package eval

// Calibration claims, each one a way the number could be wrong.
//
//   The denominators: a short list must not be scored as missing the positions it
//   never filled, and a repeat inside a list must not shift its neighbours.
//
//   The rate: a position nothing reached has NO rate, and printing 0 for it is a
//   claim the run did not make.
//
//   The sample: three readers produce rates of 0, 0.33, 0.67 and 1, so a rate under
//   MinCalibrationReaders readers is withheld and the page says why.
//
//   The meaning: "right" is "the reader later bookmarked it", and the sentence has
//   to say that rather than promise that this reader will like this work.

import (
	"context"
	"strings"
	"testing"
)

// The test does not need a fake recommender at all: Run's calibration comes from
// the positions it walks, so a recommender that returns a fixed list per reader is
// enough. This one is keyed by the train set's first id.
type listRec map[int64][]int64 // first train id -> returned list

func (l listRec) Recommend(_ context.Context, train []int64, k int) ([]int64, error) {
	if len(train) == 0 {
		return nil, nil
	}
	ids := l[train[0]]
	if len(ids) > k {
		ids = ids[:k]
	}
	out := make([]int64, len(ids))
	copy(out, ids)
	return out, nil
}

func TestAShortListDoesNotMissThePositionsItNeverFilled(t *testing.T) {
	// Two readers at k=4. One returns 4 ids, the other 2 (and both ids are
	// held-out). Counting the short list as two misses at positions 3 and 4 would
	// halve the rate at the tail for no reason the recommender can act on.
	split := Split{
		Train: map[int64][]int64{1: {10, 11}, 2: {20, 21}},
		Test:  map[int64][]int64{1: {100}, 2: {200, 201}},
	}
	rec := listRec{
		10: {900, 100, 901, 902}, // hit at position 2
		20: {200, 201},           // hits at positions 1 and 2, nothing after
	}
	rep, err := Run(context.Background(), rec, split, 4)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, p := range rep.Calibration.Positions {
		if p.Position <= 2 && p.Observed != 2 {
			t.Fatalf("position %d observed %d, want 2", p.Position, p.Observed)
		}
		if p.Position > 2 && p.Observed != 1 {
			t.Fatalf("position %d observed %d, want 1 — the short list never filled it", p.Position, p.Observed)
		}
	}
	if p, ok := rep.Calibration.At(4); !ok || p.Hits != 0 {
		t.Fatalf("position 4 = %+v, want one observation and no hit", p)
	}
	// And the tail rate must be 0/1, not 0/2: one observation, no hit.
	p4, _ := rep.Calibration.At(4)
	if r, _ := p4.Rate(); r != 0 {
		t.Fatalf("position 4 rate = %v, want 0", r)
	}
	if _, ok := p4.Rate(); !ok {
		t.Fatal("position 4 has an observation, so it has a rate")
	}
}

func TestAPositionNothingReachedHasNoRateRatherThanZero(t *testing.T) {
	split := Split{
		Train: map[int64][]int64{1: {10}},
		Test:  map[int64][]int64{1: {100}},
	}
	rec := listRec{10: {100}} // one id, so positions 2..4 are unreached
	rep, err := Run(context.Background(), rec, split, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rep.Calibration.Positions {
		if p.Observed == 0 {
			t.Fatalf("a position with no observation was recorded: %+v", p)
		}
	}
	if _, ok := rep.Calibration.At(4); ok {
		t.Fatal("position 4 was never filled, so the run makes no claim about it")
	}
}

func TestARepeatedWorkDoesNotShiftItsNeighbours(t *testing.T) {
	// [100, 100, 200]: the repeat occupies no new slot, so 200 is at position 2 and
	// a hit. An index-based position would call it position 3.
	split := Split{
		Train: map[int64][]int64{1: {10}},
		Test:  map[int64][]int64{1: {100, 200}},
	}
	rec := listRec{10: {100, 100, 200}}
	rep, err := Run(context.Background(), rec, split, 3)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := rep.Calibration.At(1)
	second, _ := rep.Calibration.At(2)
	if first.Hits != 1 || second.Hits != 1 {
		t.Fatalf("positions 1 and 2 = %+v / %+v, want one hit each", first, second)
	}
	if third, ok := rep.Calibration.At(3); ok {
		t.Fatalf("position 3 recorded %+v; the list had two distinct works", third)
	}
}

func TestABandExcludesPositionsItNeverObserved(t *testing.T) {
	c := Calibration{K: 10, Readers: 40, Positions: []PositionRate{
		{Position: 1, Hits: 10, Observed: 40},
		{Position: 2, Hits: 6, Observed: 40},
		{Position: 3, Hits: 4, Observed: 40},
	}}
	top := c.Band(1, 3)
	if top.Hits != 20 || top.Observed != 120 {
		t.Fatalf("band 1-3 = %+v, want 20/120", top)
	}
	tail := c.Band(4, 10)
	if tail.Observed != 0 {
		t.Fatalf("band 4-10 = %+v, want no observations", tail)
	}
	if _, ok := tail.Rate(); ok {
		t.Fatal("an unobserved band has no rate")
	}
}

func TestARateUnderTwentyReadersIsWithheldWithItsCount(t *testing.T) {
	thin := Calibration{K: 10, Readers: 3, Positions: []PositionRate{{Position: 1, Hits: 1, Observed: 3}}}
	s := thin.Sentence()
	if strings.Contains(s, "%") {
		t.Fatalf("a 3-reader run printed a percentage: %q", s)
	}
	if !strings.Contains(s, "3 reader") || !strings.Contains(s, "too few") {
		t.Fatalf("sentence does not say why there is no rate: %q", s)
	}
	enough := Calibration{K: 10, Readers: 40, Positions: []PositionRate{
		{Position: 1, Hits: 10, Observed: 40},
		{Position: 2, Hits: 4, Observed: 40},
	}}
	// The sentence reports the BAND (1-3), which is 14/80 = 17.5% here, not the
	// single position's 25%. Both are claims; the page has room for one.
	if got := enough.Sentence(); !strings.Contains(got, "18%") {
		t.Fatalf("40-reader sentence = %q, want the band's measured rate", got)
	}
	if p1, _ := enough.At(1); true {
		if r, ok := p1.Rate(); !ok || r != 0.25 {
			t.Fatalf("position 1 rate = %v/%v, want 0.25", r, ok)
		}
	}
}

func TestAnUnmeasuredCalibrationSaysSoInsteadOfShowingZero(t *testing.T) {
	empty := Calibration{K: 10, Readers: 40}
	if empty.Measured() {
		t.Fatal("a calibration with no positions reports itself as measured")
	}
	s := empty.Sentence()
	if strings.Contains(s, "0%") || strings.Contains(s, "1 in") {
		t.Fatalf("an unmeasured calibration printed a rate: %q", s)
	}
	if !strings.Contains(s, "not calibrated") {
		t.Fatalf("sentence does not say it is not calibrated: %q", s)
	}
	skipped := Calibration{K: 10, Readers: 40, Skipped: "no reader's list reached a position"}
	if got := skipped.Sentence(); got != "no reader's list reached a position" {
		t.Fatalf("the recorded reason was replaced by %q", got)
	}
}

func TestTheSentenceNamesTheOutcomeAndThePopulation(t *testing.T) {
	c := Calibration{K: 10, Readers: 40, Positions: []PositionRate{
		{Position: 1, Hits: 12, Observed: 40},
	}}
	s := c.Sentence()
	for _, want := range []string{"40 readers", "bookmarked", "not that a particular reader"} {
		if !strings.Contains(s, want) {
			t.Fatalf("sentence %q is missing %q", s, want)
		}
	}
}
