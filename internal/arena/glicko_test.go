package arena

import (
	"math"
	"testing"
)

// Glickman's own worked example, from "Example of the Glicko-2 system"
// (glicko.net/glicko/glicko2.pdf, rev. 22 March 2022). The paper prints
// the answer AND its intermediates, and this test pins all of them.
//
// That is not redundancy. A tolerance set on the final answer alone lets
// three distinct implementations through: a wrong v with a compensating
// error in mu', a right mu' reached by the draft step-7 term, and a
// bisection that happens to land near 0.06. Pinning v, delta and phi*
// separately means a failure names WHICH step is wrong.
func TestGlickoReferenceExample(t *testing.T) {
	r, v, d := ReferenceExample()

	// Step 3: v = 1.7785. The pre-2012 draft formula gives 0.2921, so
	// this one assertion rejects the whole draft generation.
	if math.Abs(v-referenceV) > 0.001 {
		t.Errorf("step 3: v = %.4f, paper says %.4f (draft formula gives 0.2921)", v, referenceV)
	}
	// Step 4: delta = -0.4834.
	if math.Abs(d-referenceD) > 0.001 {
		t.Errorf("step 4: delta = %.4f, paper says %.4f", d, referenceD)
	}
	// Step 6: phi* = 1.152862.
	mu, phi := reduced(1500, 200)
	phiStar := math.Sqrt(phi*phi + r.Sigma*r.Sigma)
	if math.Abs(phiStar-1.152862) > 1e-5 {
		t.Errorf("step 6: phi* = %.6f, paper says 1.152862", phiStar)
	}
	// Step 7: phi' = 0.8722 on the Glicko-2 scale.
	phiNew := 1.0 / math.Sqrt(1.0/(phiStar*phiStar)+1.0/v)
	if math.Abs(phiNew-0.8722) > 0.0002 {
		t.Errorf("step 7: phi'_reduced = %.6f, paper says 0.8722", phiNew)
	}
	// Step 8: back to the Glicko scale.
	if math.Abs(r.Mu-1464.06) > 0.01 {
		t.Errorf("step 8: r' = %.4f, paper says 1464.06", r.Mu)
	}
	if math.Abs(r.Phi-151.52) > 0.01 {
		t.Errorf("step 8: RD' = %.4f, paper says 151.52", r.Phi)
	}
	if math.Abs(r.Sigma-0.05999) > 1e-5 {
		t.Errorf("step 4: sigma' = %.6f, paper says 0.05999", r.Sigma)
	}
	_ = mu
}

// The ln(10) trap, asserted from both sides.
//
// A tolerance set by eyeballing the output is what let this through in the
// old kindling draft: 1456.45 vs 1464.06 is 0.5%, which looks fine next
// to a plot and is a systematically wrong constant across every rating in
// the database. So: the right answer must be near the paper, and the
// wrong one must be BOTH wrong AND more than 5 points away.
func TestE_FunctionUsesLn10Not400(t *testing.T) {
	// Same inputs as the reference example's first opponent.
	right := Expected(1500, 1400, 30)
	wrong := ExpectedWrong(1500, 1400, 30)

	if math.Abs(right-wrong) < 0.01 {
		t.Fatalf("Expected and ExpectedWrong agree (%.5f); the test cannot detect the trap", right)
	}
	// The paper's own table: g(0.1727) = 0.9955, E = 0.639.
	if right < 0.63 || right > 0.65 {
		t.Errorf("E = %.4f, paper's table says 0.639", right)
	}
	if math.Abs(right-wrong) < 0.05 {
		t.Errorf("the two constants differ by only %.4f; too close to be the 400 trap", right-wrong)
	}
	// And the second row: g(0.5756) = 0.9531, E = 0.432.
	if e2 := Expected(1500, 1550, 100); math.Abs(e2-0.432) > 0.005 {
		t.Errorf("E(1500,1550,100) = %.4f, paper's table says 0.432", e2)
	}
	// Third row: g(1.7269) = 0.7242, E = 0.303.
	if e3 := Expected(1500, 1700, 300); math.Abs(e3-0.303) > 0.005 {
		t.Errorf("E(1500,1700,300) = %.4f, paper's table says 0.303", e3)
	}
}

