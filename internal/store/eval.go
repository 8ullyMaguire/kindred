package store

import (
	"context"
	"fmt"
)

// Eval runs: the offline half of the evaluation loop, kept.
//
// `kindred eval` computed recall@K and NDCG@K and then printed them to a
// terminal, which meant the only record of a measurement was whoever happened
// to scroll back. A number that is not stored cannot be compared with the
// number from last week, and comparing it is the entire point of an offline
// arm: the arena tells you what one reader preferred today, and this says
// whether the last change to the weights made the recommender better.
type EvalRun struct {
	ID    int64  `json:"id"`
	RanAt string `json:"ran_at"`
	// K is the depth the metrics were computed at. Stored because recall@10
	// and recall@50 are not comparable, and a table that mixes them is a
	// table that lies by juxtaposition.
	K int `json:"k"`
	// Readers and Bookmarks describe the split.
	Readers   int `json:"readers"`
	Bookmarks int `json:"bookmarks"`
	Evaluated int `json:"evaluated"`
	Failures  int `json:"failures"`
	HeldOut   int `json:"held_out"`
	Hits      int `json:"hits"`
	// Recall, NDCG and Coverage are the report's own numbers, unrounded.
	Recall   float64 `json:"recall_at_k"`
	NDCG     float64 `json:"ndcg_at_k"`
	Coverage float64 `json:"coverage"`
	// PoolDepth, PoolHits and PoolRecall describe the candidate set: how many
	// works the recommender was asked for, how many held-out works were in
	// that list, and the mean per-user share. Zero depth means the run did not
	// measure it.
	PoolDepth  int     `json:"pool_depth,omitempty"`
	PoolHits   int     `json:"pool_hits,omitempty"`
	PoolRecall float64 `json:"pool_recall,omitempty"`
	// TagOverlap and DistinctFandoms are what the ranking BOUGHT and COST:
	// recall says how many held-out works were found, these say whether the
	// list was twelve variations on one fic. Stored per run so a tune that
	// quietly turned diversification off shows up as an overlap rise rather
	// than as an unexplained recall gain. DiversityRuns is how many readers
	// they were averaged over; zero means the run did not measure them, which
	// is NOT the same as zero overlap.
	TagOverlap      float64 `json:"tag_overlap"`
	DistinctFandoms float64 `json:"distinct_fandoms"`
	DiversityRuns   int     `json:"diversity_runs"`
	// Degraded names the signals that skipped for every candidate in this
	// run, comma separated. A run with idf_overlap in this list measured a
	// different recommender from the one serving requests, and the recall
	// number cannot say so by itself.
	Degraded string `json:"degraded,omitempty"`
	// Mode is "full" or "lite". The lite mode skips the embedding signal, so
	// a lite run and a full run measure different recommenders and must not
	// be plotted as one line.
	Mode string `json:"mode"`
}

// RecordEvalRun stores one completed evaluation and returns its id.
func (s *Store) RecordEvalRun(ctx context.Context, r EvalRun) (int64, error) {
	if s == nil || s.DB == nil {
		return 0, fmt.Errorf("no store")
	}
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO eval_runs
			(k, readers, bookmarks, evaluated, failures, held_out, hits,
			 recall_at_k, ndcg_at_k, coverage, mode, degraded,
			 pool_depth, pool_hits, pool_recall,
		 tag_overlap, distinct_fandoms, diversity_runs)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.K, r.Readers, r.Bookmarks, r.Evaluated, r.Failures, r.HeldOut, r.Hits,
		r.Recall, r.NDCG, r.Coverage, r.Mode, r.Degraded,
		r.PoolDepth, r.PoolHits, r.PoolRecall,
		r.TagOverlap, r.DistinctFandoms, r.DiversityRuns)
	if err != nil {
		return 0, fmt.Errorf("record eval run: %w", err)
	}
	return res.LastInsertId()
}

// EvalRuns returns the most recent runs, newest first.
//
// limit is clamped to a sane range rather than trusted: this feeds a page, and
// a query string that asks for a million rows is a way to make the page the
// problem.
func (s *Store) EvalRuns(ctx context.Context, limit int) ([]EvalRun, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("no store")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, ran_at, k, readers, bookmarks, evaluated, failures,
		       held_out, hits, recall_at_k, ndcg_at_k, coverage, mode, degraded,
		       pool_depth, pool_hits, pool_recall,
		       tag_overlap, distinct_fandoms, diversity_runs
		  FROM eval_runs
		 ORDER BY id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("eval runs: %w", err)
	}
	defer rows.Close()

	var out []EvalRun
	for rows.Next() {
		var r EvalRun
		if err := rows.Scan(&r.ID, &r.RanAt, &r.K, &r.Readers, &r.Bookmarks,
			&r.Evaluated, &r.Failures, &r.HeldOut, &r.Hits,
			&r.Recall, &r.NDCG, &r.Coverage, &r.Mode, &r.Degraded,
			&r.PoolDepth, &r.PoolHits, &r.PoolRecall,
			&r.TagOverlap, &r.DistinctFandoms, &r.DiversityRuns); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EvalSlice is one group's share of a run's held-out works.
type EvalSlice struct {
	Axis    string  `json:"axis"`
	Name    string  `json:"name"`
	N       int     `json:"n"`
	Hits    int     `json:"hits"`
	HitRate float64 `json:"hit_rate"`
}

// RecordEvalSlices stores a run's per-group breakdown.
//
// It replaces whatever the run had before rather than appending: a run is one
// measurement, and a second call for the same run is a correction, not a
// second measurement. Recording slices is separate from recording the run
// because the run is the thing worth keeping even if the slicing fails.
func (s *Store) RecordEvalSlices(ctx context.Context, runID int64, slices []EvalSlice) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("no store")
	}
	if runID <= 0 {
		return fmt.Errorf("eval slices: run id %d is not a record", runID)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("eval slices: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM eval_slices WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("eval slices: clear run %d: %w", runID, err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO eval_slices (run_id, axis, name, n, hits)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("eval slices: %w", err)
	}
	defer stmt.Close()
	for _, sl := range slices {
		if sl.Axis == "" || sl.Name == "" {
			return fmt.Errorf("eval slice %+v: axis and name are required", sl)
		}
		if _, err := stmt.ExecContext(ctx, runID, sl.Axis, sl.Name, sl.N, sl.Hits); err != nil {
			return fmt.Errorf("eval slice %s/%s: %w", sl.Axis, sl.Name, err)
		}
	}
	return tx.Commit()
}

// EvalSlices returns a run's per-group breakdown, by axis then hit rate
// descending, so the weakest groups of a fandom axis print last and the page
// can reverse them without re-sorting.
func (s *Store) EvalSlices(ctx context.Context, runID int64) ([]EvalSlice, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("no store")
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT axis, name, n, hits
		  FROM eval_slices
		 WHERE run_id = ?
		 ORDER BY axis, CAST(hits AS REAL) / n DESC, name`, runID)
	if err != nil {
		return nil, fmt.Errorf("eval slices: %w", err)
	}
	defer rows.Close()

	var out []EvalSlice
	for rows.Next() {
		var sl EvalSlice
		if err := rows.Scan(&sl.Axis, &sl.Name, &sl.N, &sl.Hits); err != nil {
			return nil, fmt.Errorf("eval slices: %w", err)
		}
		sl.HitRate = float64(sl.Hits) / float64(sl.N)
		out = append(out, sl)
	}
	return out, rows.Err()
}
