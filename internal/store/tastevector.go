package store

// Taste export and import (#36).
//
// `arena_user_tag_weights` is the only learned model of a READER in this deployment: it is
// built from the arena's comparisons, it is read by the session_taste signal, and until now it
// could not be read out or written back. A reader's taste was confined to one database, one
// browser cookie and one deployment.
//
// Export is a read: the weights with their evidence counts. Import is a WRITE with a decision
// in it, and the decision is MERGE by default. Replacing the whole vector on every import
// would mean a partial file silently deletes forty comparisons the reader made and never
// mentioned; `Replace` exists for the case where a caller really does mean "this vector, only
// this vector", and it has to be asked for.

import (
	"context"
	"fmt"
)

// SetTagWeight writes an ABSOLUTE weight for one tag, with its evidence count.
//
// The difference from UpsertTagWeight is the whole point of import: upsert folds one more
// observation into a running mean, which is right for the arena and wrong for a file that
// says "this is the vector". The evidence count travels with the weight because the signal
// divides by sqrt(n) -- importing a weight of 0.8 with n=1 must not look like a weight of 0.8
// corroborated forty times.
func (s *Store) SetTagWeight(ctx context.Context, ownerKey string, tagID int64, weight float64, n int) error {
	if ownerKey == "" {
		return fmt.Errorf("set tag weight: no owner key")
	}
	if tagID <= 0 {
		return fmt.Errorf("set tag weight: tag id %d", tagID)
	}
	if n <= 0 {
		// A weight with no evidence is not a weight; the signal reads n to know how much to
		// trust it, and n=0 would make it unreadable by TagWeights (which filters n > 0).
		n = 1
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO arena_user_tag_weights (owner_key, tag_id, weight, n)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(owner_key, tag_id) DO UPDATE SET
		  weight = excluded.weight,
		  n = excluded.n
		WHERE arena_user_tag_weights.n <> excluded.n
		   OR arena_user_tag_weights.weight <> excluded.weight`,
		ownerKey, tagID, weight, n)
	if err != nil {
		return fmt.Errorf("set tag weight: %w", err)
	}
	return nil
}

// ClearTagWeights deletes a reader's whole learned vector and returns how many rows went.
//
// The count is returned because the caller reports it: "imported 12 tags and replaced 40" is a
// different statement from "imported 12 tags", and only the first tells the reader what they
// just lost.
func (s *Store) ClearTagWeights(ctx context.Context, ownerKey string) (int, error) {
	if ownerKey == "" {
		return 0, fmt.Errorf("clear tag weights: no owner key")
	}
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM arena_user_tag_weights WHERE owner_key = ?`, ownerKey)
	if err != nil {
		return 0, fmt.Errorf("clear tag weights: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear tag weights: %w", err)
	}
	return int(n), nil
}

// CountTagWeights is how many tags a reader currently has.
func (s *Store) CountTagWeights(ctx context.Context, ownerKey string) (int, error) {
	if ownerKey == "" {
		return 0, nil
	}
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arena_user_tag_weights WHERE owner_key = ? AND n > 0`,
		ownerKey).Scan(&n)
	if err != nil {
		if isMissingTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("count tag weights: %w", err)
	}
	return n, nil
}

// RebuildTagWeight rebuilds one weight from a set of observations, for imports that carry
// observations rather than a mean.
//
// Not used by the HTTP route (which takes weights) but kept beside its siblings so a future
// caller does not reach for UpsertTagWeight in a loop, which would damp each observation
// against the previous one and land somewhere nobody can predict.
func RebuildTagWeight(observations []float64) (weight float64, n int) {
	for _, o := range observations {
		weight = (weight*float64(n) + o) / float64(n+1)
		n++
	}
	return weight, n
}