// The end-to-end consequence: the wrong constant produces a rating far
// from the paper, not a near miss.
func TestWrongConstantWouldCorruptEveryRating(t *testing.T) {
	// Recompute v and delta with the Elo-scale divisor, so the whole
	// update is driven by the wrong constant.
	ops := []Opponent{
		{Mu: 1400, Phi: 30, Score: Win},
		{Mu: 1550, Phi: 100, Score: Loss},
		{Mu: 1700, Phi: 300, Score: Loss},
	}
	sum, wsum := 0.0, 0.0
	for _, o := range ops {
		e := ExpectedWrong(1500, o.Mu, o.Phi)
		gj := g(reducePhi(o.Phi))
		sum += gj * (o.Score - e)
		wsum += gj * gj * e * (1 - e)
	}
	if wsum == 0 {
		t.Fatal("the wrong-constant path produced no signal at all")
	}
	vW := 1.0 / wsum
	dW := vW * sum

	// v is the first place the wrong constant shows, and it shows big.
	if math.Abs(vW-referenceV) < 0.1 {
		t.Errorf("the 400 variant gives v = %.4f, within 0.1 of the paper; the test cannot see the trap", vW)
	}
	t.Logf("400-variant: v = %.4f (paper %.4f), delta = %.4f (paper %.4f)", vW, referenceV, dW, referenceD)
}

// A period with no games must leave the rating completely alone,
// including phi. Letting v default to 1 would inflate phi on every idle
// tick, and phi is the confidence -- an arena nobody plays would slowly
// make every rating meaningless.
func TestEmptyPeriodIsANoOp(t *testing.T) {
	r := Rating{Mu: 1600, Phi: 80, Sigma: 0.07}
	got := r.Update(nil)
	if got != r {
		t.Errorf("an empty period changed the rating: %+v -> %+v", r, got)
	}
	if got2 := r.Update([]Opponent{}); got2 != r {
		t.Errorf("an empty slice changed the rating: %+v -> %+v", r, got2)
	}
}

func TestWinRaisesAndLossLowers(t *testing.T) {
	base := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}
	beat := base.Update([]Opponent{{Mu: 1400, Phi: 40, Score: Win}})
	lost := base.Update([]Opponent{{Mu: 1400, Phi: 40, Score: Loss}})

	if beat.Mu <= base.Mu {
		t.Errorf("a win did not raise mu: %.2f -> %.2f", base.Mu, beat.Mu)
	}
	if lost.Mu >= base.Mu {
		t.Errorf("a loss did not lower mu: %.2f -> %.2f", base.Mu, lost.Mu)
	}
	// Both are smaller moves than the rating gap would suggest, because
	// the opponent's uncertainty scales the result. That is the whole
	// point of phi.
	if beat.Mu >= 1600 {
		t.Errorf("a win against a 1400 moved mu to %.2f; uncertainty is not scaling the result", beat.Mu)
	}
}

func TestDrawIsHalfway(t *testing.T) {
	base := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}
	drew := base.Update([]Opponent{{Mu: 1500, Phi: 100, Score: Draw}})
	// Against an equal opponent, a draw should barely move mu.
	if math.Abs(drew.Mu-1500) > 1.0 {
		t.Errorf("a draw against an equal opponent moved mu to %.4f", drew.Mu)
	}
}

// phi must SHRINK with every period, and settle rather than collapse to
// zero. Glickman's step 6 caps it at phi* and floors it at the linear sum.
func TestPhiShrinksAndStaysBounded(t *testing.T) {
	r := Rating{Mu: 1500, Phi: 350, Sigma: 0.06}
	prev := r.Phi
	for i := 0; i < 20; i++ {
		r = r.Update([]Opponent{{Mu: 1500, Phi: 100, Score: Win}})
		if r.Phi >= prev {
			t.Fatalf("period %d: phi grew %.3f -> %.3f", i, prev, r.Phi)
		}
		prev = r.Phi
		if r.Phi < 1.0 {
			t.Fatalf("period %d: phi collapsed to %.4f; the floor is missing", i, r.Phi)
		}
	}
}

// A losing period SHOULD move volatility, and this is the assertion the
// old kindling suite made deliberately: "a losing period must not move
// sigma" is wrong, and a test that asserted it would have hidden a broken
// volatility step. The model working means sigma is approximately stable
// over many periods, not frozen at 0.06.
// The paper's example is a LOSING period and sigma' is 0.05999 against an
// initial 0.06 -- a 0.2% change. So "a losing period must move sigma" is
// the wrong assertion at paper tolerances, and a test that demanded a
// large move would fail a correct implementation.
func TestVolatilityIsNearFlatForThePaperExample(t *testing.T) {
	r := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}
	got := r.Update([]Opponent{
		{Mu: 1400, Phi: 30, Score: Win},
		{Mu: 1550, Phi: 100, Score: Loss},
		{Mu: 1700, Phi: 300, Score: Loss},
	})
	if math.Abs(got.Sigma-0.05999) > 1e-5 {
		t.Errorf("sigma' = %.6f, paper says 0.05999", got.Sigma)
	}
}

