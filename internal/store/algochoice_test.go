package store

// The credit rule: a click credits the list that CARRIED the work, and only once.
//
// The failure this pins is the one a naive implementation has: crediting the
// reader's most recent impression of the kind, which pays an algorithm for a
// click that may have come from search, and pays whichever arm happened to be
// served last.

import (
	"context"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

func TestACreditGoesToTheListThatCarriedTheWork(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Two lists for one reader: the first carried work 7, the second did not.
	if err := s.RecordAlgoImpression(ctx, "owner", arena.KindWork, "hybrid", "default", 100, []int64{5, 6, 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAlgoImpression(ctx, "owner", arena.KindWork, "collab", "challenger", 200, []int64{8, 9}); err != nil {
		t.Fatal(err)
	}

	// Work 7 was on the FIRST list, so the first list is credited even though
	// the second is more recent.
	n, err := s.CreditAlgoImpressionByWork(ctx, "owner", arena.KindWork, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("credited %d rows, want 1", n)
	}
	var algo string
	var credited int
	if err := s.DB.QueryRow(
		`SELECT algo, credited FROM algo_impressions WHERE seed_id = 100`).Scan(&algo, &credited); err != nil {
		t.Fatal(err)
	}
	if algo != "hybrid" || credited != 1 {
		t.Fatalf("credited %s/%d, want hybrid/1", algo, credited)
	}

	// Work 9 was only on the second list.
	if _, err := s.CreditAlgoImpressionByWork(ctx, "owner", arena.KindWork, 9); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT credited FROM algo_impressions WHERE seed_id = 200`).Scan(&credited); err != nil {
		t.Fatal(err)
	}
	if credited != 1 {
		t.Fatalf("the second list was not credited for a work it carried")
	}

	// A work on NO list credits nothing — and must not fall back to the most
	// recent impression, which is the whole point.
	if n, err := s.CreditAlgoImpressionByWork(ctx, "owner", arena.KindWork, 4242); err != nil || n != 0 {
		t.Fatalf("a work on no list credited %d rows (err %v), want 0", n, err)
	}

	// Crediting twice for the same work is one credit: the UPDATE is pinned to
	// credited = 0.
	if n, _ := s.CreditAlgoImpressionByWork(ctx, "owner", arena.KindWork, 7); n != 0 {
		t.Fatalf("a second credit for the same work counted %d rows", n)
	}

	// And the standings read the result without inventing rows.
	stats, err := s.AlgoStandingsByKind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hybrid, collab AlgoArmStat
	for _, st := range stats {
		switch st.Algo {
		case "hybrid":
			hybrid = st
		case "collab":
			collab = st
		}
	}
	if hybrid.Credits != 1 || collab.Credits != 1 {
		t.Fatalf("credits hybrid=%d collab=%d, want 1 and 1", hybrid.Credits, collab.Credits)
	}
	if hybrid.CreditRate != 1 {
		t.Fatalf("hybrid rate = %v, want 1.0 for one credit on one impression", hybrid.CreditRate)
	}
}
