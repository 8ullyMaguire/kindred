// Package stats holds the small amount of statistics this project is willing to
// do, written out rather than pulled in as a dependency.
//
// It exists because the interleave standings printed a RATE and called it a
// result. A 1.000 rate over one slot and a 0.31 rate over four hundred slots
// rendered identically, and the page's own words ("the difference is A minus B")
// invited a reader to treat a difference of 0.0004 as an ordering. A ranking that
// cannot say how sure it is has not finished.
//
// Nothing here needs a normal approximation: the counts are small, exact tests are
// a few lines, and an approximation would be the wrong tool at exactly the sample
// sizes this deployment has.
package stats

import "math"

// FisherExact returns the two-sided Fisher exact p-value for a 2x2 table.
//
//	          read    not read
//	arm A      a       b
//	arm B      c       d
//
// The two-sided p is the sum of the probabilities of every table at least as
// extreme as the observed one, where "at least as extreme" means a probability no
// greater than the observed table's (the standard definition, and the one R's
// fisher.test uses). It is exact under the null that the two arms are equally
// likely to be read given the totals, which is the hypothesis the experiment is
// actually asking about — not "the two rates are equal", which is a different and
// weaker claim.
//
// It returns 0 for a table with no data (every cell in one row or one column), and
// 1 for a table whose marginals allow only one arrangement: with nothing to vary,
// there is no evidence of a difference, and "p = 1" says that rather than "p = 0",
// which would be the strongest possible claim.
func FisherExact(a, b, c, d int) float64 {
	if a < 0 || b < 0 || c < 0 || d < 0 {
		return 1
	}
	n1 := a + b
	n2 := c + d
	m1 := a + c
	n := n1 + n2
	if n == 0 || n1 == 0 || n2 == 0 || m1 == 0 || m1 == n {
		return 1
	}

	observed := hypergeom(a, n1, m1, n)
	if observed == 0 {
		return 1
	}
	// Every table with the same marginals, walked by the count in the top-left
	// cell. "At least as extreme" is p <= observed, which is what makes this the
	// two-sided version rather than a one-sided one.
	const eps = 1e-12
	lo := maxInt(0, m1-n2)
	hi := minInt(m1, n1)
	p := 0.0
	for x := lo; x <= hi; x++ {
		px := hypergeom(x, n1, m1, n)
		if px <= observed*(1+eps) {
			p += px
		}
	}
	if p > 1 {
		p = 1
	}
	return p
}

// hypergeom is the probability of exactly x successes in n1 draws from a
// population of n containing m1 successes, computed in log space so the factorials
// cannot overflow: with a few thousand slots the direct form overflows a float64.
func hypergeom(x, n1, m1, n int) float64 {
	return math.Exp(logChoose(m1, x) + logChoose(n-m1, n1-x) - logChoose(n, n1))
}

// logChoose is log(n choose k), or -Inf when the choice is impossible.
func logChoose(n, k int) float64 {
	if k < 0 || k > n || n < 0 {
		return math.Inf(-1)
	}
	// logΓ(n+1) - logΓ(k+1) - logΓ(n-k+1); Lgamma is stdlib and exact enough.
	ln, _ := math.Lgamma(float64(n + 1))
	lk, _ := math.Lgamma(float64(k + 1))
	lm, _ := math.Lgamma(float64(n - k + 1))
	return ln - lk - lm
}

// MinImpressions is how many slots an arm needs before the standings may call a
// difference at all, independent of p.
//
// The reason is not conservatism for its own sake: with ten slots an arm at 3/10
// against 0/10 gives p ≈ 0.21, so the test already refuses. The floor exists for
// the other failure, where a large p-value difference over a handful of slots
// looks decisive because the numbers are small and clean — 1/1 against 0/1 is
// p = 1.0, but 2/2 against 0/2 is p = 0.33 and reads as "perfect" in both columns.
const MinImpressions = 50

// Verdict is what the standings are allowed to say.
type Verdict struct {
	// P is the two-sided Fisher exact p-value for this pair.
	P float64 `json:"p"`
	// Leading names the arm ahead, or "" when the test does not support a claim.
	Leading string `json:"leading,omitempty"`
	// Reasons says why no lead was declared, in the reader's words. Empty when
	// one was.
	Reason string `json:"reason,omitempty"`
	// Underpowered is true when the floor, not the test, is what stopped it.
	Underpowered bool `json:"underpowered,omitempty"`
}

// Alpha is the threshold for calling a lead. 0.05, stated rather than assumed,
// because a page that prints a p-value without saying what would count as evidence
// leaves the reader to invent a standard.
const Alpha = 0.05

// Compare decides whether the difference between two arms is evidence.
//
// The order of the two refusals matters and is the whole point: the power floor
// comes FIRST, because with few slots a significant-looking p from a 3-cell table
// is a coincidence generator, and reporting it as "significant but underpowered"
// would still be printing a winner.
func Compare(aArm string, aRead, aSlots int, bArm string, bRead, bSlots int) Verdict {
	if aSlots < MinImpressions || bSlots < MinImpressions {
		return Verdict{
			P:            1,
			Underpowered: true,
			Reason: "too few slots to compare: " + plural(aSlots) + " for " + aArm +
				" and " + plural(bSlots) + " for " + bArm + ", and " +
				itoa(MinImpressions) + " each are needed. Read marks on this list are " +
				"the evidence; until there are enough of them a difference is a coin " +
				"landing, whichever way it landed.",
		}
	}
	p := FisherExact(aRead, aSlots-aRead, bRead, bSlots-bRead)
	if p > Alpha {
		return Verdict{
			P: p,
			Reason: "no evidence of a difference: p = " + f3(p) + ", and anything above " +
				f2(Alpha) + " is consistent with the two arms being equally likely to be read.",
		}
	}
	// The lead follows the RATE, cross-multiplied so the comparison is exact:
	// bRead/bSlots ? aRead/aSlots  <=>  bRead*aSlots ? aRead*bSlots.
	leading := aArm
	if bRead*aSlots > aRead*bSlots {
		leading = bArm
	}
	return Verdict{P: p, Leading: leading}
}

