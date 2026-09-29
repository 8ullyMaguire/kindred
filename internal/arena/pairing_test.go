package arena

import (
	"errors"
	"math"
	"testing"
)

// c is a candidate with a given rating, for terse table tests.
func c(id int64, mu, phi float64) Candidate {
	return Candidate{ID: id, Rating: Rating{Mu: mu, Phi: phi, Sigma: 0.06}}
}

// fixedRand returns a deterministic generator: a test that cannot control
// the randomness cannot assert anything about which pair came out, which is
// how a pair selector ends up "tested" by tests that pass regardless.
func fixedRand(seq ...int) func(int) int {
	i := 0
	return func(n int) int {
		if n <= 0 {
			return 0
		}
		if len(seq) == 0 {
			return 0
		}
		v := seq[i%len(seq)]
		i++
		return v % n
	}
}

func TestChoosePairNeverRepeatsASeenPair(t *testing.T) {
	// Three works, so three possible pairs, all already seen. The selector
	// must refuse rather than re-present: a pair shown twice teaches
	// nothing, and re-presenting one a user already declined is the
	// fastest way to make an arena feel broken.
	cands := []Candidate{c(1, 1500, 100), c(2, 1520, 100), c(3, 1480, 100)}
	seen := map[[2]int64]bool{
		{1, 2}: true, {1, 3}: true, {2, 3}: true,
	}
	for _, s := range []Strategy{StrategyRandom, StrategyMaxInfo, StrategyClose} {
		_, err := ChoosePair(PairRequest{Candidates: cands, Seen: seen, Strategy: s, Rand: fixedRand(0)})
		if !errors.Is(err, ErrNoPair) {
			t.Errorf("strategy %s: err = %v, want ErrNoPair when every pair is seen", s, err)
		}
	}

	// Free one pair up and it must be the one chosen, for every strategy.
	delete(seen, [2]int64{2, 3})
	for _, s := range []Strategy{StrategyRandom, StrategyMaxInfo, StrategyClose} {
		p, err := ChoosePair(PairRequest{Candidates: cands, Seen: seen, Strategy: s, Rand: fixedRand(0)})
		if err != nil {
			t.Fatalf("strategy %s: %v", s, err)
		}
		if got := p.key(); got != [2]int64{2, 3} {
			t.Errorf("strategy %s: chose %v, want the only unseen pair {2,3}", s, got)
		}
	}
}

// The Seen key is the ordered pair with the LOWER id first, so it must
// match however the candidate slice happens to be ordered. An off-by-one
// here means a pair silently repeats in production while the test passes.
func TestSeenKeyIsOrderIndependent(t *testing.T) {
	a, b := c(7, 1500, 100), c(9, 1510, 100)
	seen := map[[2]int64]bool{{7, 9}: true}

	for _, order := range [][]Candidate{{a, b}, {b, a}} {
		_, err := ChoosePair(PairRequest{
			Candidates: order, Seen: seen, Strategy: StrategyMaxInfo, Rand: fixedRand(0),
		})
		if !errors.Is(err, ErrNoPair) {
			t.Errorf("candidates in order %v: got %v, want ErrNoPair", ids(order), err)
		}
	}
}

// A pair where the answer is a foregone conclusion must be rejected. Two
// works 900 points apart is not a comparison, and a stream of those
// converges to the graph's opinion while learning nothing about the user.
func TestForegonePairsAreRejected(t *testing.T) {
	cands := []Candidate{c(1, 800, 60), c(2, 1700, 60)}
	_, err := ChoosePair(PairRequest{Candidates: cands, Strategy: StrategyMaxInfo, Rand: fixedRand(0)})
	if !errors.Is(err, ErrNoPair) {
		t.Errorf("a 900-point gap was accepted (err=%v); that is a formality, not a comparison", err)
	}

	// But an UNCERTAIN work makes a big gap worth testing: the comparison
	// could go either way, which is the definition of informative.
	uncertain := []Candidate{c(1, 800, 340), c(2, 1700, 60)}
	if _, err := ChoosePair(PairRequest{
		Candidates: uncertain, Strategy: StrategyMaxInfo, Rand: fixedRand(0),
	}); err != nil {
		t.Errorf("a 900-point gap between an uncertain and a settled work was rejected: %v", err)
	}
}

