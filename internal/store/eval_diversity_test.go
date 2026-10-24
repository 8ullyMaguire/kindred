package store

// The diversity columns in eval_runs.
//
// Three claims, each a way a stored number could lie:
//
//   - the values round-trip (a zero written and read back is indistinguishable
//     from a column that was never written);
//   - diversity_runs is stored, so "not measured" survives the round trip — a
//     run recorded before the columns existed reads 0, and the page renders that
//     as unmeasured rather than as a perfectly diverse list;
//   - the migration adds the columns to a table that predates them, which is the
//     only path an existing deployment takes.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// makeCorpus writes an empty corpus DB: Open reads the corpus, and a missing
// file is a failure rather than a fresh empty mirror.
func makeCorpus(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(testcorpus.Schema); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func evalStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	corpus := filepath.Join(dir, "corpus.db")
	makeCorpus(t, corpus)
	s, err := Open(ctx, filepath.Join(dir, "state.db"), corpus)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTheDiversityColumnsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := evalStore(t)

	id, err := s.RecordEvalRun(ctx, EvalRun{
		K: 10, Readers: 40, Evaluated: 38, HeldOut: 300, Hits: 12,
		Recall: 0.04, NDCG: 0.11, Coverage: 0.3, Mode: "full",
		Degraded:   "all",
		TagOverlap: 0.2875, DistinctFandoms: 3.5, DiversityRuns: 38,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	runs, err := s.EvalRuns(ctx, 5)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got *EvalRun
	for i := range runs {
		if runs[i].ID == id {
			got = &runs[i]
		}
	}
	if got == nil {
		t.Fatalf("run %d not found among %d runs", id, len(runs))
	}
	if got.TagOverlap != 0.2875 {
		t.Fatalf("tag_overlap = %v, want 0.2875", got.TagOverlap)
	}
	if got.DistinctFandoms != 3.5 {
		t.Fatalf("distinct_fandoms = %v, want 3.5", got.DistinctFandoms)
	}
	if got.DiversityRuns != 38 {
		t.Fatalf("diversity_runs = %d, want 38 — without it a stored 0.0 overlap "+
			"cannot be told apart from a run that never measured", got.DiversityRuns)
	}
}

func TestARunThatMeasuredNothingStoresZeroRunsNotZeroOverlapOnly(t *testing.T) {
	ctx := context.Background()
	s := evalStore(t)

	id, err := s.RecordEvalRun(ctx, EvalRun{
		K: 10, Readers: 5, Evaluated: 5, Mode: "full", Degraded: "all",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	runs, err := s.EvalRuns(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.ID != id {
			continue
		}
		if r.DiversityRuns != 0 {
			t.Fatalf("a run with no measurement reports %d measured readers", r.DiversityRuns)
		}
		if r.TagOverlap != 0 || r.DistinctFandoms != 0 {
			t.Fatalf("an unmeasured run carries numbers: %+v", r)
		}
	}
}

// The migration path: a table created without the columns must gain them, and the
// existing rows must read as unmeasured rather than as a claim.
func TestTheMigrationAddsTheColumnsToAnOlderTable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus.db")
	makeCorpus(t, corpus)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	// The shape eval_runs had before the diversity columns.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE eval_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ran_at TEXT NOT NULL DEFAULT (datetime('now')),
			k INTEGER NOT NULL, readers INTEGER NOT NULL, bookmarks INTEGER NOT NULL,
			evaluated INTEGER NOT NULL, failures INTEGER NOT NULL DEFAULT 0,
			held_out INTEGER NOT NULL, hits INTEGER NOT NULL,
			recall_at_k REAL NOT NULL, ndcg_at_k REAL NOT NULL,
			coverage REAL NOT NULL DEFAULT 0, mode TEXT NOT NULL DEFAULT 'full',
			degraded TEXT NOT NULL DEFAULT '',
			pool_depth INTEGER NOT NULL DEFAULT 0, pool_hits INTEGER NOT NULL DEFAULT 0,
			pool_recall REAL NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO eval_runs (k, readers, bookmarks, evaluated, held_out, hits,
		   recall_at_k, ndcg_at_k) VALUES (10, 1, 2, 1, 2, 1, 0.5, 0.5)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(ctx, filepath.Join(dir, "state.db"), corpus)
	if err != nil {
		t.Fatalf("open over an old table: %v", err)
	}
	defer s.Close()
	runs, err := s.EvalRuns(ctx, 5)
	if err != nil {
		t.Fatalf("read after migration: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("read %d runs, want the one pre-existing row", len(runs))
	}
	if runs[0].DiversityRuns != 0 {
		t.Fatalf("a legacy row reports %d measured readers; it must read as unmeasured",
			runs[0].DiversityRuns)
	}
}

// The tune must round-trip, and a run from before the column must read "" -- not
// "default". The difference matters: "" says the run did not RECORD what it
// measured, which is true; "default" would be a claim about a run nobody can check.
func TestTheTuneRoundTripsAndOlderRunsReadEmpty(t *testing.T) {
	s := evalStore(t)
	ctx := context.Background()
	id, err := s.RecordEvalRun(ctx, EvalRun{
		K: 10, Readers: 40, Bookmarks: 900, Evaluated: 37, HeldOut: 370, Hits: 39,
		Recall: 0.1063, NDCG: 0.1888, Coverage: 0.6579, Mode: "full",
		Tune: "walk25", LengthRuns: 37, LengthFit: 0.713,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.EvalRuns(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != id {
		t.Fatalf("expected the run just written, got %+v", runs)
	}
	if runs[0].Tune != "walk25" {
		t.Fatalf("tune = %q, want walk25: an ablation that cannot be attributed is "+
			"four rows of numbers", runs[0].Tune)
	}
	// A row written before the column existed.
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO eval_runs (k, readers, bookmarks, evaluated, failures, held_out,
			hits, recall_at_k, ndcg_at_k, coverage, mode, degraded, pool_depth,
			pool_hits, pool_recall, tag_overlap, distinct_fandoms, diversity_runs,
			length_fit, length_runs)
		 VALUES (10, 1, 1, 1, 0, 1, 0, 0, 0, 0, 'full', '', 0, 0, 0, 0, 0, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	runs, err = s.EvalRuns(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Tune != "" {
		t.Fatalf("an unattributed run reads %q, want the empty string", runs[0].Tune)
	}
}
