// Pair selection: choosing which two works to show.
//
// This is the part of the arena that decides whether the feature feels
// intelligent or random, and it is worth being explicit about the
// tension. A pair that is too easy teaches nothing: the user already knew
// they would prefer the more-kudosed work, and the comparison confirms
// what the graph already says. A pair that is too hard is a coin flip, and
// a coin flip is a noisy rating. The interesting pairs are the ones where
// the arena is genuinely uncertain -- where phi is high enough to admit
// either answer, and the two works are different enough that the choice
// carries information about WHICH axis the user cares about.

package arena

import (
	"errors"
	"math"
	"sort"
)

// Strategy names a pair-selection policy. They are recorded on every
// comparison so a rating change can be attributed to how the pair was
// chosen: a work that only ever appears in hard pairs has a rating built
// from a different evidence distribution than one that appears in easy
// ones, and a leaderboard that ignored that would be comparing
// incomparable things.
type Strategy string

const (
	// StrategyRandom is the cold-start and tie-breaker policy: two works
	// drawn from the eligible pool, weighted so that under-rated works
	// appear more often. Without it a work can never earn its first
	// comparison, because nothing would ever present it.
	StrategyRandom Strategy = "random"
	// StrategyMaxInfo picks the pair whose expected information gain is
	// highest, which is g(phi_a)g(phi_b)E(1-E) -- high when both works
	// are uncertain, and the comparison is genuinely 50/50.
	StrategyMaxInfo Strategy = "maxinfo"
	// StrategyClose presents two works of nearly equal rating, which is
	// where a single comparison is worth the most points. This is what
	// sharpens the top of the leaderboard, and it is also the strategy
	// that most needs the no-repeat constraint: two similar works will
	// keep coming back.
	StrategyClose Strategy = "close"
	// StrategyExplore deliberately presents a pairing the user's learned
	// tag weights say they will DISLIKE, to find out whether the model is
	// wrong. Without an explicit explore phase, a recommender can only
	// ever confirm itself: the tags it already believes get more
	// evidence, and the tags it has never seen never get any.
	StrategyExplore Strategy = "explore"
)

// Valid reports whether s is a known strategy, for validating query
// parameters. An unrecognised strategy falls back to StrategyRandom
// rather than erroring, because a bad ?strategy= in a URL should not be a
// 500.
func (s Strategy) Valid() bool {
	switch s {
	case StrategyRandom, StrategyMaxInfo, StrategyClose, StrategyExplore:
		return true
	}
	return false
}

// Candidate is a work the pair selector may choose from.
type Candidate struct {
	ID     int64
	Rating Rating
	// Score is the recommender's or graph's opinion of the work, 0..1.
	// Used only by the explore strategy, and only as a tie-breaker.
	Score float64
	// Tags is the work's tag set, for the user's learned preferences.
	Tags []int64
	// Comparisons is how often this work has been shown. A work shown
	// many times and never chosen is a work the arena is wasting slots
	// on, and the fatigue term is what stops that.
	Comparisons int
}

// InfoGain is the expected information in comparing two works, as
// g(phi_a) g(phi_b) E(1-E) -- the same weighting Glickman's step 3 uses
// for opponents.
//
// Read the two factors carefully, because they pull in opposite
// directions and getting the reading wrong is how this function gets
// "fixed" into its own inverse:
//
//   - E(1-E) is maximal at 0.5, so the pair is most informative when the
//     two works are at nearly EQUAL rating. This is the dominant term and
//     it is the one that makes the arena feel sharp: two works of similar
//     strength are the ones a comparison actually resolves.
//
//   - g(phi) DECREASES as phi grows (g(340) = 0.68, g(20) = 1.00), so an
//     UNCERTAIN work contributes LESS, not more. That is the right
//     direction for a rating system: a comparison against a work whose
//     rating is barely established tells you less about which of the two
//     is genuinely better, because the winner may have got lucky.
//
// An earlier version of this file's comment claimed the opposite -- that
// wide intervals score high -- and a test written from that comment
// demanded it. The test was wrong and the code was right, which is the
// argument for reading the function before writing its contract.
func InfoGain(a, b Candidate) float64 {
	e := Expected(a.Rating.Mu, b.Rating.Mu, b.Rating.Phi)
	gA, gB := g(reducePhi(a.Rating.Phi)), g(reducePhi(b.Rating.Phi))
	return gA * gB * e * (1 - e)
}

