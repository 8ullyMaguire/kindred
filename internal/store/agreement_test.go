package store

// Agreement recording reached the store as a finished feature and was none of
// the way finished: the cache map was never made, the owner vector was never
// built, and nothing read the column the whole thing writes. The two tests
// below pin the two defects that had a *user-visible* consequence, because
// those are the ones that must not come back.
//
// The absence of a caller for `BuildOwnerVector` cannot be pinned by a test --
// it is the absence of a call site, not a behaviour -- so it is recorded here
// and in the doc comment on `AgreementFor` instead. A test asserting "some
// caller exists" would be a test of the test suite.

import (
	"context"
	"testing"
)

// A Store with an uninitialised agreement cache must not panic.
//
// `AgreementFor` reads `agreementCache` on its miss path, and a nil-map READ is
// legal in Go -- it returns the zero value. So the uninitialised state looked
// healthy right up to the first WRITE, "assignment to entry in nil map", deep
// inside `RecordJudgement`. That call is reached from `POST /arena/judge`, so
// the defect surfaced as a 500 to a voter after their judgement had already
// been committed to the pair row, and it took out all nine arena integration
// tests. It was reachable because `Store` has three ways to come into existence
// (`Open`, `OpenMemory`, and the zero value) and none of them made the map.
//
// Without the fix in `AgreementFor` this test panics rather than failing, which
// is the honest reproduction: a panic is what a voter got.
func TestAgreementForInitialisesItsCacheOnANilMap(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Re-nil it to reproduce exactly what the constructors used to leave behind.
	s.agreementCache = nil

	// An owner vector with content, so the early return for "no owner vector"
	// is not what answers -- the cache write on the insufficient-evidence path
	// is what is under test.
	ownerVector := map[int64]float64{1: 1.0}

	got, err := s.AgreementFor(ctx, "voter-1", ownerVector)
	if err != nil {
		t.Fatalf("AgreementFor on a nil cache: %v", err)
	}
	// This voter has no arena_user_tag_weights rows at all, so evidence is 0,
	// which is below minEvidence. 0 is the conservative answer: an unproven
	// voter does not move the global signal.
	if got != 0 {
		t.Fatalf("AgreementFor with no evidence rows = %v, want 0", got)
	}
	if s.agreementCache == nil {
		t.Fatal("agreementCache is still nil: the write path did not initialise it")
	}
	// The write is what panicked before, so assert the entry actually landed
	// rather than only that the map exists.
	if _, ok := s.agreementCache["voter-1"]; !ok {
		t.Fatal("no cache entry for voter-1 after AgreementFor returned")
	}
}

