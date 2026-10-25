package store

import (
	"context"
	"errors"
	"testing"
)

func duelStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// A vote with no owner cannot be de-duplicated, so one reader reloading the page could move the
// standings as many times as they like. The page mints a session before asking; this is the
// second guard.
func TestExplanationDuelRefusesANamelessVote(t *testing.T) {
	s := duelStore(t)
	err := s.RecordExplanationDuel(context.Background(), "",
		ExplanationDuel{SeedID: 1, WorkID: 2, Winner: DuelTags})
	if !errors.Is(err, ErrNoIdentifier) {
		t.Fatalf("err = %v, want ErrNoIdentifier", err)
	}
}

func TestExplanationDuelRefusesASelfExplanation(t *testing.T) {
	s := duelStore(t)
	err := s.RecordExplanationDuel(context.Background(), "owner",
		ExplanationDuel{SeedID: 9, WorkID: 9, Winner: DuelTags})
	if err == nil {
		t.Fatal("a work explaining itself was recorded")
	}
}

func TestExplanationDuelIsOneVotePerReaderPerWork(t *testing.T) {
	ctx := context.Background()
	s := duelStore(t)
	for _, w := range []string{DuelTags, DuelReaders, DuelTags} {
		if err := s.RecordExplanationDuel(ctx, "reader-1",
			ExplanationDuel{SeedID: 1, WorkID: 2, Winner: w}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ExplanationStandings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Three votes from ONE reader on one work is still one vote: the last one.
	if rows[0].Judged != 1 {
		t.Fatalf("judged = %d, want 1 (upsert, not append)", rows[0].Judged)
	}
	if rows[0].Wins != 1 || rows[1].Losses != 1 {
		t.Fatalf("tags = %+v, readers = %+v; want one tags win", rows[0], rows[1])
	}
}

func TestExplanationStandingsAreMirrored(t *testing.T) {
	ctx := context.Background()
	s := duelStore(t)
	votes := []string{DuelTags, DuelTags, DuelReaders, DuelNeither, DuelTags}
	for i, w := range votes {
		if err := s.RecordExplanationDuel(ctx, "r"+itoa(i),
			ExplanationDuel{SeedID: 10, WorkID: int64(100 + i), Winner: w}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ExplanationStandings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d standings rows, want both kinds even when one has no vote", len(rows))
	}
	tags, readers := rows[0], rows[1]
	if tags.Wins != 3 || tags.Losses != 1 || tags.Ties != 1 || tags.Judged != 5 {
		t.Fatalf("tags = %+v, want 3/1/1 of 5", tags)
	}
	if readers.Wins != 1 || readers.Losses != 3 || readers.Ties != 1 {
		t.Fatalf("readers = %+v, want the mirror of tags", readers)
	}
}

// The power floor: four decisive duels cannot name a leader, and the verdict must SAY why rather
// than print a leader from a record that a coin could produce.
func TestExplanationStandingsRefuseALeaderUnderTheFloor(t *testing.T) {
	ctx := context.Background()
	s := duelStore(t)
	for i := 0; i < 4; i++ {
		if err := s.RecordExplanationDuel(ctx, "r"+itoa(i),
			ExplanationDuel{SeedID: 1, WorkID: int64(50 + i), Winner: DuelTags}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ExplanationStandings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Verdict.Leading || rows[0].Verdict.Behind {
		t.Fatalf("4-0 named a leader: %+v", rows[0].Verdict)
	}
	if !rows[0].Verdict.Underpowered {
		t.Fatalf("4-0 was not reported as underpowered: %+v", rows[0].Verdict)
	}
	if rows[0].Verdict.Reason == "" {
		t.Fatal("an underpowered refusal with no reason reads as 'no difference'")
	}
}

// And a clean sweep past the floor does name one, so the refusal above is the floor and not a
// permanent silence.
func TestExplanationStandingsNameALeaderPastTheFloor(t *testing.T) {
	ctx := context.Background()
	s := duelStore(t)
	for i := 0; i < 12; i++ {
		if err := s.RecordExplanationDuel(ctx, "r"+itoa(i),
			ExplanationDuel{SeedID: 1, WorkID: int64(1000 + i), Winner: DuelTags}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ExplanationStandings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].Verdict.Leading {
		t.Fatalf("12-0 did not lead: %+v", rows[0].Verdict)
	}
	if rows[1].Verdict.Leading || !rows[1].Verdict.Behind {
		t.Fatalf("readers at 0-12 = %+v, want Behind", rows[1].Verdict)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
