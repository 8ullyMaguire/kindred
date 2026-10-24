package store

// Reading and writing the ALS work factors (#89).

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
)

// SaveWorkFactors replaces the factor table with one row per work.
//
// Replaced, not merged: factors from two training runs are two different coordinate
// systems, and a cosine between a vector from run A and one from run B is meaningless
// even though both are 32 floats. A merge would leave exactly those pairs in the table.
func (s *Store) SaveWorkFactors(ctx context.Context, dim int, factors map[int64][]float32) error {
	if dim <= 0 {
		return fmt.Errorf("save work factors: dim %d", dim)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save work factors: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM work_factors`); err != nil {
		return fmt.Errorf("save work factors: clear: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO work_factors (work_id, dim, vec) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("save work factors: prepare: %w", err)
	}
	defer stmt.Close()
	buf := make([]byte, 4*dim)
	for id, vec := range factors {
		if len(vec) != dim {
			return fmt.Errorf("save work factors: work %d has %d values, want %d",
				id, len(vec), dim)
		}
		for i, v := range vec {
			binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
		}
		if _, err := stmt.ExecContext(ctx, id, dim, buf); err != nil {
			return fmt.Errorf("save work factors: insert %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// LoadWorkFactors reads every factor row into memory.
//
// All of it, because a recommendation needs any candidate's vector and a lazy read per
// candidate would be a query per result row. 112,935 works x 32 float32 is 14 MB, which
// is the reason the width is a parameter rather than a knob to raise casually.
func (s *Store) LoadWorkFactors(ctx context.Context) (dim int, out map[int64][]float32, err error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT work_id, dim, vec FROM work_factors`)
	if err != nil {
		return 0, nil, fmt.Errorf("load work factors: %w", err)
	}
	defer rows.Close()
	out = make(map[int64][]float32, 1024)
	for rows.Next() {
		var id int64
		var d int
		var blob []byte
		if err := rows.Scan(&id, &d, &blob); err != nil {
			return 0, nil, fmt.Errorf("load work factors: scan: %w", err)
		}
		if len(blob) != 4*d {
			return 0, nil, fmt.Errorf("load work factors: work %d has %d bytes for dim %d",
				id, len(blob), d)
		}
		if dim == 0 {
			dim = d
		} else if d != dim {
			// A table holding two widths is a training run that was interrupted mid-write
			// or two runs merged by hand. Either way the model is not one model.
			return 0, nil, fmt.Errorf("load work factors: work %d has dim %d, the table "+
				"already held dim %d", id, d, dim)
		}
		vec := make([]float32, d)
		for i := range vec {
			vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
		}
		out[id] = vec
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("load work factors: rows: %w", err)
	}
	return dim, out, nil
}

// CountWorkFactors reports how many works the table holds, which is what a page states
// when it says the signal is available or degraded.
func (s *Store) CountWorkFactors(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_factors`).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("count work factors: %w", err)
	}
	return n, nil
}
