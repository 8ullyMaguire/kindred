// Package arena implements the pairwise comparison arena: a user is shown
// two works, picks one (or neither), and works acquire a Glicko-2 rating
// from those judgements.
//
// The rationale for Glicko-2 over Elo is in the old kindling spec and it
// holds. Elo has no cold-start advantage, no confidence measure and no
// inactivity decay, and this mirror's corpus is 59% updated within two
// years -- exactly the population RD (rating deviation) decay exists for.
// A work nobody has compared in eight months should drift back toward the
// middle, and Elo cannot say that.
package arena

import "math"

// Glicko-2, implemented against Glickman's paper (glicko.net/glicko/
// glicko2.pdf, rev. 22 March 2022) and pinned to its own worked example
// in glicko_test.go.
//
// FIVE THINGS THE PUBLISHED ALGORITHM REQUIRES
// ----------------------------------------------
// Each of these was wrong in the first draft of this file, and each was
// caught by the reference example rather than by reading the paper. They
// are the reason the test pins v, delta, phi* and the volatility bracket
// individually and not just the final answer.
//
//  1. v is NOT 1/(1 + sum g(phi_j)^2). That is the pre-2012 DRAFT formula.
//     The published step 3 weights each opponent by how DECISIVE the game
//     was: v = [sum g(phi_j)^2 E(1-E)]^-1. A comparison everyone expected
//     carries almost no information about the player's strength, and the
//     draft formula makes it count as much as an upset.
//
//  2. mu' uses phi'^2, not sigma^2/(phi^2+v). The draft form is roughly
//     20x smaller here. The published step 7 is
//     mu' = mu + phi'^2 * sum g(phi_j)(s_j - E) -- it needs only the sum
//     and the NEW phi, with delta nowhere in it.
//
//  3. The volatility f(x) cannot be shortcut. The published one is
//     f(x) = e^x (D^2 - phi^2 - v - e^x) / (2 (phi^2 + v + e^x)^2)
//            - (x - a)/tau^2
//     and it is DISCONTINUOUS where it crosses zero, which is why step 4
//     is the Illinois regula-falsi iteration with an fA/2 damping term and
//     not a bisection or a Newton step.
//
//  4. The bracket is asymmetric. A = a = ln(sigma^2) always, but B is
//     ln(delta^2 - phi^2 - v) when delta^2 > phi^2 + v, and otherwise
//     a - k*tau found by SEARCHING left in multiples of tau. A plain
//     bisection over [a-1, a+1] converges to a plausible-looking number
//     that is not sigma'.
//
//  5. There is no "ln(10)" in the paper. It writes E with exp():
//     E = 1/(1 + exp(-g(phi_j)(mu - mu_j))). That is algebraically the
//     same as the 10^(-g*gap/ln10) form, and both are correct; the old
//     kindling notes' 400 divisor is the actual error, and it belongs to
//     the pre-2001 Glicko scale. Either spelling passes the reference.
//
// SCALE, THE OTHER TRAP
// ---------------------
// mu, phi and every Opponent field are on the Glicko scale. The reduced
// Glicko-2 values exist only as locals inside Update. Getting this wrong
// does not produce a small error: feeding a raw 100-point gap into the
// exponent gives -0.9955*100/ln(10) = -43.2, so E comes out as EXACTLY
// 1.0 for every opponent, delta is 0, and the update is a silent no-op
// that looks like it ran. That is why reducePhi and the gap reduction live
// in exactly one place each.

const (
	// SCALE is 400 / ln(10), the Glicko <-> Glicko-2 conversion factor.
	SCALE = 173.7178
	// TAU constrains how fast volatility may move. The paper suggests
	// 0.3 to 1.2, and 0.2 for very improbable outcome sets.
	TAU = 0.5
	// EPS is Glickman's convergence tolerance for the volatility
	// iteration: "a sufficiently small choice", quoted as 0.000001.
	EPS = 1e-6
)

// Rating defaults, on the Glicko scale.
const (
	MU_INIT    = 1500.0
	PHI_INIT   = 350.0
	SIGMA_INIT = 0.06
)

