package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func seenTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestMarkShownIsIdempotentAndRefreshesTheTimestamp pins the two properties
// that make "seen recently" a window rather than a lifetime ban.
//
// Without the refresh, a work a reader keeps being shown keeps its ORIGINAL
// timestamp and silently ages out of the exclusion window — the recommender
// starts repeating itself again and nothing explains why, because the feature
// is on and the history is intact.
func TestMarkShownIsIdempotentAndRefreshesTheTimestamp(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	if err := s.MarkShown(ctx, "reader", []string{"ao3_work:1"}); err != nil {
		t.Fatalf("first sighting: %v", err)
	}
	if _, err := s.DB.Exec(
		`UPDATE seen_works SET shown_at='2001-01-01 00:00:00' WHERE owner_key='reader'`,
	); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	// Age the row so a refresh is observable even at second granularity.
	if err := s.MarkShown(ctx, "reader", []string{"ao3_work:1"}); err != nil {
		t.Fatalf("second sighting: %v", err)
	}

	read, shown, err := s.SeenCount(ctx, "reader")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	// Idempotent: one row, not two.
	if read != 0 || shown != 1 {
		t.Fatalf("after two sightings of the same work: read=%d shown=%d, want 0 and 1",
			read, shown)
	}
	// And the timestamp moved forward, so the window restarts.
	seen, err := s.SeenIDs(ctx, "reader", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("seen ids: %v", err)
	}
	if !seen["ao3_work:1"] {
		t.Fatal("a work shown 30 days ago and shown again just now is excluded by " +
			"a 7-day window: the refresh did not happen")
	}
}

// TestSeenIDsWindowDistinguishesReadFromShown is the core semantic test.
//
// The two sets have different lifetimes, and the reason is that they answer
// different questions. A reader who has READ a work does not want it back ever;
// a reader who was merely SHOWN one wants it back eventually, or a
// recommendation list never evolves.
func TestSeenIDsWindowDistinguishesReadFromShown(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	if err := s.MarkShown(ctx, "reader", []string{"ao3_work:shown"}); err != nil {
		t.Fatalf("mark shown: %v", err)
	}
	if err := s.MarkRead(ctx, "reader", "ao3_work:read"); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	// No window: only the read work is excluded. This is what a reader who has
	// never ticked "hide what I have seen" gets.
	got, err := s.SeenIDs(ctx, "reader", 0)
	if err != nil {
		t.Fatalf("seen ids: %v", err)
	}
	if !got["ao3_work:read"] {
		t.Error("a work the reader has read is not excluded: the durable " +
			"exclusion is missing")
	}
	if got["ao3_work:shown"] {
		t.Error("a merely-shown work is excluded with no window requested: " +
			"the parameter does nothing")
	}

	// With a window, both are excluded.
	got, err = s.SeenIDs(ctx, "reader", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("seen ids with window: %v", err)
	}
	if !got["ao3_work:read"] || !got["ao3_work:shown"] {
		t.Errorf("with a 7-day window both should be excluded, got %v", got)
	}
}

