package store

// Visit cursors (#65).
//
// The tests below pin the three claims the feature makes, each of which a
// plausible implementation gets wrong:
//
//   - A visit EXTENDS on activity and does not move its own start. An
//     implementation that moves current_at on every request has a boundary that
//     drifts forward with the reader, so "since your last visit" quietly becomes
//     "since a moment ago" and the list empties.
//   - The gap is measured from the last request, not from the visit's start, so
//     an hour of continuous browsing is ONE visit.
//   - A first visit has no boundary, and says so rather than using zero time.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func visitStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// corpusStore attaches a tiny writable corpus, so NewSince has works to read.
// OpenMemory has none by design (it tests the STATE database).
func corpusStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	corpusPath := filepath.Join(dir, "corpus.db")
	db, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	// The corpus is validated on Open: a database missing works/tags/work_tags/
	// cooccurrence_edges is refused as "not a kindred corpus", which is the
	// check that keeps this store from being pointed at the wrong file. So the
	// fixture writes the real schema rather than a four-column imitation.
	if _, err := db.Exec(testcorpus.Schema); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id    int64
		title string
		seen  string
	}{
		{1, "old", "2026-01-01T00:00:00+00:00"},
		{2, "just after the boundary", "2026-02-01T00:00:00+00:00"},
		{3, "also after", "2026-02-02T00:00:00+00:00"},
		{4, "no date at all", ""},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO works(id,url,title,authors,first_seen) VALUES(?,?,?,?,?)`,
			r.id, "https://example.com/"+r.title, r.title, "author", r.seen); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), filepath.Join(dir, "state.db"), corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAVisitExtendsWithoutMovingItsStart(t *testing.T) {
	ctx := context.Background()
	s := visitStore(t)
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

	first, err := s.NoteVisit(ctx, "owner", t0, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !first.FirstVisit {
		t.Fatal("the first visit is not marked as one: there is no boundary to compare against")
	}

	// Five minutes later, in the same sitting.
	same, err := s.NoteVisit(ctx, "owner", t0.Add(5*time.Minute), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if same.CurrentAt != t0 {
		t.Fatalf("the visit's start moved to %v, want %v: a boundary that drifts "+
			"forward with the reader turns \"since your last visit\" into \"since a "+
			"moment ago\"", same.CurrentAt, t0)
	}
	if same.Visits != 1 {
		t.Fatalf("visits = %d after five minutes, want 1", same.Visits)
	}

	// Forty minutes after the last request: a new visit, and the boundary is the
	// START of the previous one.
	next, err := s.NoteVisit(ctx, "owner", t0.Add(45*time.Minute), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next.Visits != 2 {
		t.Fatalf("visits = %d, want 2", next.Visits)
	}
	if next.FirstVisit {
		t.Fatal("the second visit is marked as a first visit")
	}
	if !next.PreviousAt.Equal(t0) {
		t.Fatalf("previous visit = %v, want %v (the start of the visit before this "+
			"one, not the time of its last request)", next.PreviousAt, t0)
	}
}

// TestLongBrowsingIsOneVisit: the gap is measured from the last REQUEST, so an
// hour of activity is one sitting. Measured from the visit's start it would be
// two, and the reader would lose the list they were reading mid-scroll.
func TestLongBrowsingIsOneVisit(t *testing.T) {
	ctx := context.Background()
	s := visitStore(t)
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	if _, err := s.NoteVisit(ctx, "owner", t0, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	var cur VisitCursor
	for m := 20; m <= 60; m += 20 {
		var err error
		cur, err = s.NoteVisit(ctx, "owner", t0.Add(time.Duration(m)*time.Minute), 30*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
	}
	if cur.Visits != 1 {
		t.Fatalf("an hour of browsing in 20-minute steps counted as %d visits, want 1", cur.Visits)
	}
}

func TestNewSinceListsOnlyWhatArrivedAfterTheBoundary(t *testing.T) {
	ctx := context.Background()
	s := corpusStore(t)
	since := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	works, err := s.NewSince(ctx, "", since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 2 {
		t.Fatalf("got %d works, want 2 (work 2 and work 3; work 1 predates the "+
			"boundary and work 4 has no first_seen at all)", len(works))
	}
	if works[0].ID != 3 {
		t.Fatalf("the newest work is %d, want 3: the list is ordered by first_seen desc",
			works[0].ID)
	}
	for _, w := range works {
		if w.ID == 1 {
			t.Fatal("a work that predates the boundary is in the list")
		}
		if w.ID == 4 {
			t.Fatal("a work with no first_seen is in the list: it cannot be shown to " +
				"have arrived after anything")
		}
		if w.FirstSeen == "" {
			t.Fatalf("work %d has no first_seen in the answer, so the reader cannot "+
				"check the claim", w.ID)
		}
	}
}

// TestNewSinceExcludesWhatTheReaderHasMet: BOTH halves of seen_works. A work the
// reader only scrolled past was still shown to them, and re-offering it is the
// failure this list exists to avoid.
func TestNewSinceExcludesWhatTheReaderHasMet(t *testing.T) {
	ctx := context.Background()
	s := corpusStore(t)
	since := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// Work 3 is marked read; work 2 is only marked SHOWN.
	// Written in the form the PRODUCT writes: `ao3_work:<id>`. A fixture using
	// bare numbers was green against an exclusion that never matched a live row —
	// CAST('ao3_work:3' AS INTEGER) is 0, so every work was "unseen" and the
	// re-offer list looked correct on a deployment where it was inert.
	if err := s.MarkRead(ctx, "owner", WorkEntityID(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO seen_works (owner_key, entity_id, kind, shown_at) VALUES (?,?,?,?)`,
		"owner", WorkEntityID(2), "ao3_work", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	works, err := s.NewSince(ctx, "owner", since, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range works {
		if w.ID == 2 || w.ID == 3 {
			t.Fatalf("work %d is in the list but this reader has already met it "+
				"(2 was shown, 3 was read)", w.ID)
		}
	}
	// And a different owner still sees them, so the exclusion is per-reader and
	// not a global filter that would make the list empty for everybody.
	other, err := s.NewSince(ctx, "someone-else", since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 2 {
		t.Fatalf("another reader sees %d works, want 2: the exclusion must be per-owner",
			len(other))
	}
}

// TestEntityNumericIDAcceptsBothShapes: the prefixed form is what production
// writes and the bare form is what older rows hold; a parser that accepts only
// one silently ignores the other's history.
func TestEntityNumericIDAcceptsBothShapes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"ao3_work:11623035", 11623035, true},
		{"11623035", 11623035, true},
		{"ao3_tag:29", 29, true},
		{"ao3_work:", 0, false},
		{"", 0, false},
		{"nonsense", 0, false},
		{"ao3_work:0", 0, false},
		{"ao3_work:-5", 0, false},
	} {
		got, ok := EntityNumericID(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("EntityNumericID(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestMarksSinceCountsWhatTheReaderDid (#48, the count half).
func TestMarksSinceCountsWhatTheReaderDid(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []int64{1, 2, 3} {
		if err := s.MarkRead(ctx, "owner", WorkEntityID(id)); err != nil {
			t.Fatal(err)
		}
	}
	// A boundary in the future counts nothing: a count that ignores its own
	// boundary would report every mark ever made as "since your last visit".
	n, err := s.MarksSince(ctx, "owner", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("marks since a future boundary = %d, want 0", n)
	}
	// A boundary in the past counts all three.
	n, err = s.MarksSince(ctx, "owner", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("marks since an hour ago = %d, want 3", n)
	}
	// And another reader's marks never appear.
	if n, err := s.MarksSince(ctx, "someone-else", time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("another reader's marks = %d, %v; want 0", n, err)
	}
}
