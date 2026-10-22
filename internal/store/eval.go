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
			 recall_at_k, ndcg_at_k, coverage, mode)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.K, r.Readers, r.Bookmarks, r.Evaluated, r.Failures, r.HeldOut, r.Hits,
		r.Recall, r.NDCG, r.Coverage, r.Mode)
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
		       held_out, hits, recall_at_k, ndcg_at_k, coverage, mode
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
			&r.Recall, &r.NDCG, &r.Coverage, &r.Mode); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