// Inactivity widens the RD and leaves mu and sigma alone: the paper's
// note on step 6.
func TestInactivityOnlyWidensRD(t *testing.T) {
	r := Rating{Mu: 1700, Phi: 60, Sigma: 0.08}
	got := r.Inactivity()
	if got.Mu != r.Mu {
		t.Errorf("inactivity changed mu: %.2f -> %.2f", r.Mu, got.Mu)
	}
	if got.Sigma != r.Sigma {
		t.Errorf("inactivity changed sigma: %.4f -> %.4f", r.Sigma, got.Sigma)
	}
	if got.Phi <= r.Phi {
		t.Errorf("inactivity did not widen the RD: %.2f -> %.2f", r.Phi, got.Phi)
	}
	// phi* = sqrt(phi^2 + sigma^2) on the Glicko scale, which for these
	// values is sqrt(60^2 + 0.08^2) -- i.e. the RD barely moves, because
	// sigma is tiny next to phi. That is correct, and it is why repeated
	// inactivity is the only thing that makes the decay visible.
	for i := 0; i < 20; i++ {
		r = r.Inactivity()
	}
	if r.Phi <= got.Phi {
		t.Errorf("twenty inactive periods left the RD at %.2f (was %.2f); decay is not accumulating", r.Phi, got.Phi)
	}
}

// Inactivity raises phi through RD: this is the reason for Glicko over
// Elo. A rating that is not updated for a long period should carry more
// uncertainty, and a caller expresses that by passing a wider phi.
func TestUncertaintyIsExpressible(t *testing.T) {
	// Same result, different opponent confidence: a well-established
	// opponent should move mu further than a brand-new one.
	settled := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}.
		Update([]Opponent{{Mu: 1500, Phi: 20, Score: Win}})
	noisy := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}.
		Update([]Opponent{{Mu: 1500, Phi: 350, Score: Win}})

	if math.Abs(settled.Mu-1500) <= math.Abs(noisy.Mu-1500) {
		t.Errorf("a settled opponent (%.3f) moved mu no more than an unknown one (%.3f)",
			settled.Mu, noisy.Mu)
	}
}

func TestReducedRoundTrip(t *testing.T) {
	mu, phi := 1650.0, 87.5
	r, p := reduced(mu, phi)
	mu2, phi2 := unreduced(r, p)
	if math.Abs(mu2-mu) > 1e-9 || math.Abs(phi2-phi) > 1e-9 {
		t.Errorf("scale round trip lost precision: %.10f/%.10f -> %.10f/%.10f", mu, phi, mu2, phi2)
	}
}

// The paper prints its volatility ITERATION TABLE, which is a far stronger
// assertion than "sigma moved": it pins fA, fB and the converged A to the
// paper's own five significant figures.
//
// The old version of this test asserted that sigma must grow after five
// heavy losses, and it FAILED against a correct implementation. Glicko's
// volatility is slow by design: the paper's own losing period moves it
// from 0.06 to 0.05999, a 0.2% change. A test demanding a visible move
// demands a wrong model.
func TestVolatilityIterationMatchesThePaperTable(t *testing.T) {
	// The paper's numbers: v 1.7785, delta -0.4834, phi 1.1513,
	// sigma 0.06, tau 0.5.
	phi2 := 1.1513 * 1.1513
	v := referenceV
	d := referenceD
	a := math.Log(0.06 * 0.06)

	// Iteration 0, exactly as the paper's table prints it.
	A, B := a, a-TAU // k = 1
	if got := f(A, a, phi2, v, d); math.Abs(got-(-0.00053567)) > 1e-7 {
		t.Errorf("fA = %.8f, paper's table says -0.00053567", got)
	}
	if got := f(B, a, phi2, v, d); math.Abs(got-1.999675) > 1e-6 {
		t.Errorf("fB = %.8f, paper's table says 1.999675", got)
	}

	// The whole iteration, which must land on the paper's A = -5.62696.
	got := volatility(1.1513, 0.06, v, d)
	wantSigma := 0.05999
	if math.Abs(got-wantSigma) > 1e-5 {
		t.Errorf("sigma' = %.6f, paper says %.5f", got, wantSigma)
	}
}

// The bracket in step 4 can be oriented either way, and getting that wrong
// is a silent failure: the guard rejects a valid bracket and returns sigma
// UNCHANGED, so volatility freezes for exactly the players whose
// volatility most needs to move.
//
// In the log branch B = ln(delta^2 - phi^2 - v) lands far ABOVE a for a big
// upset by an established player, which is what this test uses. Before the
// fix the result was sigma unchanged; the correct answer is a rise.
func TestVolatilityBracketMayBeInverted(t *testing.T) {
	// A well-established work (RD 30) beats a much stronger, also
	// well-known one. delta^2 > phi^2 + v, so this is the log branch.
	r := Rating{Mu: 1500, Phi: 30, Sigma: 0.06}
	ops := []Opponent{{Mu: 2300, Phi: 20, Score: Win}}

	// Confirm we really are on the inverted bracket, so this test cannot
	// silently stop testing the thing it claims to.
	phi2 := reducePhi(30) * reducePhi(30)
	v := variance(r.Mu, ops)
	d := Delta(r.Mu, ops)
	a := math.Log(0.06 * 0.06)
	B := math.Log(d*d - phi2 - v)
	if !(B > a) {
		t.Fatalf("precondition gone: the bracket is no longer inverted (B=%.4f, a=%.4f)", B, a)
	}

	got := r.Update(ops)
	if got.Sigma <= 0.06 {
		t.Errorf("sigma' = %.6f, want a rise above 0.06 for a big upset by an established player", got.Sigma)
	}
}

