package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// TuneNames lists the stored tune names, alphabetically.
//
// A missing table is an empty list rather than an error: a corpus with no
// tunes stored yet is the normal case, and "no tunes" is not a failure. The
// caller decides what an empty list means.
func (s *Store) TuneNames(ctx context.Context) ([]string, error) {
	if s == nil || s.DB == nil {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT name FROM tunes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Tune loads one stored tune by name.
//
// It returns ErrNotFound for an unknown name rather than falling back to the
// default. `?tune=` is a request for specific weights, and answering it with
// the defaults produces a normal-looking list ranked by something the reader
// did not ask for — indistinguishable from the tune having worked. A named
// tune that cannot be loaded is a visible error; that is the whole contract
// of the parameter.
//
// "default" is not stored and is answered here rather than in the table: it
// is a name that must always work, and it means the built-in weights.
func (s *Store) Tune(ctx context.Context, name string) (map[string]float64, error) {
	if name == "" || name == "default" {
		return nil, ErrNotFound
	}
	if s == nil || s.DB == nil {
		return nil, ErrNotFound
	}
	var weights string
	err := s.DB.QueryRowContext(ctx,
		`SELECT weights FROM tunes WHERE name = ?`, name).Scan(&weights)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	out := map[string]float64{}
	if err := json.Unmarshal([]byte(weights), &out); err != nil {
		// A corrupt row is reported, not swallowed. Silently ranking with
		// the defaults because a JSON blob is malformed is the accepted-
		// and-ignored shape again.
		return nil, fmt.Errorf("tune %q: stored weights are not JSON: %w", name, err)
	}
	return out, nil
}

// SaveTune stores a weight vector under a name, replacing any existing one.
func (s *Store) SaveTune(ctx context.Context, name string, weights map[string]float64) error {
	if name == "" {
		return errors.New("a tune needs a name")
	}
	if s == nil || s.DB == nil {
		return errors.New("no store")
	}
	blob, err := json.Marshal(weights)
	if err != nil {
		return fmt.Errorf("tune %q: %w", name, err)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO tunes (name, weights, updated_at) VALUES (?, ?, datetime('now'))
		 ON CONFLICT(name) DO UPDATE SET weights = excluded.weights,
		                                  updated_at = excluded.updated_at`,
		name, string(blob))
	return err
}