// Score is the residual for one comparison. Draw is a DRAW, not an
// absence: the arena's third button says "neither", which is a judgement
// that neither is preferable, and discarding it would bias ratings toward
// works that only ever get compared decisively.
const (
	Win  = 1.0
	Loss = 0.0
	Draw = 0.5
)

// Opponent is one opponent in a rating period: their rating, and the
// residual this player's result against them produced. Volatility is
// deliberately absent -- the paper states the opponents' volatilities are
// not relevant in the calculations.
type Opponent struct {
	Mu    float64
	Phi   float64
	Score float64
}

// Rating is a Glicko-2 rating on the Glicko scale.
type Rating struct {
	Mu    float64 `json:"mu"`
	Phi   float64 `json:"phi"`
	Sigma float64 `json:"sigma"`
}

// Initial is the rating of a work nobody has compared.
func Initial() Rating {
	return Rating{Mu: MU_INIT, Phi: PHI_INIT, Sigma: SIGMA_INIT}
}

// g is g(phi) = 1/sqrt(1 + 3 phi^2/pi^2), phi on the Glicko-2 scale.
func g(phi float64) float64 {
	return 1.0 / math.Sqrt(1+3*phi*phi/(math.Pi*math.Pi))
}

// reducePhi converts a rating deviation from the Glicko scale to the
// Glicko-2 scale. It is the ONLY place that division happens for a phi
// fed to g() or Expected(); every caller passes raw phis.
func reducePhi(phi float64) float64 { return phi / SCALE }

func reduced(mu, phi float64) (float64, float64) {
	return (mu - MU_INIT) / SCALE, reducePhi(phi)
}

func unreduced(mu, phi float64) (float64, float64) {
	return SCALE*mu + MU_INIT, SCALE * phi
}

// Expected is E(mu, muJ, phiJ): the probability that mu beats muJ.
//
// mu, muJ and phiJ are all on the Glicko scale; the gap and the phi are
// both reduced here. Written with exp(), as the paper does, which gives
// the paper's 0.639 for the example's first opponent.
func Expected(mu, muJ, phiJ float64) float64 {
	return 1.0 / (1.0 + math.Exp(-g(reducePhi(phiJ))*(mu-muJ)/SCALE))
}

// ExpectedWrong is E() with 400 in place of ln(10) in the exponent. The
// gap reduction is still correct here, so the constant is the ONLY
// difference under test.
func ExpectedWrong(mu, muJ, phiJ float64) float64 {
	return 1.0 / (1.0 + math.Pow(10, -g(reducePhi(phiJ))*(mu-muJ)/SCALE)/400)
}

// residualSum is the evidence in the period: sum g(phi_j)(s_j - E). Both
// the variance and the rating update are functions of it (the former
// with the E(1-E) weights).
func residualSum(mu float64, ops []Opponent) float64 {
	sum := 0.0
	for _, o := range ops {
		sum += g(reducePhi(o.Phi)) * (o.Score - Expected(mu, o.Mu, o.Phi))
	}
	return sum
}

// variance is step 3, the PUBLISHED form: each opponent is weighted by
// how decisive the game was.
//
//	v = [sum g(phi_j)^2 E(1-E)]^-1
//
// The pre-2012 draft used 1/(1 + sum g(phi_j)^2), which weights an
// expected result the same as an upset, and is what the reference example
// rejects: 0.2921 against the paper's 1.7785.
func variance(mu float64, ops []Opponent) float64 {
	sum := 0.0
	for _, o := range ops {
		e := Expected(mu, o.Mu, o.Phi)
		sum += g(reducePhi(o.Phi)) * g(reducePhi(o.Phi)) * e * (1.0 - e)
	}
	if sum == 0 {
		// Every comparison was exactly 50/50 expected AND the opponent had
		// zero uncertainty. That carries no information, but it is a
		// possible degenerate input, and returning 0 divides by zero
		// below. A large v is the honest answer: "this period told us
		// nothing".
		return math.Inf(1)
	}
	return 1.0 / sum
}

// Delta is step 4, exposed for tests and for explaining a rating move.
// delta = v * sum g(phi_j)(s_j - E).
func Delta(mu float64, ops []Opponent) float64 {
	return variance(mu, ops) * residualSum(mu, ops)
}