// A bracketing failure must return sigma UNCHANGED rather than a NaN, and
// the rating must stay finite. A NaN in one work's rating propagates into
// every comparison it appears in, and from there into the leaderboard.
func TestNoNaNEverEscapes(t *testing.T) {
	cases := []Rating{
		Initial(),
		{Mu: 0, Phi: 1, Sigma: 0.06},
		{Mu: 3000, Phi: 0.5, Sigma: 0.5},
		{Mu: 1500, Phi: 350, Sigma: 0.06},
		{Mu: math.Inf(1), Phi: 350, Sigma: 0.06},
		{Mu: math.NaN(), Phi: 350, Sigma: 0.06},
	}
	opsets := [][]Opponent{
		{{Mu: 1400, Phi: 30, Score: Win}},
		{{Mu: 1400, Phi: 30, Score: Win}, {Mu: 1600, Phi: 100, Score: Loss}},
		{{Mu: math.NaN(), Phi: 30, Score: Win}},
		{{Mu: 1400, Phi: 0, Score: Draw}},
	}
	// A rating that ARRIVES non-finite is returned unchanged, not
	// laundered into a fresh NaN elsewhere. Repair belongs to the store,
	// which can reset the row to Initial() and log it; the maths package
	// inventing a plausible-looking number for a corrupted row would
	// hide the corruption instead of surfacing it.
	corrupt := Rating{Mu: math.NaN(), Phi: 350, Sigma: 0.06}
	if got := corrupt.Update([]Opponent{{Mu: 1400, Phi: 30, Score: Win}}); !math.IsNaN(got.Mu) {
		t.Errorf("a NaN rating was repaired to %.2f; that hides the corruption", got.Mu)
	}
	if got := (Rating{Mu: math.Inf(1), Phi: 350, Sigma: 0.06}).Inactivity(); !math.IsInf(got.Mu, 1) {
		t.Errorf("an infinite rating was repaired to %.2f", got.Mu)
	}

	for _, r := range cases {
		if !isFinite(r.Mu) {
			// Covered above: returned unchanged on purpose.
			continue
		}
		for i, ops := range opsets {
			got := r.Update(ops)
			if math.IsNaN(got.Mu) || math.IsNaN(got.Phi) || math.IsNaN(got.Sigma) {
				t.Errorf("r%+v ops[%d] -> NaN %+v", r, i, got)
			}
			if math.IsInf(got.Mu, 0) || math.IsInf(got.Phi, 0) {
				t.Errorf("r%+v ops[%d] -> Inf %+v", r, i, got)
			}
			in := r.Inactivity()
			if math.IsNaN(in.Phi) || math.IsInf(in.Phi, 0) {
				t.Errorf("r%+v Inactivity -> %+v", r, in)
			}
		}
	}
}

// A non-finite OPPONENT is dropped, not fatal, and the period still
// updates from the comparisons that remain. A corrupted row for one work
// must not freeze every other work in the arena.
func TestBadOpponentIsDroppedNotFatal(t *testing.T) {
	r := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}
	withBad := r.Update([]Opponent{
		{Mu: math.NaN(), Phi: 30, Score: Loss}, // poisoned
		{Mu: 1400, Phi: 30, Score: Win},        // good
	})
	cleanOnly := r.Update([]Opponent{{Mu: 1400, Phi: 30, Score: Win}})

	if math.Abs(withBad.Mu-cleanOnly.Mu) > 1e-9 {
		t.Errorf("a poisoned opponent changed the outcome: %.6f vs %.6f", withBad.Mu, cleanOnly.Mu)
	}
	if withBad.Mu <= r.Mu {
		t.Errorf("the surviving comparison did not register: %.2f -> %.2f", r.Mu, withBad.Mu)
	}

	// A period where EVERY opponent is poisoned leaves the rating alone.
	allBad := r.Update([]Opponent{
		{Mu: math.NaN(), Phi: 30, Score: Loss},
		{Mu: math.Inf(1), Phi: 30, Score: Win},
		{Mu: 1400, Phi: 0, Score: Win}, // zero phi is not a deviation
		{Mu: 1400, Phi: -5, Score: Win},
	})
	if allBad != r {
		t.Errorf("a period of only-bad opponents changed the rating: %+v -> %+v", r, allBad)
	}
}
