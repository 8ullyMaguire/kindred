package signal

import (
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// agreementFixture gives every tag in the "equal weight" group the same
// document frequency, so a coverage fraction is readable from the test body
// without recomputing logarithms: 100 works out of 1000 is idf log(10) for
// all of 10..16.
func agreementFixture() SeedAgreement {
	df := map[int32]float64{
		1:  1000, // on every work: contributes nothing
		10: 100, 11: 100, 12: 100, 13: 100,
		14: 100, 15: 100, 16: 100,
		20: 10,  // rare
		21: 900, // almost universal
	}
	return SeedAgreement{DocFreq: df, Total: 1000, MaxTags: 200}
}

func agreedCand(tags ...int32) rank.Candidate {
	return rank.Candidate{TagIDs: tags}
}

// TestAgreementRewardsBreadthOverDepth is idea 33's whole claim: a work that
// matches ONE of four seeds perfectly is a worse recommendation than a work
// that matches most of them partly. Every max()-over-seeds signal in this
// package scores both at 1.0, which is why this one exists.
func TestAgreementRewardsBreadthOverDepth(t *testing.T) {
	s := agreementFixture()
	seeds := []rank.Candidate{agreedCand(10, 11), agreedCand(12, 13), agreedCand(14, 15), agreedCand(16)}

	deep, _, err := s.Score(agreedCand(10, 11), seeds, nil)
	if err != nil {
		t.Fatalf("deep: %v", err)
	}
	broad, reason, err := s.Score(agreedCand(10, 12, 14, 16), seeds, nil)
	if err != nil {
		t.Fatalf("broad: %v", err)
	}

	// Depth on one seed out of four is a quarter, not a whole: the mean over
	// seeds includes the three that matched nothing. A division by the seeds
	// that MATCHED instead of the seeds CONSIDERED would report 1.0 here.
	if math.Abs(deep-0.25) > 1e-9 {
		t.Errorf("perfect match on 1 of 4 seeds = %v, want 0.25 (exactly)", deep)
	}
	// Coverage 0.5, 0.5, 0.5, 1.0 over four seeds.
	if math.Abs(broad-0.625) > 1e-9 {
		t.Errorf("partial match on 4 of 4 seeds = %v, want 0.625", broad)
	}
	if broad <= deep {
		t.Errorf("breadth %.3f did not beat depth %.3f — this is the max() behaviour the signal exists to replace", broad, deep)
	}
	if want := "agrees with 4 of 4 seeds"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

// TestAgreementWeightsEachSeedByRarity pins that "matching" a seed means
// matching the part of it that discriminates. A work sharing only the
// near-universal tag has not agreed with the seed in any useful sense.
func TestAgreementWeightsEachSeedByRarity(t *testing.T) {
	s := agreementFixture()
	// One seed carrying both a rare and an almost-universal tag, plus a
	// second seed so the signal has something to average over.
	seeds := []rank.Candidate{agreedCand(20, 21), agreedCand(10)}

	rareOnly, _, err := s.Score(agreedCand(20, 10), seeds, nil)
	if err != nil {
		t.Fatalf("rareOnly: %v", err)
	}
	commonOnly, _, err := s.Score(agreedCand(21, 10), seeds, nil)
	if err != nil {
		t.Fatalf("commonOnly: %v", err)
	}
	if rareOnly <= commonOnly {
		t.Errorf("matching the rare tag (%.4f) did not beat matching the near-universal one (%.4f)", rareOnly, commonOnly)
	}
	// log(1000/10) / (log(1000/10) + log(1000/900)) = 4.6/4.7, i.e. nearly the
	// whole seed; unweighted counting would give 0.5.
	if rareOnly < 0.9 {
		t.Errorf("rare-only coverage = %.4f, want ~0.978", rareOnly)
	}
}

// TestAgreementSkipsWhenThereIsNothingToAverage pins the refusals. One seed is
// idf_overlap under another name; two copies of one opinion is a double
// weight, not a second signal.
func TestAgreementSkipsWhenThereIsNothingToAverage(t *testing.T) {
	s := agreementFixture()

	cases := []struct {
		name  string
		c     rank.Candidate
		seeds []rank.Candidate
	}{
		{"one seed", agreedCand(10), []rank.Candidate{agreedCand(10)}},
		{"no seeds", agreedCand(10), nil},
		{"candidate with no tags", agreedCand(), []rank.Candidate{agreedCand(10), agreedCand(12)}},
		{
			// One usable seed plus one whose only tag is on every work: a
			// universal tag is zero evidence, so there is still only one
			// seed to agree with.
			"one usable seed", agreedCand(10), []rank.Candidate{agreedCand(10), agreedCand(1)},
		},
	}
	for _, tc := range cases {
		_, _, err := s.Score(tc.c, tc.seeds, nil)
		if !rank.IsSkip(err) {
			t.Errorf("%s: err = %v, want a skip", tc.name, err)
		}
	}
}

// TestAgreementCountsOnlySeedsWithRareTags pins that a seed's silence is not
// the candidate's fault: a tagless seed is not a zero in the average.
func TestAgreementCountsOnlySeedsWithRareTags(t *testing.T) {
	s := agreementFixture()
	seeds := []rank.Candidate{agreedCand(10, 11), agreedCand(12), agreedCand()} // the last carries no tags

	got, reason, err := s.Score(agreedCand(10, 11), seeds, nil)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	// Coverage 1.0 on the first, 0 on the second, and the third is not
	// counted at all: mean over two, not three.
	if math.Abs(got-0.5) > 1e-9 {
		t.Errorf("score = %v, want 0.5 (the tagless seed must not count)", got)
	}
	if want := "agrees with 1 of 2 seeds"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}