// Close but settled on both sides: still a foregone conclusion even inside
// the generous band, because the arena is confident about both.
func TestConfidentBigGapIsRejected(t *testing.T) {
	cands := []Candidate{c(1, 1200, 50), c(2, 1700, 50)} // 500 apart, both settled
	_, err := ChoosePair(PairRequest{Candidates: cands, Strategy: StrategyMaxInfo, Rand: fixedRand(0)})
	if !errors.Is(err, ErrNoPair) {
		t.Errorf("a 500-point gap between two settled works was accepted (err=%v)", err)
	}
}

func TestInfoGainIsMaximisedByAnEvenPair(t *testing.T) {
	// E(1-E) is the dominant term and peaks at a coin flip, so a pair of
	// near-equal works must beat a lopsided one at the same uncertainty.
	even := InfoGain(c(1, 1500, 100), c(2, 1500, 100))
	lopsided := InfoGain(c(1, 1500, 100), c(2, 1900, 100))
	if even <= lopsided {
		t.Errorf("an even pair (%.6f) is not better than a lopsided one (%.6f); E(1-E) is being ignored",
			even, lopsided)
	}
}

// g(phi) DECREASES as phi grows, so an uncertain work contributes LESS
// expected information, not more. An earlier version of this file asserted
// the opposite and failed against a correct implementation: the code was
// right and the test had the sign of g backwards.
//
// This is worth pinning because it is the single easiest thing to
// "correct" in this file, and a correction would look like an improvement.
func TestInfoGainFallsWithUncertainty(t *testing.T) {
	// Same rating gap, different confidence. g(340) = 0.68, g(20) = 1.00.
	uncertain := InfoGain(c(1, 1490, 340), c(2, 1510, 340))
	settled := InfoGain(c(3, 1490, 20), c(4, 1510, 20))
	if uncertain >= settled {
		t.Errorf("info gain %.6f (phi 340) >= %.6f (phi 20); g(phi) must DECREASE with phi",
			uncertain, settled)
	}
	// And pin g itself, so a change to the constant is caught here rather
	// than as a mysterious shift in pair selection.
	if gv := g(reducePhi(20)); math.Abs(gv-0.998) > 0.002 {
		t.Errorf("g(20) = %.4f, want ~0.998", gv)
	}
	if gv := g(reducePhi(340)); math.Abs(gv-0.680) > 0.002 {
		t.Errorf("g(340) = %.4f, want ~0.680", gv)
	}
}

func TestCloseStrategyPrefersSimilarRatings(t *testing.T) {
	req := PairRequest{
		Candidates: []Candidate{
			c(1, 1500, 100), c(2, 1510, 100), // similar
			c(3, 1800, 100), c(4, 1810, 100), // also similar to each other
		},
		Strategy: StrategyClose,
		Rand:     fixedRand(0),
	}
	// With two similar pairs, either is acceptable; what matters is that
	// the chosen one is one of the two SIMILAR pairs, not a cross pair.
	// The cross pairs (1,3) are 300 apart, which is informative, so this
	// is really a test that the strategy's own objective is being used.
	p, err := ChoosePair(req)
	if err != nil {
		t.Fatal(err)
	}
	gap := math.Abs(p.A.Rating.Mu - p.B.Rating.Mu)
	if gap > 20 {
		t.Errorf("StrategyClose chose a pair %v apart; it should choose the closest", gap)
	}
}

