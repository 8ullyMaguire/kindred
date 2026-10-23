package store

// "Works you keep avoiding" (#60).
//
// The list is a claim about a reader ("you keep skipping this"), so the tests
// pin the three ways such a claim can be wrong:
//
//   - a work shown twice is not avoidance, it is a two-sighting coincidence;
//   - a work the reader ACTED on is not avoidance, whichever arm showed it;
//   - a work the reader READ is not avoidance even if the impression was never
//     credited, because reading from a bookmark screen never touches this log.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func showWork(t *testing.T, s *Store, owner string, workID int64, hoursAgo int, credited int, arm string) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Duration(hoursAgo) * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := s.DB.ExecContext(context.Background(),
		`INSERT INTO interleave_impressions
		   (owner_key, seed_kind, seed_id, arm_a, arm_b, work_id, arm, slot, credited, shown_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		owner, "ao3_tag", 7, "default", "hybrid", workID, arm, 0, credited, at); err != nil {
		t.Fatal(err)
	}
}

func TestSkippedNeedsMoreThanACombinationOfCircumstances(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Work 11: three shows, never acted on -> skipped.
	for _, h := range []int{1, 5, 9} {
		showWork(t, s, "owner", 11, h, 0, "a")
	}
	// Work 12: two shows -> not yet a claim about the reader.
	for _, h := range []int{2, 6} {
		showWork(t, s, "owner", 12, h, 0, "b")
	}
	// Work 13: three shows, one of them acted on -> not avoidance.
	showWork(t, s, "owner", 13, 3, 0, "a")
	showWork(t, s, "owner", 13, 7, 0, "a")
	showWork(t, s, "owner", 13, 11, 1, "a")
	// Work 14: three shows, and the reader marked it read from elsewhere.
	for _, h := range []int{4, 8, 12} {
		showWork(t, s, "owner", 14, h, 0, "b")
	}
	if err := s.MarkRead(ctx, "owner", WorkEntityID(14)); err != nil {
		t.Fatal(err)
	}
	// Another reader's history must not leak in.
	for _, h := range []int{1, 2, 3} {
		showWork(t, s, "someone-else", 11, h, 0, "a")
	}

	got, err := s.SkippedWorks(ctx, "owner", 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WorkID != 11 {
		t.Fatalf("skipped = %+v, want exactly work 11", got)
	}
	if got[0].Shows != 3 {
		t.Fatalf("shows = %d, want 3 (one row per showing)", got[0].Shows)
	}
	if got[0].LastShown == "" {
		t.Fatal("the row does not say when it was last shown, so the reader cannot tell " +
			"whether the list is about today or last month")
	}

	// And the same reader's own second read: work 13 loses its skip status the
	// moment any showing was acted on, and 11 is unaffected.
	if _, _, _, _, err := s.CreditInterleave(ctx, "owner", 13); err != nil {
		t.Fatal(err)
	}
	again, err := s.SkippedWorks(ctx, "owner", 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range again {
		if w.WorkID == 13 {
			t.Fatal("a credited work is still offered as skipped")
		}
	}
}

// TestSkippedIsOrderedByHowOften: the list is a "most skipped first" list, or it
// is a random sample of two works and a reader cannot tell which to act on.
func TestSkippedIsOrderedByHowOften(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for h := 1; h <= 5; h++ {
		showWork(t, s, "owner", 21, h, 0, "a")
	}
	for h := 1; h <= 3; h++ {
		showWork(t, s, "owner", 22, h, 0, "b")
	}
	got, err := s.SkippedWorks(ctx, "owner", 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].WorkID != 21 || got[0].Shows != 5 {
		t.Fatalf("first row is work %d with %d shows, want 21 with 5", got[0].WorkID, got[0].Shows)
	}
	if len(got[0].Arms) != 1 || got[0].Arms[0] != "a" {
		t.Fatalf("arms = %v, want the arm that showed it", got[0].Arms)
	}
}

// TestSkippedWithNoHistoryIsEmptyRatherThanAnError: a new reader must get an
// empty list and a sentence, not a 500 — and definitely not a list invented from
// the corpus.
func TestSkippedWithNoHistoryIsEmpty(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.SkippedWorks(ctx, "brand-new", 3, 10)
	if err != nil {
		t.Fatalf("a reader with no impressions got an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a reader with no impressions got %d rows: %s", len(got), fmt.Sprint(got))
	}
}