// SignTest returns the two-sided exact p-value for wins against losses under the
// null that each decisive comparison is a coin: P(X <= min(w,l)) * 2, capped at 1.
//
// Ties are EXCLUDED, and that is the whole definition rather than a convenience: a
// reader who could not choose has said nothing about which algorithm is better, and
// counting a tie as half a win would let a pair of identical lists drift toward
// significance by being compared many times.
func SignTest(wins, losses int) float64 {
	n := wins + losses
	if n == 0 {
		return 1
	}
	k := wins
	if losses < wins {
		k = losses
	}
	// Sum C(n,i)/2^n for i <= k, doubled. In log space for the same reason as
	// the hypergeometric: 2^n overflows long before n reaches a thousand.
	tail := 0.0
	for i := 0; i <= k; i++ {
		tail += math.Exp(logChoose(n, i) - float64(n)*math.Ln2)
	}
	p := 2 * tail
	if p > 1 {
		p = 1
	}
	return p
}

// MinDecisive is how many WIN-OR-LOSE comparisons are needed before the vote table
// may name a leader. Ties do not count toward it for the reason above.
//
// Ten, stated rather than derived: at ten decisive comparisons a clean sweep is
// p = 0.002 and 9-1 is p = 0.021, so the test can fire; at six a sweep is only
// p = 0.031 and at five nothing can reach 0.05 at all. The floor makes the standard
// explicit instead of leaving a reader to notice that 4-0 "looks" decisive.
const MinDecisive = 10

// VoteVerdict is what the vote table is allowed to say about one algorithm.
type VoteVerdict struct {
	// P is the two-sided sign-test p-value over decisive comparisons.
	P float64 `json:"p"`
	// Leading is true when the evidence says this algorithm is the better one.
	Leading bool `json:"leading"`
	// Behind is true when the evidence is significant and goes the other way:
	// a record that is evidence this algorithm LOSES. Distinct from "no
	// evidence", which is what an empty or noisy record gives.
	Behind bool `json:"behind"`
	// Reason explains a refusal in the reader's words.
	Reason string `json:"reason,omitempty"`
	// Underpowered is true when the decisive-comparison floor is what stopped it.
	Underpowered bool `json:"underpowered,omitempty"`
}

// CompareVotes decides whether an algorithm's win/loss record is evidence.
func CompareVotes(name string, wins, losses, ties int) VoteVerdict {
	decisive := wins + losses
	if decisive < MinDecisive {
		return VoteVerdict{
			P:            1,
			Underpowered: true,
			Reason: "only " + itoa(decisive) + " comparison(s) went one way or the other " +
				"(" + itoa(ties) + " tied); " + itoa(MinDecisive) + " are needed before a " +
				"record can be more than a coin landing. Ties do not count: a reader who " +
				"could not choose has said nothing about which is better.",
		}
	}
	p := SignTest(wins, losses)
	if p > Alpha {
		return VoteVerdict{
			P: p,
			Reason: "no evidence that " + name + " is better or worse: " + itoa(wins) +
				" wins against " + itoa(losses) + " losses gives p = " + f3(p) + ".",
		}
	}
	if wins > losses {
		return VoteVerdict{P: p, Leading: true}
	}
	// Significant and BEHIND: the record is evidence, and the evidence is that
	// this algorithm loses. Saying nothing here would be the same silence in the
	// other direction.
	return VoteVerdict{
		P:      p,
		Behind: true,
		Reason: "clear evidence AGAINST " + name + ": " + itoa(losses) + " losses against " + itoa(wins) + " wins, p = " + f3(p) + ".",
	}
}

func plural(n int) string {
	if n == 1 {
		return "1 slot"
	}
	return itoa(n) + " slots"
}
func f3(v float64) string { return trimFloat(v, 3) }
func f2(v float64) string { return trimFloat(v, 2) }
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// trimFloat formats with at most `places` decimals and no trailing zeros, so a
// p-value of 0.5 prints as "0.5" rather than "0.500" and a p of 0.0004 prints as
// "0.0004" rather than "0.000" — which would read as zero evidence against.
func trimFloat(v float64, places int) string {
	if v == 0 {
		return "0"
	}
	if v < math.Pow(10, -float64(places)) {
		return "<0.001"
	}
	scale := math.Pow(10, float64(places))
	rounded := math.Round(v*scale) / scale
	s := itoa(int(rounded * scale))
	for len(s) < places+1 {
		s = "0" + s
	}
	whole := s[:len(s)-places]
	frac := s[len(s)-places:]
	for len(frac) > 0 && frac[len(frac)-1] == '0' {
		frac = frac[:len(frac)-1]
	}
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
