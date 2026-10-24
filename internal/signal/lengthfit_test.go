package signal

// LengthFit claims, each a way the score could be wrong.

import (
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

func work(id int64, words float64) rank.Candidate {
	c := rank.Candidate{ID: id}
	if words > 0 {
		c.Stats = map[string]float64{"word_count": words}
	}
	return c
}

func TestAWorkAtTheSeedsOwnLengthScoresOne(t *testing.T) {
	seeds := []rank.Candidate{work(1, 10000), work(2, 12000)}
	v, _, err := LengthFit{}.Score(work(9, 11000), seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v-1) > 0.06 {
		t.Fatalf("a work at the seeds' median scored %.3f, want ~1", v)
	}
}

// The falloff is LOGARITHMIC: one doubling is one doubling whether it happens at
// 5k or 500k. A linear difference would score 5k→10k as far more similar than
// 500k→505k, which is the opposite of how a reader experiences length.
func TestOneDoublingCostsTheSameAtEveryScale(t *testing.T) {
	short, _, _ := LengthFit{}.Score(work(9, 20000), []rank.Candidate{work(1, 10000)}, nil)
	long, _, _ := LengthFit{}.Score(work(9, 1000000), []rank.Candidate{work(1, 500000)}, nil)
	if math.Abs(short-long) > 1e-9 {
		t.Fatalf("doubling at 10k scored %.4f and at 500k scored %.4f; length is multiplicative", short, long)
	}
	// And the value is the one documented: the ratio of the smaller to the larger,
	// so a doubling is 1/2.
	if math.Abs(short-0.5) > 1e-9 {
		t.Fatalf("one doubling scored %.4f, want 0.5", short)
	}
}

func TestTwoDoublingsCostLessThanThree(t *testing.T) {
	seeds := []rank.Candidate{work(1, 10000)}
	two, _, _ := LengthFit{}.Score(work(9, 40000), seeds, nil)
	four, _, _ := LengthFit{}.Score(work(9, 160000), seeds, nil)
	if !(two > four) {
		t.Fatalf("4x scored %.4f and 16x scored %.4f; the score must fall with distance", two, four)
	}
}

// A work whose length is unknown is not a zero-word work. Scoring it 0 would rank
// it as maximally distant -- a claim about a work whose length was never read.
func TestAWorkWithNoLengthSkipsRatherThanScoringZero(t *testing.T) {
	_, _, err := LengthFit{}.Score(work(9, 0), []rank.Candidate{work(1, 10000)}, nil)
	if err != rank.ErrSkip {
		t.Fatalf("a work with no word count returned err=%v, want ErrSkip", err)
	}
}

// And a REQUEST whose seeds have no lengths at all skips: the reader's length
// preference is unmeasured, which is not the same as "all lengths are equal".
func TestSeedsWithNoLengthsSkipTheWholeRequest(t *testing.T) {
	_, _, err := LengthFit{}.Score(work(9, 10000), []rank.Candidate{work(1, 0), work(2, 0)}, nil)
	if err != rank.ErrSkip {
		t.Fatalf("seeds with no lengths returned err=%v, want ErrSkip", err)
	}
}

// One silent seed among several must not drag the median toward zero: the seed is
// missing information, not a very short book.
func TestASeedWithoutALengthIsIgnoredRatherThanCountedAsZero(t *testing.T) {
	withSilent := []rank.Candidate{work(1, 10000), work(2, 0), work(3, 10000)}
	clean := []rank.Candidate{work(1, 10000), work(3, 10000)}
	a, _, _ := LengthFit{}.Score(work(9, 10000), withSilent, nil)
	b, _, _ := LengthFit{}.Score(work(9, 10000), clean, nil)
	if math.Abs(a-b) > 1e-9 {
		t.Fatalf("a silent seed changed the score from %.4f to %.4f", b, a)
	}
	if a < 0.99 {
		t.Fatalf("score = %.4f, want ~1: a zero-length seed pulled the median down", a)
	}
}

// The median, not the mean: one 400k-word epic among twenty bookmarks must not move
// the target the way it moves an average.
func TestTheSeedTargetIsAMedianNotAMean(t *testing.T) {
	seeds := []rank.Candidate{
		work(1, 5000), work(2, 6000), work(3, 7000), work(4, 400000),
	}
	// Median 6,500 -> a 6,500-word work is a perfect fit. The mean would be
	// 104,500 and this work would score about 0.06.
	v, _, _ := LengthFit{}.Score(work(9, 6500), seeds, nil)
	if v < 0.99 {
		t.Fatalf("a work at the median scored %.4f: the mean is being used", v)
	}
}

func TestTheMedianSortsACopyAndLeavesTheSeedsAlone(t *testing.T) {
	seeds := []rank.Candidate{work(3, 30000), work(1, 10000), work(2, 20000)}
	LengthFit{}.Score(work(9, 20000), seeds, nil)
	if seeds[0].ID != 3 || seeds[1].ID != 1 || seeds[2].ID != 2 {
		t.Fatalf("the caller's seed order was sorted in place: %d,%d,%d",
			seeds[0].ID, seeds[1].ID, seeds[2].ID)
	}
}
