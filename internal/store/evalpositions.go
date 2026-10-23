package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Per-position calibration, stored beside the run it came from.
//
// The run row carries recall, NDCG, coverage and diversity; none of them says how
// far down a list a reader had to go. That is what these rows add: for each
// position 1..k, how many readers' lists reached it and how many of those held a
// work the reader later bookmarked.
//
// Stored per position rather than as a band, because a band is a presentation
// choice: the page shows three bands today and may show five tomorrow, and
// re-running the evaluation to change a label would be absurd.

// EvalPosition is one position's record from one run.
type EvalPosition struct {
	Position int `json:"position"`
	Hits     int `json:"hits"`
	Observed int `json:"observed"`
}

// StoredCalibration is the newest run's per-position record.
//
// OK is false when no run has recorded positions at all — a state that predates
// this table, and which must read as "not calibrated" rather than as "zero".
type StoredCalibration struct {
	RunID     int64          `json:"run_id"`
	K         int            `json:"k"`
	Readers   int            `json:"readers"`
	Positions []EvalPosition `json:"positions"`
	OK        bool           `json:"ok"`
}

// RecordEvalPositions writes one run's per-position rows.
//
// Positions with no observations are skipped rather than stored as 0/0: a row
// saying "position 8: 0 of 0" would be read as a total miss by anything that
// divides without checking, which is exactly the bug the calibration model exists
// to prevent.
func (s *Store) RecordEvalPositions(ctx context.Context, runID int64, positions []EvalPosition) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("no store")
	}
	if runID <= 0 {
		return fmt.Errorf("record eval positions: run id %d", runID)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record eval positions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range positions {
		if p.Observed <= 0 {
			continue
		}
		if p.Hits < 0 || p.Hits > p.Observed {
			return fmt.Errorf("record eval positions: position %d has %d hits of %d observations",
				p.Position, p.Hits, p.Observed)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO eval_positions (run_id, position, hits, observed)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(run_id, position) DO UPDATE SET hits = excluded.hits, observed = excluded.observed`,
			runID, p.Position, p.Hits, p.Observed); err != nil {
			return fmt.Errorf("record eval positions: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record eval positions: %w", err)
	}
	return nil
}

// LatestCalibration returns the newest run's positions, newest run first.
//
// It joins the run row so the reader count and k travel with the positions: a rate
// without its denominator is the thing this whole table exists to avoid.
func (s *Store) LatestCalibration(ctx context.Context) (StoredCalibration, error) {
	if s == nil || s.DB == nil {
		return StoredCalibration{}, fmt.Errorf("no store")
	}
	var out StoredCalibration
	var evaluated int
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, k, readers, evaluated FROM eval_runs
		WHERE id = (SELECT MAX(run_id) FROM eval_positions)`)
	if err := row.Scan(&out.RunID, &out.K, &out.Readers, &evaluated); err != nil {
		// "No recorded positions" is a state; a broken query is not. Folding the
		// two together is how this method first shipped: the column was spelled
		// run_id where eval_runs has id, the query errored, and every deployment
		// honestly reported "not calibrated yet" while the rows sat in the table.
		if errors.Is(err, sql.ErrNoRows) {
			return StoredCalibration{}, nil
		}
		return StoredCalibration{}, fmt.Errorf("latest calibration: %w", err)
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT position, hits, observed FROM eval_positions
		WHERE run_id = ? ORDER BY position`, out.RunID)
	if err != nil {
		return StoredCalibration{}, fmt.Errorf("latest calibration: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p EvalPosition
		if err := rows.Scan(&p.Position, &p.Hits, &p.Observed); err != nil {
			return StoredCalibration{}, fmt.Errorf("latest calibration: %w", err)
		}
		out.Positions = append(out.Positions, p)
	}
	if err := rows.Err(); err != nil {
		return StoredCalibration{}, fmt.Errorf("latest calibration: %w", err)
	}
	// The reader count that travels with the rates is the number of LISTS the
	// positions were observed in, not the number of users in the split. They are
	// different numbers (40 users, 38 evaluated, 37 lists long enough to have a
	// first item) and the sentence quotes this one beside a denominator that was
	// built from it: "37 readers" next to 111 observed positions is checkable,
	// "40 readers" next to 111 is not. Derived from position 1 rather than
	// remembered, for the same reason the roadmap total is.
	if len(out.Positions) > 0 && out.Positions[0].Position == 1 && out.Positions[0].Observed > 0 {
		out.Readers = out.Positions[0].Observed
	} else if evaluated > 0 {
		out.Readers = evaluated
	}
	out.OK = len(out.Positions) > 0
	return out, nil
}
