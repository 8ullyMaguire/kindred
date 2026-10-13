package store

// MirrorHasWork is the single definition of "we already have this work", asked
// by the auto-crawler's skip rule and by discover --skip-known. Its tests are
// about the boundary between "the row exists" and "the row has the metadata we
// want", because that difference is the whole reason it is a predicate and not
// an existence check.

import (
	"context"
	"database/sql"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
	_ "modernc.org/sqlite"
)

// mirrorFixture builds a works table with the three summary shapes the
// predicate has to tell apart.
func mirrorFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(testcorpus.Schema); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id      int
		summary any
	}{
		{1, "a summary"}, // have it
		{2, ""},          // present, but nothing to show
		{3, nil},         // NULL: AO3 works may legitimately have no summary
	} {
		if _, err := db.Exec(
			`INSERT INTO works(id, url, title, authors, summary) VALUES (?,?,?,?,?)`,
			row.id, "https://a/works/x", "title", "author", row.summary); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestMirrorHasWorkNeedsASummaryNotJustARow(t *testing.T) {
	// The boundary that matters. A work row with a NULL or empty summary is
	// exactly what EnqueueIncompleteWorks exists to fill, so treating "the row
	// exists" as known would make that queue a permanent no-op and the mirror
	// would never gain the summaries it is chasing.
	ctx := context.Background()
	db := mirrorFixture(t)

	for _, tc := range []struct {
		id   int64
		want bool
		why  string
	}{
		{1, true, "has a summary"},
		{2, false, "empty summary means nothing to show"},
		{3, false, "NULL summary is a real AO3 work with no summary field"},
		{99, false, "not in the mirror at all"},
	} {
		got, err := MirrorHasWork(ctx, db, tc.id)
		if err != nil {
			t.Errorf("MirrorHasWork(%d): %v", tc.id, err)
			continue
		}
		if got != tc.want {
			t.Errorf("MirrorHasWork(%d) = %v; want %v (%s)", tc.id, got, tc.want, tc.why)
		}
	}
}

func TestMirrorHasWorkTreatsAMissingRowAsAnAnswer(t *testing.T) {
	// sql.ErrNoRows is the answer "we do not have it", not a failure. Returning
	// it as an error would make the worker record a failed attempt -- and after
	// five, park -- every work the mirror legitimately lacks, which is every
	// work the queue exists to fetch.
	ctx := context.Background()
	db := mirrorFixture(t)

	got, err := MirrorHasWork(ctx, db, 12345)
	if err != nil {
		t.Fatalf("a missing row returned an error: %v", err)
	}
	if got {
		t.Error("a missing row reported as known")
	}
}

func TestMirrorHasWorkRejectsNonsenseWithoutTouchingTheDatabase(t *testing.T) {
	// A nil handle and a non-positive id both mean "cannot be known", and both
	// must answer false without a query: the worker calls this with a possibly
	// nil mirror handle and an id that is 0 when the queue URL did not parse.
	ctx := context.Background()
	db := mirrorFixture(t)

	for _, tc := range []struct {
		name string
		q    RowQuerier
		id   int64
	}{
		{"nil querier", nil, 1},
		{"zero id", db, 0},
		{"negative id", db, -5},
		{"nil querier and zero id", nil, 0},
	} {
		got, err := MirrorHasWork(ctx, tc.q, tc.id)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if got {
			t.Errorf("%s reported known", tc.name)
		}
	}
}

func TestMirrorHasWorkSurfacesARealError(t *testing.T) {
	// A locked or unreadable mirror must NOT be papered over as "not present".
	// That difference is re-fetching something we have versus skipping
	// something we never got, and the worker parks a job on the error path
	// rather than silently retrying forever.
	ctx := context.Background()
	db := mirrorFixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := MirrorHasWork(ctx, db, 1); err == nil {
		t.Error("a closed handle returned no error; a broken mirror must not read as an empty one")
	}
}