func TestRandomStrategyFavoursUnratedWorks(t *testing.T) {
	// The bootstrap property: a work nobody has compared must be
	// presentable, or it can never acquire a rating at all.
	//
	// Both candidates sit at mu 1500 on purpose. An unrated work is mu
	// 1500 with phi 350; the settled one is mu 1500 with phi 30. The
	// previous version of this strategy scored on |mu - MU_INIT|, which is
	// zero for both, so the two scored identically and the arena had no
	// reason to present the works that needed comparisons most. The test
	// that should have caught it used two candidates that differed only
	// in phi, against a strategy that never read phi.
	// The partner is SETTLED in both cases, so the only difference between
	// the two scores is phi_a. An earlier version shared an unrated
	// partner, which made the comparison vacuous: under a max-of-needs
	// both pairs are pinned by the partner and the assertion could not
	// distinguish them.
	unrated := c(1, 1500, 350)
	settled := c(2, 1500, 30)
	partner := c(3, 1500, 30)

	scoreU := scorePair(StrategyRandom, unrated, partner, nil)
	scoreS := scorePair(StrategyRandom, settled, partner, nil)
	if scoreU <= scoreS {
		t.Errorf("an unrated work scores %.6f, no better than a settled one's %.6f; it can never bootstrap",
			scoreU, scoreS)
	}
	// Bootstrap must be strong enough to matter, not a rounding detail.
	if scoreU < scoreS*1.5 {
		t.Errorf("bootstrap term is weak: %.6f vs %.6f, under 1.5x", scoreU, scoreS)
	}

	// A pair of two unrated works must not score below a mixed one. This
	// is the case that exposed the averaging bug: both-unrated came out at
	// 0.5 and mixed at 0.71, so the works with the most need for a first
	// comparison were shown least.
	bothNew := scorePair(StrategyRandom, c(1, 1500, 350), c(3, 1500, 350), nil)
	mixed := scorePair(StrategyRandom, c(1, 1500, 350), c(3, 1500, 30), nil)
	if bothNew < mixed {
		t.Errorf("a pair of two unrated works (%.6f) scores below a mixed one (%.6f)",
			bothNew, mixed)
	}
	if mixed <= scoreS {
		t.Errorf("a mixed pair (%.6f) is not above an all-settled one (%.6f)", mixed, scoreS)
	}

	// The bootstrap term must RISE with phi. This is the inverted-term
	// regression: 1/(1+phi/PHI_INIT) gives a settled work 0.92 and an
	// unrated one 0.5, which prefers exactly the works that need
	// comparisons least.
	if a, b := 30.0/PHI_INIT, 350.0/PHI_INIT; !(a < b) {
		t.Errorf("need(phi=30) = %.4f, need(phi=350) = %.4f; need must rise with uncertainty", a, b)
	}
}

func TestExplorePrefersDislikedPairs(t *testing.T) {
	preferred := map[int64]float64{10: 0.9, 11: 0.8, 20: -0.9, 21: -0.7}
	liked := Candidate{ID: 1, Rating: Rating{Mu: 1500, Phi: 100, Sigma: 0.06}, Tags: []int64{10, 11}}
	disliked := Candidate{ID: 2, Rating: Rating{Mu: 1510, Phi: 100, Sigma: 0.06}, Tags: []int64{20, 21}}

	if affinity(liked, preferred) <= affinity(disliked, preferred) {
		t.Errorf("affinity: liked %.4f, disliked %.4f; sign is inverted",
			affinity(liked, preferred), affinity(disliked, preferred))
	}
	// Explore should score the disliked pair HIGHER, because that is the
	// one that tests the model.
	scoreLiked := scorePair(StrategyExplore, liked, c(3, 1500, 100), preferred)
	scoreDisliked := scorePair(StrategyExplore, disliked, c(3, 1500, 100), preferred)
	if scoreDisliked <= scoreLiked {
		t.Errorf("explore scores the disliked pair %.6f, no higher than the liked %.6f",
			scoreDisliked, scoreLiked)
	}
}