// f is the volatility objective exactly as the paper writes it. It is
// discontinuous at the root, which is the entire reason step 4 is an
// Illinois iteration on a bracket.
func f(x, a, phi2, v, delta float64) float64 {
	ex := math.Exp(x)
	den := phi2 + v + ex
	return ex*(delta*delta-phi2-v-ex)/(2*den*den) - (x-a)/(TAU*TAU)
}

// volatility is step 4, the Illinois algorithm.
//
// The bracket is NOT symmetric and NOT guessed. A = a = ln(sigma^2)
// always. If delta^2 > phi^2 + v then B = ln(delta^2 - phi^2 - v);
// otherwise the root lies to the LEFT of a and B = a - k*tau, found by
// searching left in multiples of tau (the paper notes k is almost always
// 1, very rarely 2 or more).
func volatility(phi, sigma, v, delta float64) float64 {
	phi2 := phi * phi
	a := math.Log(sigma * sigma)

	A := a
	var B float64
	if delta*delta > phi2+v {
		B = math.Log(delta*delta - phi2 - v)
	} else {
		k := 1
		for k < 100 && f(a-float64(k)*TAU, a, phi2, v, delta) < 0 {
			k++
		}
		if k >= 100 {
			// No bracket: sigma is non-finite upstream. Return it
			// unchanged rather than propagate a NaN into every rating.
			return sigma
		}
		B = a - float64(k)*TAU
	}
	// The bracket may be oriented EITHER way, and this is not a detail.
	// In the log branch B = ln(delta^2 - phi^2 - v) can be far ABOVE a --
	// for a big upset by an established player it is around +9 -- and a
	// guard that insists A > B rejects that perfectly good bracket and
	// returns sigma unchanged. That silently freezes volatility for
	// exactly the players whose volatility most needs to move, and the
	// symptom is a rating system that looks stable because it is stuck.
	//
	// What must be checked is the BRACKET: that f changes sign between
	// the two endpoints. Illinois needs nothing else -- it is
	// orientation-agnostic, and the false-position step is symmetric.
	// If the signs already agree there is no root in the bracket and
	// sigma is left alone.
	fA := f(A, a, phi2, v, delta)
	fB := f(B, a, phi2, v, delta)
	if math.IsNaN(fA) || math.IsNaN(fB) {
		return sigma
	}
	if fA*fB > 0 {
		// Same sign at both ends: no sign change, so no bracketed root.
		// For finite inputs the branch conditions guarantee a change, so
		// this is defensive.
		return sigma
	}
	if A > B {
		A, B = B, A
		fA, fB = fB, fA
	}

	// Illinois: regula falsi, halving the retained fA when both endpoints
	// land on the same side of the root. That damping is the Illinois
	// modification, and dropping it is what made the older closed form
	// unreliable.
	for i := 0; math.Abs(B-A) > EPS; i++ {
		if i > 200 {
			// The paper reports a max of 19 iterations over 10,000
			// simulations. 200 is 10x the worst observed; reaching it
			// means something is non-finite, and returning A/2 keeps
			// sigma close rather than NaN.
			break
		}
		den := fB - fA
		if den == 0 {
			break
		}
		C := A + (A-B)*fA/den
		fC := f(C, a, phi2, v, delta)

		if fC*fB <= 0 {
			// The new point is on the far side: move A to the old B.
			A = B
			fA = fB
		} else {
			// Same side: damp the retained value.
			fA = fA / 2
		}
		B = C
		fB = fC
	}
	return math.Exp(A / 2)
}

