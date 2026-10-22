package signal

import (
	"fmt"
	"math"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// SessionTaste scores a candidate against the reader's OWN learned tag
// preferences.
//
// The preferences are not new. `arena_user_tag_weights` has been storing them
// for the arena's own pair-selection, and `Store.TagWeights` reads them back;
// what was missing was the recommender reading them. A reader who has told
// the arena "I pick this one" forty times has a taste vector, and a ranking
// that ignores it is a ranking that throws away the only evidence this
// deployment has of what that reader actually wants.
//
// The signal is a SUM over the candidate's tags of the reader's weight for
// that tag, normalised by the candidate's tag count. It is deliberately
// simple: a cosine between a taste vector and a work's tag set would need
// every tag's embedding, and the arena's weights are learned over TAG IDS, not
// over a shared embedding space. The sum is the honest dot product of the two
// vectors that actually exist.
//
// The evidence count matters as much as the weight. `arena_user_tag_weights`
// stores `n` beside every weight, and a weight of 0.4 from one comparison is
// not a weight of 0.4 from forty. The score divides by sqrt(n) so a single
// strong opinion moves the ranking a little and a corroborated one moves it a
// lot -- the same damping `UpsertTagWeight` uses to write the weight in the
// first place, applied on the way back out.
type SessionTaste struct {
	// Weights maps tag id to the reader's learned weight for it.
	Weights map[int32]float64
	// Evidence maps tag id to how many comparisons produced that weight.
	Evidence map[int32]float64
}

// Name is the signal's name, and the name a tune's weight vector uses.
func (s SessionTaste) Name() string { return "session_taste" }

// Score returns the reader's taste match for a candidate.
//
// It SKIPS rather than scoring zero when the reader has no learned weights or
// the candidate carries none of them. A skip is a gap in the evidence; a zero
// is a claim that the reader actively dislikes the work, and the two are
// different statements. A skipped signal is named in meta.degraded[], which is
// what makes the absence visible instead of silent.
func (s SessionTaste) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	if len(s.Weights) == 0 || len(c.TagIDs) == 0 {
		return 0, "", rank.ErrSkip
	}

	sum := 0.0
	matched := 0
	var strongest string
	strongestW := 0.0

	for _, id := range c.TagIDs {
		w, ok := s.Weights[id]
		if !ok {
			continue
		}
		n := s.Evidence[id]
		if n <= 0 {
			n = 1
		}
		// Confidence is a MULTIPLIER on the weight, and it SATURATES.
		//
		// The first version divided by sqrt(n), which was backwards: the
		// better-evidenced weight then contributed LESS, so a single
		// comparison outranked forty. The test caught it immediately.
		//
		// Dividing out the evidence count is wrong anyway, because the
		// stored weight is ALREADY an average over the comparisons
		// (UpsertTagWeight averages on write). The weight says "how much
		// this reader likes the tag"; `n` says "how sure are we". They
		// are different questions and multiplying answers both.
		//
		// n/(n+1) saturates: one comparison gives 0.5, four give 0.8,
		// forty give 0.976. So one impulse moves the ranking a little, a
		// corroborated preference moves it a lot, and the hundredth
		// comparison barely changes the result.
		conf := n / (n + 1)
		d := w * conf
		sum += d
		matched++
		if math.Abs(d) > math.Abs(strongestW) {
			strongestW = d
			strongest = fmt.Sprintf("tag %d", id)
		}
	}

	if matched == 0 {
		// The reader has preferences, and this candidate shares none of
		// their tags. That is not a zero score -- it is no evidence, and
		// scoring it zero would drag the candidate down for a reason the
		// reader never gave.
		return 0, "", rank.ErrSkip
	}

	// The score is the summed damped weight, NOT a ratio.
	//
	// The first version divided by the summed damping, and that was
	// wrong in a way the tests caught immediately: with a uniform weight,
	// the ratio is always exactly 1.0, so a candidate backed by one
	// comparison and a candidate backed by forty scored the SAME, and
	// matching three liked tags scored the same as matching one. Dividing
	// out the evidence count destroys the two things the count exists to
	// express.
	//
	// So the score is a plain sum of damped weights, which means:
	//
	//   - more matched liked tags -> higher score (the sum grows)
	//   - better-evidenced weights -> higher score (each term is bigger)
	//
	// It is then clamped to [-1, 1] so it lives in the same range as every
	// other signal and a tune weight of 0.14 means 0.14 of a unit, not
	// 0.14 of an unbounded sum.
	score := sum
	if score > 1 {
		score = 1
	}
	if score < -1 {
		score = -1
	}
	// A negative taste match is information too: the reader dislikes this
	// tag. It must not be clamped away, or a disliked candidate would look
	// neutral instead of actively wrong.
	reason := fmt.Sprintf("%d of %d tags match this reader's taste; strongest %s (%+.3f)",
		matched, len(c.TagIDs), strongest, strongestW)
	return score, reason, nil
}

// SessionTasteFrom converts the store's learned weights into a signal.
//
// It takes the store's shape (`[]store.TagWeight`-like triples) without
// importing the store, so the signal package stays free of the persistence
// layer -- the layering rule in SPEC §5. A signal reads a rank.Store, and
// this constructor is how the engine hands it what it read.
func SessionTasteFrom(weights map[int32]float64, evidence map[int32]float64) SessionTaste {
	if weights == nil {
		return SessionTaste{}
	}
	if evidence == nil {
		evidence = map[int32]float64{}
	}
	return SessionTaste{Weights: weights, Evidence: evidence}
}

// Ensure the signal satisfies the interface. A compile-time assertion is
// cheaper than discovering at runtime that a new signal does not implement
// the method the ranker calls.
var _ rank.Signal = SessionTaste{}