// An existing database without `arena_comparisons.agreement` is migrated, and
// its rows survive the rebuild.
//
// This is the failure only operators upgrading ever see: `CREATE TABLE IF NOT
// EXISTS` does nothing to a table that already exists, so the new column
// appeared only on fresh databases while every judgement on an existing one
// failed with "no such column: agreement". The migration rebuilds the table
// from itself; this test builds the OLD shape explicitly and checks the
// migration both adds the column and carries the history across, because a
// rebuild that lost rows would be worse than the error it fixes.
func TestMigrateAddsAgreementToAnExistingArenaComparisons(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Rebuild the table in its pre-migration shape, and put a judged row in it.
	// Taken from the schema before the `agreement` column was added, so this
	// test keeps testing the migration after the schema moves on.
	if _, err := s.DB.ExecContext(ctx, `
		DROP TABLE arena_comparisons;
		CREATE TABLE arena_comparisons (
		  id           INTEGER PRIMARY KEY,
		  work_a       INTEGER NOT NULL,
		  work_b       INTEGER NOT NULL,
		  choice       TEXT,
		  session_key  TEXT NOT NULL,
		  owner_key    TEXT NOT NULL,
		  strategy     TEXT NOT NULL,
		  presented_at TEXT NOT NULL DEFAULT (datetime('now')),
		  judged_at    TEXT,
		  CHECK (work_a < work_b),
		  CHECK (choice IS NULL OR choice IN ('a', 'b', 'neither'))
		);
		INSERT INTO arena_comparisons
		  (id, work_a, work_b, choice, session_key, owner_key, strategy, judged_at)
		  VALUES (7, 10, 20, 'a', 'sess-1', 'owner-1', 'closest', datetime('now'));`); err != nil {
		t.Fatalf("build pre-migration table: %v", err)
	}

	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	has, err := s.columnExists(ctx, "arena_comparisons", "agreement")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("arena_comparisons has no agreement column after migrate")
	}

	// The row survived, kept its judgement, and took the documented default.
	// 1.0 for pre-existing rows is deliberate: they were counted at full weight
	// before the column existed, and a migration must not re-weight history it
	// cannot recompute.
	var workA, workB int64
	var choice string
	var agreement float64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT work_a, work_b, choice, agreement FROM arena_comparisons WHERE id = 7`).
		Scan(&workA, &workB, &choice, &agreement); err != nil {
		t.Fatalf("migrated row is missing or unreadable: %v", err)
	}
	if workA != 10 || workB != 20 || choice != "a" {
		t.Fatalf("migrated row = (work_a=%d, work_b=%d, choice=%q), want (10, 20, \"a\")",
			workA, workB, choice)
	}
	if agreement != 1.0 {
		t.Fatalf("agreement on a pre-existing row = %v, want 1.0", agreement)
	}

	// Migrating again is a no-op, so a restart does not rebuild the table a
	// second time. `migrate` is called on every Open.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arena_comparisons`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("row count after a second migrate = %d, want 1 (the rebuild duplicated rows)", n)
	}

	// The rebuild drops the table and its indexes with it. The schema block's
	// `CREATE INDEX IF NOT EXISTS` has already run by the time `migrate` is
	// called, so if the migration does not recreate them they are gone for
	// good -- and nothing else would notice, because a missing index is a slow
	// query rather than a wrong answer.
	for _, idx := range []string{"idx_arena_pairs", "idx_arena_pending"} {
		var name string
		if err := s.DB.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).
			Scan(&name); err != nil {
			t.Fatalf("index %s missing after the rebuild: %v", idx, err)
		}
	}
}

// The constraints survive the rebuild. They are the reason the columns are
// declared inline rather than inherited: a rebuild that silently dropped
// `CHECK (work_a < work_b)` would let a pair be stored the wrong way round, and
// the no-repeat query depends on the ordering rather than re-checking it.
func TestMigratePreservesTheArenaComparisonsConstraints(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Force the rebuild path by re-creating the pre-migration shape.
	if _, err := s.DB.ExecContext(ctx, `
		DROP TABLE arena_comparisons;
		CREATE TABLE arena_comparisons (
		  id           INTEGER PRIMARY KEY,
		  work_a       INTEGER NOT NULL,
		  work_b       INTEGER NOT NULL,
		  choice       TEXT,
		  session_key  TEXT NOT NULL,
		  owner_key    TEXT NOT NULL,
		  strategy     TEXT NOT NULL,
		  presented_at TEXT NOT NULL DEFAULT (datetime('now')),
		  judged_at    TEXT,
		  CHECK (work_a < work_b),
		  CHECK (choice IS NULL OR choice IN ('a', 'b', 'neither'))
		);`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// A pair stored the wrong way round must still be refused.
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO arena_comparisons (work_a, work_b, session_key, owner_key, strategy)
		 VALUES (20, 10, 's', 'o', 'closest')`); err == nil {
		t.Fatal("the rebuild dropped CHECK (work_a < work_b): a reversed pair was accepted")
	}
	// And an unknown choice must still be refused.
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO arena_comparisons (work_a, work_b, choice, session_key, owner_key, strategy)
		 VALUES (10, 20, 'c', 's', 'o', 'closest')`); err == nil {
		t.Fatal("the rebuild dropped the choice CHECK: an unknown choice was accepted")
	}
}