// Update applies one rating period and returns the new rating.
//
// This is Glickman's step 5-8: ONE call for the whole period, not one per
// event. The caller must pass every opponent the player faced in the
// period, with the residual each produced. A single-comparison rule
// applied to a batch inflates ratings; the old kindling notes made
// exactly that mistake.
//
// A period with no opponents returns the rating UNCHANGED. The paper's
// step 6 says an inactive player's RD should widen instead -- that is
// Inactivity(), a separate explicit call, so the decay is visible at the
// call site rather than hidden inside an Update that looks like it did
// nothing.
func (r Rating) Update(ops []Opponent) Rating {
	if len(ops) == 0 {
		return r
	}
	// Guard the boundary. A single non-finite opponent rating turns the
	// whole period into NaN, and because every later period includes that
	// work, one bad row poisons the leaderboard permanently. The
	// non-finite opponents are DROPPED rather than aborting the period: a
	// comparison against an unknown work is missing evidence, not a
	// reason to discard the evidence that is present.
	ops = finiteOps(r, ops)
	if len(ops) == 0 {
		return r
	}
	v := variance(r.Mu, ops)
	if math.IsInf(v, 1) {
		// A period of pure expected results carries no evidence; leave
		// the rating alone rather than divide by infinity.
		return r
	}

	mu, phi := reduced(r.Mu, r.Phi)
	sigma := r.Sigma

	d := Delta(r.Mu, ops)
	newSigma := volatility(phi, sigma, v, d)

	// Step 6: phi* is the pre-convergence cap. Step 7 uses the NEW phi,
	// which is the other thing the draft form got wrong.
	phiStar := math.Sqrt(phi*phi + newSigma*newSigma)
	phiNew := 1.0 / math.Sqrt(1.0/(phiStar*phiStar)+1.0/v)

	// Step 7: mu' = mu + phi'^2 * sum g(phi_j)(s_j - E). No delta and no
	// sigma here; that is the published form.
	muNew := mu + phiNew*phiNew*residualSum(r.Mu, ops)

	muNew, phiNew = unreduced(muNew, phiNew)
	return Rating{Mu: muNew, Phi: phiNew, Sigma: newSigma}
}

// finiteOps returns the opponents that are safe to compute with: the
// player's own rating must be finite, and so must every opponent's mu and
// phi. A zero or negative phi is also rejected, since g(0) is defined but
// a negative phi is not a rating deviation and would make v meaningless.
func finiteOps(r Rating, ops []Opponent) []Opponent {
	if !isFinite(r.Mu) || !isFinite(r.Phi) || !isFinite(r.Sigma) {
		return nil
	}
	out := ops[:0:0]
	for _, o := range ops {
		if isFinite(o.Mu) && isFinite(o.Phi) && o.Phi > 0 {
			out = append(out, o)
		}
	}
	return out
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// Inactivity applies the paper's note on step 6 for a player who did not
// compete: rating and volatility are unchanged, and the RD widens to
// phi* = sqrt(phi^2 + sigma^2).
//
// This is the mechanism that makes a Glicko-2 arena different from Elo: a
// work nobody has compared in months carries more uncertainty, so a single
// later comparison moves it more, and a long-dormant work cannot sit
// frozen at the top of the leaderboard. The batch job calls this lazily --
// per active work, not by sweeping the whole corpus.
func (r Rating) Inactivity() Rating {
	if !isFinite(r.Mu) || !isFinite(r.Phi) || !isFinite(r.Sigma) || r.Phi <= 0 || r.Sigma <= 0 {
		return r
	}
	_, phi := reduced(r.Mu, r.Phi)
	phiStar := math.Sqrt(phi*phi + r.Sigma*r.Sigma)
	_, phiNew := unreduced(0, phiStar)
	return Rating{Mu: r.Mu, Phi: phiNew, Sigma: r.Sigma}
}

// ReferenceExample is Glickman's worked example: three opponents, one
// period, and a known result.
//
// Player: r 1500, RD 200, sigma 0.06.
// Opponents: (1400, 30) scored 1, (1550, 100) scored 0, (1700, 300)
// scored 0.
// Paper: r' 1464.06, RD' 151.52, sigma' 0.05999, v 1.7785,
// delta -0.4834, phi* 1.152862.
func ReferenceExample() (Rating, float64, float64) {
	ops := []Opponent{
		{Mu: 1400, Phi: 30, Score: Win},
		{Mu: 1550, Phi: 100, Score: Loss},
		{Mu: 1700, Phi: 300, Score: Loss},
	}
	r := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}
	return r.Update(ops), variance(r.Mu, ops), Delta(r.Mu, ops)
}

// The paper's printed intermediates, so a failing test can name the
// quantity that drifted instead of only the final answer.
const (
	referenceV = 1.7785
	referenceD = -0.4834
)