// Fatigue is the penalty for a work that has been shown often and judged
// rarely.
//
// The first few presentations of a work are what tell you whether it
// belongs; the fiftieth tells you nothing new. The term is deliberately
// gentle (a power, not a hard exclusion) so a genuinely informative work
// can still come back, because the alternative -- hard-excluding a work
// after N presentations -- silently removes it from the arena forever
// without anyone noticing.
func Fatigue(c Candidate) float64 {
	return 1.0 / (1.0 + math.Log1p(float64(c.Comparisons)))
}

// PairRequest is everything pair selection needs. It is a plain struct
// rather than a pile of arguments because this is called from three
// places (the web handler, the API, and the tests) and a positional
// argument list here would be a bug waiting to happen.
type PairRequest struct {
	// Candidates is the eligible pool. The caller has already filtered it
	// (rating floor, tag match, corpus membership); the selector only
	// chooses within it.
	Candidates []Candidate
	// Seen is the set of pairs this owner has already been shown, keyed
	// on the ordered id pair with the LOWER id first. A nil map means
	// "no history", which is not the same as "no repeats allowed" -- an
	// empty pool must be allowed to return ErrNoPair rather than
	// inventing a duplicate.
	Seen map[[2]int64]bool
	// Preferred is the user's learned tag weights, for the explore
	// strategy. Nil means no learned preferences yet, which makes explore
	// identical to random.
	Preferred map[int64]float64
	// Strategy is the requested policy; an invalid value becomes
	// StrategyRandom.
	Strategy Strategy
	// Explored is the share of presentations that should be exploratory,
	// 0..1. Zero disables exploration entirely.
	Explored float64
	// Rand is the source of randomness. Nil means a deterministic
	// fallback, which is what makes the selector testable without a
	// seed: a test that cannot control the randomness cannot assert
	// anything about which pair came out.
	Rand func(n int) int
}

// Pair is a chosen comparison.
type Pair struct {
	A, B     Candidate
	Strategy Strategy
	// Score is the strategy's own objective value for this pair, kept for
	// logging and for the "why am I seeing this" explanation.
	Score float64
}

// normalised returns a pair with the lower id first, so it matches the
// Seen key and the schema's CHECK (work_a < work_b).
func normalised(a, b Candidate) (Candidate, Candidate) {
	if b.ID < a.ID {
		return b, a
	}
	return a, b
}

func (p Pair) key() [2]int64 {
	a, b := p.A.ID, p.B.ID
	if b < a {
		a, b = b, a
	}
	return [2]int64{a, b}
}

