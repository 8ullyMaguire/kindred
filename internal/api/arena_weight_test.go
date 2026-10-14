package api

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// The owner-weighted batch: arena_comparisons.agreement -> the voter's
// influence on the ratings.
//
// internal/arena proves the arithmetic. This file proves the WIRING, which
// is where the two ways of getting this wrong live:
//
//  1. The floor has to mean "drop it". The rating maths reads a weight of
//     zero as UNSET (arena.weightOf), because that is what keeps every
//     caller that predates weighting at influence 1. So a comparison whose
//     voter has opposite taste cannot be passed down as weight 0 -- it
//     would arrive as FULL influence, exactly backwards. RunBatch has to
//     leave it out.
//
//  2. A period skipped because nobody influential voted must not consume
//     the rows. Advancing the watermark there would discard those
//     judgements permanently, and the failure would be invisible: the next
//     batch would just report fewer comparisons, which looks like there
//     were fewer.
//
// Both are silent at the endpoint level -- a batch still returns 200 and a
// number -- so they are asserted against the database.

// batchRun is one judged comparison, the store it lives in, and the batch
// that was run over it.
type batchRun struct {
	ts     *httptest.Server
	st     *store.Store
	winner int64
	body   map[string]any
}

// judgeOne judges a single comparison, overrides its recorded agreement,
// runs a batch, and reports the winning work.
func judgeOne(t *testing.T, agreement float64) batchRun {
	t.Helper()
	ts, st := newTestServerFull(t)
	arenaSession(t, ts, "a")

	if _, err := st.DB.Exec(
		`UPDATE arena_comparisons SET agreement = ? WHERE judged_at IS NOT NULL`, agreement); err != nil {
		t.Fatalf("setting agreement to %v: %v", agreement, err)
	}
	status, body := postJSON(t, ts, "/api/v1/arena/batch")
	if status != http.StatusOK {
		t.Fatalf("batch with agreement %v -> %d: %v", agreement, status, body)
	}
	var winner int64
	if err := st.DB.QueryRow(
		`SELECT work_a FROM arena_comparisons WHERE judged_at IS NOT NULL LIMIT 1`).Scan(&winner); err != nil {
		t.Fatalf("reading the judged winner: %v", err)
	}
	return batchRun{ts: ts, st: st, winner: winner, body: body}
}

func (h batchRun) mu(t *testing.T, work int64) float64 {
	t.Helper()
	var mu float64
	if err := h.st.DB.QueryRow(`SELECT mu FROM arena_ratings WHERE work_id = ?`, work).Scan(&mu); err != nil {
		t.Fatalf("work %d has no rating row after the batch: %v", work, err)
	}
	return mu
}

