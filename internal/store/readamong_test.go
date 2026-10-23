package store

// "Complete your collection" (#50): how much of an author's shelf a reader has
// already read.
//
// Three claims can each be wrong and each gets a test:
//
//   - a work that was SHOWN is not read (the count would flatter the reader);
//   - another reader's reads are not this reader's (the count would be nonsense);
//   - a work the corpus has but the reader has never met must be counted as
//     unread rather than dropped from the denominator (a "2 of 2" for an author
//     with seven works is the kind of number that ends trust in a page).

import (
	"context"
	"testing"
)

func TestReadAmongCountsReadNotShown(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 1 read, 2 only shown, 3 never seen, 4 read by somebody else.
	if err := s.MarkRead(ctx, "owner", WorkEntityID(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkShown(ctx, "owner", []string{WorkEntityID(2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead(ctx, "somebody-else", WorkEntityID(4)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadAmong(ctx, "owner", []int64{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if !got[1] {
		t.Fatal("a work this reader marked read is missing from the read set")
	}
	if got[2] {
		t.Fatal("a work that was only SHOWN counts as read, so every \"to go\" number on the " +
			"page would be short by exactly the works the site recommended")
	}
	if got[3] {
		t.Fatal("a work the reader has never seen counts as read")
	}
	if got[4] {
		t.Fatal("another reader's read counts as this reader's")
	}
	if len(got) != 1 {
		t.Fatalf("read set = %v, want exactly {1}", got)
	}
}

// TestReadAmongSpansChunks: the chunking exists because SQLite has a marker
// limit; a 401-id call that silently answered only for the first 400 would make
// the last work on a long author page look unread forever.
func TestReadAmongSpansChunks(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ids := make([]int64, 0, 901)
	for i := int64(1); i <= 901; i++ {
		ids = append(ids, i)
	}
	// Mark the first, the 400th, the 401st and the last: one on each side of a
	// chunk boundary.
	for _, id := range []int64{1, 400, 401, 901} {
		if err := s.MarkRead(ctx, "owner", WorkEntityID(id)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ReadAmong(ctx, "owner", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("read set has %d entries, want 4 across three chunks", len(got))
	}
	for _, id := range []int64{1, 400, 401, 901} {
		if !got[id] {
			t.Fatalf("work %d is missing: a chunk boundary dropped it", id)
		}
	}
}

func TestReadAmongWithNoSessionIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.ReadAmong(ctx, "", []int64{1, 2})
	if err != nil {
		t.Fatalf("a reader with no session got an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("read set for an empty owner = %v, want empty", got)
	}
}