// ChoosePair selects a pair from the request.
//
// The selection is a three-stage filter, and the ORDER is the design:
// cheap rejections first, expensive scoring only on what survives.
//
//  1. Reject any pair already in Seen. A pair shown twice teaches nothing
//     and is the single most reliable way to make the arena feel broken.
//     This is why presentation is recorded before judgement.
//  2. Reject any pair with no information in it: two works at wildly
//     different ratings, where the answer is a foregone conclusion.
//  3. Score what remains by the requested strategy, and take the best.
//
// Step 2 is what stops the arena being boring. Two works rated 900 and
// 2100 are not a comparison, they are a formality, and a rating system
// fed a stream of them converges to the graph's opinion and learns
// nothing about the people using it.
func ChoosePair(req PairRequest) (Pair, error) {
	if len(req.Candidates) < 2 {
		return Pair{}, ErrNoPair
	}
	if !req.Strategy.Valid() {
		req.Strategy = StrategyRandom
	}
	if req.Rand == nil {
		req.Rand = func(n int) int { return 0 }
	}
	if req.Preferred == nil {
		req.Preferred = map[int64]float64{}
	}

	// Deliberate exploration: with probability Explored, present a pair
	// the user's weights say they will dislike. Computed ONCE per request
	// rather than per pair, so the decision is "this presentation is an
	// exploration" rather than "this pair happens to look unfamiliar".
	explore := req.Explored > 0 && float64(req.Rand(1000))/1000.0 < req.Explored
	if explore {
		req.Strategy = StrategyExplore
	}

	best := Pair{Strategy: req.Strategy, Score: math.Inf(-1)}
	anyUsable := false
	// A bounded scan rather than all-pairs: the pool is the eligible set
	// from a recommender, and O(n^2) over 4000 candidates is 8M pair
	// evaluations per presentation. Instead, take the top M by a cheap
	// proxy and evaluate those exhaustively. M is small because the
	// scores are informative enough that a good pair is near the top.
	const pool = 40
	cands := topCandidates(req, pool)

	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			a, b := normalised(cands[i], cands[j])
			if a.ID == b.ID {
				continue
			}
			p := Pair{A: a, B: b, Strategy: req.Strategy}
			// 1. never repeat
			if req.Seen != nil && req.Seen[p.key()] {
				continue
			}
			// 2. must carry information
			if !informative(a, b) {
				continue
			}
			anyUsable = true
			// 3. score by strategy
			p.Score = scorePair(req.Strategy, a, b, req.Preferred)
			if p.Score > best.Score {
				best = p
			}
		}
	}
	if !anyUsable || best.Score == math.Inf(-1) {
		return Pair{}, ErrNoPair
	}
	return best, nil
}

// informative rejects a pair whose outcome is a foregone conclusion.
//
// The test is about CONFIDENCE, not about distance. A 500-point gap
// between two works the arena knows well is a formality: the user is
// being asked to confirm what the ratings already say. The same gap
// between a work at RD 340 and one at RD 60 is a real question, because
// the first work's rating is a guess and the comparison could go either
// way.
//
// The order of the two clauses is the whole point. An earlier version
// rejected any gap over 600 FIRST, which made the uncertainty clause
// unreachable: a 900-point gap involving an unrated work was discarded
// before its uncertainty could rescue it, and a fresh work could never
// meet a strong one. The confidence test has to come first, or the clause
// that follows it is dead.
func informative(a, b Candidate) bool {
	gap := math.Abs(a.Rating.Mu - b.Rating.Mu)
	// Both sides well measured: a wide gap really is a foregone
	// conclusion, so cut at 300. A narrow band would starve the arena --
	// only a handful of works are ever within 100 points of each other.
	settled := a.Rating.Phi < 80 && b.Rating.Phi < 80
	if settled && gap > 300 {
		return false
	}
	// At least one side unknown: allow a much wider gap, but not an
	// absurd one. Comparing a brand-new work against the strongest thing
	// in the corpus is not a question, it is a rout.
	if gap > 1000 {
		return false
	}
	return true
}