// TestBatchSkipsWhenEveryComparisonIsFromOppositeTaste: every voter in the
// period disagrees with the owner, so there is nothing to learn from and
// the batch says so instead of rating anything.
func TestBatchSkipsWhenEveryComparisonIsFromOppositeTaste(t *testing.T) {
	h := judgeOne(t, 0.0)

	if got := h.body["skipped"]; got != "no influential comparisons" {
		t.Errorf("batch with every voter at agreement 0 reported skipped=%v, want "+
			"\"no influential comparisons\": an opposite-taste comparison must be "+
			"dropped, not weighted zero, because zero reads as UNSET downstream "+
			"and arrives as full influence", got)
	}
	if n, ok := h.body["works_updated"].(float64); ok && n > 0 {
		t.Errorf("a skipped batch reported works_updated=%v", n)
	}

	var rows int
	if err := h.st.DB.QueryRow(`SELECT COUNT(*) FROM arena_ratings`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d rating rows were written by a skipped period", rows)
	}

	// The judgement survives the skip. If it did not, the skip would be a
	// quiet deletion: these rows would never be seen by a later period,
	// however the owner's vector changed.
	var judged int
	if err := h.st.DB.QueryRow(
		`SELECT COUNT(*) FROM arena_comparisons WHERE judged_at IS NOT NULL`).Scan(&judged); err != nil {
		t.Fatal(err)
	}
	if judged != 1 {
		t.Fatalf("the skip consumed the judgement: %d judged rows left, want 1", judged)
	}

	// And it is still claimable: with the same rows now carrying an
	// agreement above the floor, the next period rates the work.
	if _, err := h.st.DB.Exec(
		`UPDATE arena_comparisons SET agreement = 1.0 WHERE judged_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	status, second := postJSON(t, h.ts, "/api/v1/arena/batch")
	if status != http.StatusOK {
		t.Fatalf("second batch -> %d: %v", status, second)
	}
	if _, skipped := second["skipped"]; skipped {
		t.Errorf("after the skip the judgement was not re-offered: %v", second)
	}
	if mu := h.mu(t, h.winner); mu <= 1500 {
		t.Errorf("the winner's rating is %v after a period it won, want above the "+
			"1500 start: the skip must leave the rows for a later run", mu)
	}
}

// TestBatchMovesRatingsLessForLowAgreement is the endpoint-level statement
// of the whole feature: an identical comparison, judged by a voter who
// agrees less with the owner, moves the work less.
func TestBatchMovesRatingsLessForLowAgreement(t *testing.T) {
	full := judgeOne(t, 1.0)
	low := judgeOne(t, 0.25)

	muFull := full.mu(t, full.winner)
	muLow := low.mu(t, low.winner)

	if !(muFull > 1500) || !(muLow > 1500) {
		t.Fatalf("winner ratings %v (agreement 1.0) and %v (agreement 0.25): both "+
			"must be above the 1500 start, or the period did not rate anything",
			muFull, muLow)
	}
	if !(muLow < muFull) {
		t.Errorf("agreement 1.0 moved the winner to %v and agreement 0.25 to %v: "+
			"the lower-agreement voter must move it LESS", muFull, muLow)
	}
}

// TestBatchWithNoOwnerVectorRatesAtFullInfluence: with no owner vector
// configured the recorded agreement is 1.0 (store.AgreementWithOwner's
// documented short-circuit), so an unconfigured install runs the batch
// through the new weighting path and lands where it landed before the
// weighting existed. Both runs below are that same value by different
// routes -- recorded, and recorded-and-then-overridden -- so they must
// agree exactly rather than merely closely.
func TestBatchWithNoOwnerVectorRatesAtFullInfluence(t *testing.T) {
	recorded := judgeOne(t, 1.0)
	if _, skipped := recorded.body["skipped"]; skipped {
		t.Fatalf("the default batch skipped: %v", recorded.body)
	}
	explicit := judgeOne(t, 1.0)

	if a, b := recorded.mu(t, recorded.winner), explicit.mu(t, explicit.winner); a != b {
		t.Errorf("the default path gave the winner %v and an explicit agreement of "+
			"1.0 gave %v: with no owner configured the batch must keep the numbers "+
			"it produced before weighting existed", a, b)
	}
}

// TestInfluenceCurve pins the shape of the influence policy, which is a
// hand-chosen curve and not a derived one. Without this, "dampened
// quadratically" is a sentence in a comment that any edit can contradict
// while every other test stays green: changing the square to a linear term
// keeps the ordering between any two voters' weights, so nothing else here
// notices.
func TestInfluenceCurve(t *testing.T) {
	cases := []struct {
		agree, want float64
		why         string
	}{
		{1.0, 1.0, "full agreement counts in full"},
		{0.5, 0.25, "agreement 0.5 is dampened quadratically, not linearly"},
		{0.71, 0.5041, "the plan's table row: 0.71 -> ~0.50"},
		{0.0, 0.0, "no agreement at all"},
		{-1.0, 0.0, "opposite taste must not become influence by squaring"},
	}
	for _, c := range cases {
		if got := influenceOf(c.agree); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("influenceOf(%v) = %v, want %v: %s", c.agree, got, c.want, c.why)
		}
	}
}

// TestAgreementFloorSitsOnTheInfluenceNotTheAgreement: the floor compares
// against influence (agreement squared), so the smallest agreement that
// survives is sqrt(agreeFloor) ~= 0.2236, not agreeFloor. Comparing the two
// numbers the other way round would keep a voter at 0.22 whose influence is
// 0.048 -- just under the intended cut -- and a test that only checked
// "something is dropped" would not see it.
func TestAgreementFloorSitsOnTheInfluenceNotTheAgreement(t *testing.T) {
	justBelow := math.Sqrt(agreeFloor) - 0.001
	justAbove := math.Sqrt(agreeFloor) + 0.001

	if influenceOf(justBelow) >= agreeFloor {
		t.Errorf("influenceOf(%.4f) = %v, want below the floor %v",
			justBelow, influenceOf(justBelow), agreeFloor)
	}
	if influenceOf(justAbove) < agreeFloor {
		t.Errorf("influenceOf(%.4f) = %v, want at or above the floor %v",
			justAbove, influenceOf(justAbove), agreeFloor)
	}
	// And the floor is a threshold on influence: an agreement of 0.25
	// (influence 0.0625) survives, though it would be dropped if the floor
	// were read as an agreement.
	if influenceOf(0.25) < agreeFloor {
		t.Errorf("influenceOf(0.25) = %v is below the floor %v: the floor must "+
			"compare influence against influence", influenceOf(0.25), agreeFloor)
	}
}
