package store

// The verdict attached to an interleaved head-to-head.
//
// The scoring itself is tested in internal/stats; what is pinned here is the
// WIRING, because a verdict computed correctly and never attached is a verdict the
// page cannot show — and the failure would be invisible: an empty `leading` field
// renders as "no lead", which is also the correct answer for a young experiment.

import (
	"context"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func interleaveStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	corpus, err := testcorpus.New(8).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	s, err := Open(ctx, filepath.Join(dir, "state.db"), corpus)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedPair logs nA slots for arm a and nB for arm b, crediting the first `reads`
// slots of each, for one owner and seed.
func seedPair(t *testing.T, s *Store, armA, armB string, nA, readsA, nB, readsB int) {
	t.Helper()
	ctx := context.Background()
	slots := make([]InterleaveSlot, 0, nA+nB)
	work := int64(1000)
	for i := 0; i < nA; i++ {
		slots = append(slots, InterleaveSlot{WorkID: work, Side: "a", Slot: i})
		work++
	}
	for i := 0; i < nB; i++ {
		slots = append(slots, InterleaveSlot{WorkID: work, Side: "b", Slot: i})
		work++
	}
	if err := s.RecordInterleave(ctx, "owner1", "ao3_work", 1, armA, armB, slots); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Credit the first readsA of a and the first readsB of b by their work ids.
	if err := creditFirst(t, s, nA, 1000, readsA); err != nil {
		t.Fatal(err)
	}
	if err := creditFirst(t, s, nB, 1000+nA, readsB); err != nil {
		t.Fatal(err)
	}
}

func creditFirst(t *testing.T, s *Store, n, base, reads int) error {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < reads && i < n; i++ {
		if _, _, _, ok, err := s.CreditInterleave(ctx, "owner1", int64(base+i)); err != nil {
			return err
		} else if !ok {
			t.Fatalf("credit of work %d found no impression", base+i)
		}
	}
	return nil
}

func TestAYoungExperimentHasNoLead(t *testing.T) {
	s := interleaveStore(t)
	seedPair(t, s, "a", "b", 4, 2, 4, 0)
	_, pairs, err := s.InterleaveStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(pairs))
	}
	p := pairs[0]
	if p.Leading != "" {
		t.Fatalf("declared %q the winner on 4 slots each", p.Leading)
	}
	if !p.Underpowered {
		t.Fatal("did not mark the pair as underpowered")
	}
	if p.NoLeadReason == "" {
		t.Fatal("no reason recorded, so the page cannot say why")
	}
	// The raw numbers are still there: the page shows the difference AND the
	// verdict, because a reader is entitled to see both.
	if p.CreditsA != 2 || p.ImpressionsB != 4 {
		t.Fatalf("raw counts changed: %+v", p)
	}
}

func TestEnoughSlotsAndARealDifferenceNamesALead(t *testing.T) {
	s := interleaveStore(t)
	// 60 slots each, 40 reads against 10: p is far below 0.05.
	seedPair(t, s, "a", "b", 60, 40, 60, 10)
	_, pairs, err := s.InterleaveStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := pairs[0]
	if p.Leading != "a" {
		t.Fatalf("leading = %q, want a (p = %.2g, %d/%d vs %d/%d)",
			p.Leading, p.P, p.CreditsA, p.ImpressionsA, p.CreditsB, p.ImpressionsB)
	}
	if p.P >= 0.05 {
		t.Fatalf("p = %.4f and a lead was named", p.P)
	}
	if p.NoLeadReason != "" {
		t.Fatalf("a named lead also carries a refusal reason: %q", p.NoLeadReason)
	}
}

// The same rate over too few slots is refused even when the p-value would clear
// alpha: the floor is a separate rule, not a shortcut for a small p.
func TestAPerfectRecordOverFewSlotsIsStillNoLead(t *testing.T) {
	s := interleaveStore(t)
	seedPair(t, s, "a", "b", 20, 12, 20, 0)
	_, pairs, err := s.InterleaveStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := pairs[0]
	if p.Leading != "" {
		t.Fatalf("declared %q on 20 slots each (p = %.2g)", p.Leading, p.P)
	}
}