func scorePair(s Strategy, a, b Candidate, preferred map[int64]float64) float64 {
	fatigue := Fatigue(a) * Fatigue(b)
	switch s {
	case StrategyMaxInfo:
		return InfoGain(a, b) * fatigue
	case StrategyClose:
		// Close in rating, and the fatigue term keeps a work that keeps
		// losing from being re-presented forever.
		return (1.0 - math.Abs(a.Rating.Mu-b.Rating.Mu)/600.0) * fatigue
	case StrategyExplore:
		// Reward DISLIKED pairs. affinity is positive for tags the user
		// likes, so a pair whose affinity is negative is the one worth
		// testing.
		aff := affinity(a, preferred) + affinity(b, preferred)
		return (-aff + 1.0) * fatigue
	default: // StrategyRandom
		// Not uniform, and the term is UNCERTAINTY rather than distance
		// from the mean. Bootstrap is the reason: an unrated work sits at
		// mu 1500 with phi 350, so a strategy keyed off |mu - 1500| scored
		// it identically to a settled work at mu 1500 with phi 30 -- and
		// the works that most need their first comparison were the ones
		// the arena had no reason to show.
		//
		// The bootstrap need is read off the LESS certain of the two
		// works, and it is a need, so it RISES with phi.
		//
		// Getting that direction wrong is easy and it is catastrophic in a
		// quiet way: a term of 1/(1 + phi/PHI_INIT) gives a settled work
		// (phi 30) 0.92 and an unrated one (phi 350) 0.5, so the strategy
		// preferentially presents the works that need comparisons LEAST --
		// and a work that is never presented never gets a rating, so the
		// arena would converge to rating a small settled core and never
		// discover anything new. It looks entirely reasonable in a diff.
		//
		// Max, not mean or min-of-needs: a pair should surface if EITHER
		// side needs it. Averaging the two lets a pair of two unrated
		// works (0.5) score BELOW a mixed pair (0.71), so the works with
		// the most need for a first comparison are shown least.
		boot := math.Max(
			a.Rating.Phi/(PHI_INIT+a.Rating.Phi),
			b.Rating.Phi/(PHI_INIT+b.Rating.Phi))
		// A mild prior against extremes, whose ratings are both far from
		// the centre and rarely informative. Keyed on the pair's own
		// distance, so it cannot outvote the bootstrap term.
		centre := 1.0 - 0.3*math.Min(1.0, math.Max(
			math.Abs(a.Rating.Mu-MU_INIT), math.Abs(b.Rating.Mu-MU_INIT))/800.0)
		return boot * centre * fatigue
	}
}

// affinity is how well a work matches the user's learned tag preferences,
// normalised by the number of shared tags so a work with 30 tags cannot
// outscore a work with 4 on volume alone.
func affinity(c Candidate, preferred map[int64]float64) float64 {
	if len(preferred) == 0 || len(c.Tags) == 0 {
		return 0
	}
	sum, n := 0.0, 0
	for _, t := range c.Tags {
		if w, ok := preferred[t]; ok {
			sum += w
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / math.Sqrt(float64(len(c.Tags)))
}

// topCandidates reduces the pool to the most promising M by a cheap
// proxy, so the pair scan is bounded.
//
// The proxy is deliberately strategy-independent: it ranks by how close a
// work is to the middle of its rating distribution and how little it has
// been shown, which is a reasonable prior under every strategy. A
// strategy-specific proxy would be better and would also be a place for
// the pool to be silently wrong for three of the four strategies.
func topCandidates(req PairRequest, m int) []Candidate {
	cands := make([]Candidate, len(req.Candidates))
	copy(cands, req.Candidates)
	sort.SliceStable(cands, func(i, j int) bool {
		return proxyScore(cands[i]) > proxyScore(cands[j])
	})
	if len(cands) > m {
		cands = cands[:m]
	}
	return cands
}

func proxyScore(c Candidate) float64 {
	f := Fatigue(c)
	// Prefer works near the centre of the distribution: a work at 2400
	// will be paired against something far away and rejected as
	// uninformative anyway, so ranking it highly wastes a slot.
	centre := 1.0 - math.Min(1.0, math.Abs(c.Rating.Mu-MU_INIT)/800.0)
	return centre*f + 0.001*c.Score
}

// ErrNoPair is returned when the pool cannot yield an informative,
// unrepeated pair. It is a normal outcome -- a new user, or a user who has
// exhausted their pool -- and the handler renders it as an invitation to
// try a different filter, not as a 500.
var ErrNoPair = errors.New("no pair available")