// TestAStaleShowingStopsExcluding is what makes the window a window.
//
// A work shown a month ago and never again has genuinely not been seen lately,
// so suppressing it forever would mean the reader's list shrinks to nothing as
// their history grows. This is the failure mode the window exists to prevent.
func TestAStaleShowingStopsExcluding(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	if err := s.MarkShown(ctx, "reader", []string{"ao3_work:old"}); err != nil {
		t.Fatalf("mark shown: %v", err)
	}
	stale := time.Now().Add(-40 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := s.DB.Exec(
		`UPDATE seen_works SET shown_at = ? WHERE owner_key = 'reader'`, stale); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	got, err := s.SeenIDs(ctx, "reader", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("seen ids: %v", err)
	}
	if got["ao3_work:old"] {
		t.Error("a work shown 40 days ago is still excluded by a 7-day window: " +
			"the exclusion never expires, so a reader's list shrinks forever")
	}

	// ...but a read work never expires, however old.
	if err := s.MarkRead(ctx, "reader", "ao3_work:oldread"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if _, err := s.DB.Exec(
		`UPDATE seen_works SET shown_at = ? WHERE entity_id = 'ao3_work:oldread'`,
		stale); err != nil {
		t.Fatalf("age the row: %v", err)
	}
	got, err = s.SeenIDs(ctx, "reader", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("seen ids: %v", err)
	}
	if !got["ao3_work:oldread"] {
		t.Error("a work the reader has read fell out of the exclusion set: " +
			"reading should not expire")
	}
}

// TestMarkReadWorksWithoutAPriorSighting covers the common path: a reader
// arrives from a link and marks a work read, with no recommendation ever
// having shown it to them.
//
// An upsert, not an update. With a plain UPDATE this is a silent no-op and the
// reader's most deliberate action — "I have read this" — does nothing.
func TestMarkReadWorksWithoutAPriorSighting(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	if err := s.MarkRead(ctx, "reader", "ao3_work:neverrecommended"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	got, err := s.SeenIDs(ctx, "reader", 0)
	if err != nil {
		t.Fatalf("seen ids: %v", err)
	}
	if !got["ao3_work:neverrecommended"] {
		t.Fatal("marking a work read with no prior sighting did nothing: the " +
			"reader marked it and it is still recommended")
	}
}

// TestUnmarkReadReversesMarkRead: a mistake must be undoable. Marking read as
// a one-way door means one misclick permanently removes a work from a
// reader's options, and there is no way back.
func TestUnmarkReadReversesMarkRead(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	if err := s.MarkRead(ctx, "reader", "ao3_work:oops"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := s.UnmarkRead(ctx, "reader", "ao3_work:oops"); err != nil {
		t.Fatalf("unmark read: %v", err)
	}
	got, err := s.SeenIDs(ctx, "reader", 0)
	if err != nil {
		t.Fatalf("seen ids: %v", err)
	}
	if got["ao3_work:oops"] {
		t.Fatal("unmarking did not reverse marking: the work stays excluded")
	}

	// The row survives as a plain sighting rather than vanishing, so the
	// reader's history is not silently rewritten.
	read, shown, err := s.SeenCount(ctx, "reader")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if read != 0 || shown != 1 {
		t.Fatalf("after unmark: read=%d shown=%d, want 0 and 1", read, shown)
	}
}

// TestMarkShownSkipsEmptyAndDuplicateIds guards the primary key.
//
// The table is WITHOUT ROWID with (owner_key, entity_id) as the key, so an
// empty entity_id is a single shared row for every request that ever passed
// one — and two empties in one batch collide. A recommendation list built
// from a missing work id would then poison the reader's entire history.
func TestMarkShownSkipsEmptyAndDuplicateIds(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	err := s.MarkShown(ctx, "reader", []string{
		"ao3_work:1", "", "  ", "ao3_work:1", " ao3_work:2 ",
	})
	if err != nil {
		t.Fatalf("mark shown: %v", err)
	}
	var n int
	if err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM seen_works WHERE owner_key='reader'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("stored %d rows for ids [ao3_work:1, \"\", \"  \", ao3_work:1, \" ao3_work:2 \"], "+
			"want 2: empties are not rows and duplicates collapse", n)
	}
	var blank int
	if err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM seen_works WHERE entity_id = ''`).Scan(&blank); err != nil {
		t.Fatalf("count blanks: %v", err)
	}
	if blank != 0 {
		t.Fatalf("%d rows hold an empty entity_id", blank)
	}
}

// TestMarkShownOnAnEmptyBatchTouchesNothing: a response with no works must not
// write. Not a crash risk — the write is skipped entirely.
func TestMarkShownOnAnEmptyBatchTouchesNothing(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	for _, ids := range [][]string{nil, {}, {"", "  "}} {
		if err := s.MarkShown(ctx, "reader", ids); err != nil {
			t.Fatalf("mark shown %v: %v", ids, err)
		}
	}
	read, shown, err := s.SeenCount(ctx, "reader")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if read != 0 || shown != 0 {
		t.Fatalf("an empty batch wrote rows: read=%d shown=%d", read, shown)
	}
}

// TestReadAndShownHistoriesAreIndependentPerReader: the store is shared by
// every reader on the host, so the owner key has to be the whole scope.
//
// Without it, marking one work read silently removes it for everyone — which
// on a shared deployment means one reader's history censors another's.
func TestReadAndShownHistoriesAreIndependentPerReader(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	if err := s.MarkRead(ctx, "alice", "ao3_work:1"); err != nil {
		t.Fatalf("alice marks read: %v", err)
	}
	if err := s.MarkShown(ctx, "alice", []string{"ao3_work:2"}); err != nil {
		t.Fatalf("alice shown: %v", err)
	}

	for _, reader := range []string{"alice", "bob"} {
		got, err := s.SeenIDs(ctx, reader, 7*24*time.Hour)
		if err != nil {
			t.Fatalf("%s: seen ids: %v", reader, err)
		}
		switch reader {
		case "alice":
			if !got["ao3_work:1"] || !got["ao3_work:2"] {
				t.Errorf("alice: %v, want both works excluded", got)
			}
		case "bob":
			if len(got) != 0 {
				t.Errorf("bob sees %v from alice's history: histories are shared",
					got)
			}
		}
	}
}

// TestForgetSeenClearsOnlyTheCallersHistory: the clear button on a shared
// store must not wipe every other reader.
func TestForgetSeenClearsOnlyTheCallersHistory(t *testing.T) {
	ctx := context.Background()
	s := seenTestStore(t)

	if err := s.MarkShown(ctx, "alice", []string{"ao3_work:1"}); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if err := s.MarkShown(ctx, "bob", []string{"ao3_work:2"}); err != nil {
		t.Fatalf("bob: %v", err)
	}

	n, err := s.ForgetSeen(ctx, "alice")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if n != 1 {
		t.Fatalf("forgetting alice's history removed %d rows, want 1", n)
	}
	// Bob's row must still be there. A shared-store bug shows up as an empty
	// map here, so assert the specific work rather than a count.
	got, err := s.SeenIDs(ctx, "bob", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if !got["ao3_work:2"] {
		t.Fatalf("bob's history is %v after alice cleared hers: clearing "+
			"one reader's history removed another's", got)
	}
}

// TestSeenIDsOnAStoreWithNoSchemaAnswersEmpty: the exclusions are an
// optimisation of quality, not a correctness requirement.
//
// A server pointed at a store predating this migration would otherwise fail
// every /recommend with a SQL error, because the new predicate references a
// table that is not there. Seen work better than no recommendation at all.
func TestSeenIDsOnAStoreWithNoSchemaAnswersEmpty(t *testing.T) {
	ctx := context.Background()
	// Deliberately NOT setup(): no Migrate, so the table is absent.
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer s.Close()
	if _, err := s.DB.Exec(`DROP TABLE IF EXISTS seen_works`); err != nil {
		t.Fatalf("drop seen_works: %v", err)
	}

	got, err := s.SeenIDs(ctx, "reader", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("seen ids on an unmigrated store: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v from an empty store", got)
	}
}

// TestStoreWithNilDBDoesNotPanic: several Deps paths have an Engine without a
// Store (the CLI, tests). A nil-pointer here would be a crash in the ranking
// path rather than a skipped feature.
func TestStoreWithNilDBDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	var s *Store
	got, err := s.SeenIDs(ctx, "reader", time.Hour)
	if err != nil {
		t.Fatalf("seen ids on a nil store: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v from a nil store", got)
	}
	if _, _, err := s.SeenCount(ctx, "reader"); err != nil && err != sql.ErrNoRows {
		t.Fatalf("count on a nil store: %v", err)
	}
}