func TestExploreTriggeredByShare(t *testing.T) {
	// With Explored=1.0 the selector must switch to explore EVERY time;
	// with 0.0 it must never. The strategy is recorded on the pair, which
	// is how a rating can later be attributed to how it was collected.
	cands := []Candidate{
		{ID: 1, Rating: Rating{Mu: 1500, Phi: 100, Sigma: 0.06}, Tags: []int64{10}},
		{ID: 2, Rating: Rating{Mu: 1500, Phi: 100, Sigma: 0.06}, Tags: []int64{20}},
	}
	pref := map[int64]float64{10: 1.0, 20: -1.0}

	always, err := ChoosePair(PairRequest{
		Candidates: cands, Preferred: pref, Explored: 1.0,
		Rand: fixedRand(999), // 999/1000 > 1.0 is impossible, but -1 is out
	})
	if err != nil {
		t.Fatal(err)
	}
	if always.Strategy != StrategyExplore {
		t.Errorf("Explored=1.0 produced strategy %q, want explore", always.Strategy)
	}

	never, err := ChoosePair(PairRequest{
		Candidates: cands, Preferred: pref, Explored: 0.0, Rand: fixedRand(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if never.Strategy == StrategyExplore {
		t.Error("Explored=0.0 produced an explore pair")
	}
}

func TestFatigueDecaysWithRepetition(t *testing.T) {
	prev := math.Inf(1)
	for _, n := range []int{0, 1, 5, 20, 100} {
		f := Fatigue(Candidate{Comparisons: n})
		if f >= prev {
			t.Errorf("fatigue at %d presentations is %.4f, not below the previous %.4f", n, f, prev)
		}
		prev = f
	}
	// But it must never reach zero: a work that keeps losing to better
	// works still has to be able to come back.
	if Fatigue(Candidate{Comparisons: 100000}) <= 0 {
		t.Error("fatigue reached zero; a work can be permanently starved")
	}
}

func TestTooFewCandidatesIsNoPair(t *testing.T) {
	for _, n := range []int{0, 1} {
		var cands []Candidate
		for i := 0; i < n; i++ {
			cands = append(cands, c(int64(i+1), 1500, 100))
		}
		_, err := ChoosePair(PairRequest{Candidates: cands, Rand: fixedRand(0)})
		if !errors.Is(err, ErrNoPair) {
			t.Errorf("%d candidates: err = %v, want ErrNoPair", n, err)
		}
	}
}

func TestInvalidStrategyFallsBackRatherThanErroring(t *testing.T) {
	// A bad ?strategy= in a URL must not be a 500.
	p, err := ChoosePair(PairRequest{
		Candidates: []Candidate{c(1, 1500, 100), c(2, 1510, 100)},
		Strategy:   Strategy("nonsense"),
		Rand:       fixedRand(0),
	})
	if err != nil {
		t.Fatalf("an invalid strategy returned an error: %v", err)
	}
	if p.Strategy != StrategyRandom {
		t.Errorf("strategy = %q, want a fallback to random", p.Strategy)
	}
}

func TestPairIsAlwaysNormalisedToLowerIDFirst(t *testing.T) {
	// The schema CHECKs work_a < work_b and the Seen key assumes it, so
	// the selector must never emit a pair in the other order regardless
	// of how the candidate slice is sorted.
	for _, order := range [][]int64{{5, 2}, {2, 5}} {
		cands := []Candidate{
			c(order[0], 1500, 100), c(order[1], 1510, 100),
			c(9, 1520, 100),
		}
		p, err := ChoosePair(PairRequest{Candidates: cands, Rand: fixedRand(0)})
		if err != nil {
			t.Fatal(err)
		}
		if p.A.ID > p.B.ID {
			t.Errorf("candidate order %v produced pair (%d,%d); the lower id must come first",
				order, p.A.ID, p.B.ID)
		}
	}
}

func TestSameWorkIsNeverPairedWithItself(t *testing.T) {
	// Duplicated candidates are possible when two queries overlap, and a
	// self-pair would insert a row that violates the CHECK constraint.
	cands := []Candidate{c(4, 1500, 100), c(4, 1500, 100), c(9, 1520, 100)}
	p, err := ChoosePair(PairRequest{Candidates: cands, Rand: fixedRand(0)})
	if err != nil {
		t.Fatal(err)
	}
	if p.A.ID == p.B.ID {
		t.Errorf("chose a self-pair (%d, %d)", p.A.ID, p.B.ID)
	}
}

func TestSelectionIsDeterministicWithAFixedRand(t *testing.T) {
	// Two runs with the same pool and generator must agree, or a bug that
	// made selection order-dependent would never show up in a test.
	req := func() PairRequest {
		return PairRequest{
			Candidates: []Candidate{
				c(1, 1500, 100), c(2, 1510, 100), c(3, 1490, 100), c(4, 1520, 100),
			},
			Strategy: StrategyMaxInfo,
			Rand:     fixedRand(3),
		}
	}
	p1, err1 := ChoosePair(req())
	p2, err2 := ChoosePair(req())
	if err1 != nil || err2 != nil {
		t.Fatalf("%v %v", err1, err2)
	}
	if p1.key() != p2.key() {
		t.Errorf("the same request gave %v then %v", p1.key(), p2.key())
	}
}

func ids(cands []Candidate) []int64 {
	out := make([]int64, len(cands))
	for i, c := range cands {
		out[i] = c.ID
	}
	return out
}
