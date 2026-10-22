package store

import (
	"context"
	"testing"
)

func TestInterleaveCreditGoesToTheArmThatShowedTheWork(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)

	// Work 10 came from arm "alpha", work 20 from "beta".
	err := s.RecordInterleave(ctx, "owner", "ao3_work", 1, "alpha", "beta", []InterleaveSlot{
		{WorkID: 10, Side: "a", Slot: 0},
		{WorkID: 20, Side: "b", Slot: 1},
	})
	if err != nil {
		t.Fatalf("RecordInterleave: %v", err)
	}

	arm, _, _, ok, err := s.CreditInterleave(ctx, "owner", 20)
	if err != nil || !ok {
		t.Fatalf("credit work 20: ok=%v err=%v", ok, err)
	}
	if arm != "beta" {
		t.Fatalf("work 20 credited to %q, want beta", arm)
	}

	// And only once: a second seen mark on the same impression is a
	// revisit, not a second vote. Counting it would weight whichever arm
	// the reader happened to come back to.
	if _, _, _, ok, _ := s.CreditInterleave(ctx, "owner", 20); ok {
		t.Fatal("the same impression was credited twice")
	}

	// A work this owner was never shown is not evidence about either arm.
	if _, _, _, ok, _ := s.CreditInterleave(ctx, "owner", 999); ok {
		t.Fatal("credited a work that was never shown")
	}
}

// A credit is per owner. Two readers shown the same work are two impressions,
// and one reader's click must not close the other's row.
func TestInterleaveCreditIsPerOwner(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)
	slot := []InterleaveSlot{{WorkID: 7, Side: "a", Slot: 0}}
	if err := s.RecordInterleave(ctx, "one", "ao3_work", 1, "a1", "b1", slot); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordInterleave(ctx, "two", "ao3_work", 1, "a1", "b1", slot); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := s.CreditInterleave(ctx, "one", 7); err != nil || !ok {
		t.Fatalf("owner one: ok=%v err=%v", ok, err)
	}
	if _, _, _, ok, err := s.CreditInterleave(ctx, "two", 7); err != nil || !ok {
		t.Fatalf("owner two was closed by owner one's credit: ok=%v err=%v", ok, err)
	}
}

// The standings divide credits by impressions, so an arm with more slots must
// not look better merely for having been shown more.
func TestInterleaveStatsReportRatesNotCounts(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)

	// alpha showed 4 items, 1 credited. beta showed 2, 2 credited.
	err := s.RecordInterleave(ctx, "o1", "ao3_work", 1, "alpha", "beta", []InterleaveSlot{
		{WorkID: 1, Side: "a", Slot: 0}, {WorkID: 2, Side: "b", Slot: 1},
		{WorkID: 3, Side: "a", Slot: 2}, {WorkID: 4, Side: "b", Slot: 3},
		{WorkID: 5, Side: "a", Slot: 4}, {WorkID: 6, Side: "a", Slot: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2, 4} {
		if _, _, _, ok, err := s.CreditInterleave(ctx, "o1", id); err != nil || !ok {
			t.Fatalf("credit %d: ok=%v err=%v", id, ok, err)
		}
	}

	arms, pairs, err := s.InterleaveStats(ctx)
	if err != nil {
		t.Fatalf("InterleaveStats: %v", err)
	}
	by := map[string]InterleaveArmStat{}
	for _, a := range arms {
		by[a.Arm] = a
	}
	if got := by["alpha"]; got.Impressions != 4 || got.Credits != 1 {
		t.Fatalf("alpha = %+v, want 4 impressions and 1 credit", got)
	}
	if got := by["beta"]; got.Impressions != 2 || got.Credits != 2 {
		t.Fatalf("beta = %+v, want 2 impressions and 2 credits", got)
	}
	if by["alpha"].CreditRate != 0.25 || by["beta"].CreditRate != 1.0 {
		t.Fatalf("rates alpha=%v beta=%v, want 0.25/1.0", by["alpha"].CreditRate, by["beta"].CreditRate)
	}
	if len(pairs) != 1 {
		t.Fatalf("%d pairs, want 1", len(pairs))
	}
	// The comparison is what the experiment is FOR: beta's rate is 0.75
	// higher, so the difference is NEGATIVE -- it is measured A minus B, and
	// a page that prints it without saying which way is a page whose sign a
	// reader has to guess.
	if diff := pairs[0].Difference; diff > -0.749 || diff < -0.751 {
		t.Fatalf("difference = %v, want -0.75 (A minus B: alpha 0.25 - beta 1.0)", diff)
	}
}

// An arm that was shown and never acted on must still appear in the
// denominator. If a zero-credit arm vanished from the stats it would look like
// it had never been in the experiment -- or, worse, like a perfect arm with no
// data.
func TestInterleaveStatsKeepAnUncreditedArm(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)
	if err := s.RecordInterleave(ctx, "o", "ao3_work", 1, "quiet", "loud", []InterleaveSlot{
		{WorkID: 1, Side: "a", Slot: 0}, {WorkID: 2, Side: "b", Slot: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, _ := s.CreditInterleave(ctx, "o", 2); !ok {
		t.Fatal("credit failed")
	}
	arms, _, err := s.InterleaveStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range arms {
		if a.Arm == "quiet" {
			found = true
			if a.Impressions != 1 || a.Credits != 0 {
				t.Fatalf("quiet = %+v, want 1 impression and 0 credits", a)
			}
		}
	}
	if !found {
		t.Fatal("an arm with no credits disappeared from the standings")
	}
}

func TestRecordInterleaveRefusesAnUnknownSide(t *testing.T) {
	s := algoTestStore(t)
	err := s.RecordInterleave(context.Background(), "o", "ao3_work", 1, "a", "b",
		[]InterleaveSlot{{WorkID: 1, Side: "c", Slot: 0}})
	if err == nil {
		t.Fatal("recorded a slot from side \"c\"; only a and b exist")
	}
}
