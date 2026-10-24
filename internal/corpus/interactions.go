package corpus

// Reader interactions, the one table in the mirror holding evidence of what readers
// CHOSE (#89).
//
// `user_work_interactions` is 180,677 rows of `bookmarked` over 6,261 readers and 112,935
// works. It is the only table where a choice is recorded, and until now it was read by
// `internal/collab` (as co-bookmark pairs) and by `internal/dump` (to anonymise it) and
// nothing else.

import (
	"context"
	"fmt"
)

// Interaction is one reader touching one work, with its kind kept so a caller can decide
// what counts. Bookmark is the only kind this mirror records today; the field exists so a
// future kind does not silently join the training set.
type Interaction struct {
	UserID int64
	WorkID int64
	Kind   string
}

// Interactions returns every reader-work interaction, ordered by reader then work so the
// result is deterministic regardless of the table's physical order.
//
// No filtering by count here. A reader with one bookmark teaches the model almost
// nothing, but deciding that is the trainer's business (and a threshold invented here
// would be a number nobody measured); the loader's job is to report what the mirror
// holds.
func (a *AO3) Interactions(ctx context.Context) ([]Interaction, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT user_id, work_id, interaction_type FROM user_work_interactions
		 ORDER BY user_id, work_id`)
	if err != nil {
		return nil, fmt.Errorf("interactions: %w", err)
	}
	defer rows.Close()

	var out []Interaction
	for rows.Next() {
		var it Interaction
		if err := rows.Scan(&it.UserID, &it.WorkID, &it.Kind); err != nil {
			return nil, fmt.Errorf("interactions: scan: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("interactions: rows: %w", err)
	}
	return out, nil
}

// InteractionKinds counts the interactions by kind, so a caller can say what it trained
// on rather than assuming.
func (a *AO3) InteractionKinds(ctx context.Context) (map[string]int, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT interaction_type, COUNT(*) FROM user_work_interactions
		 GROUP BY interaction_type ORDER BY interaction_type`)
	if err != nil {
		return nil, fmt.Errorf("interaction kinds: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, fmt.Errorf("interaction kinds: scan: %w", err)
		}
		out[kind] = n
	}
	return out, rows.Err()
}
