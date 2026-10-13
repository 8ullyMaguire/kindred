package store

// One definition of "the mirror already has this work".
//
// It is asked in two places -- the auto-crawler's skip rule, which decides
// whether to make a request at all, and `discover --skip-known`, which decides
// whether to queue a work it found. They must answer the same question the same
// way: if --skip-known used a looser predicate ("the row exists") it would drop
// works the worker would have fetched, and the works would be neither queued nor
// mirrored -- a gap no page or log reports, because nothing failed.
//
// The summary is the criterion rather than the row's existence because that is
// what "metadata we want" means here: a work row with no summary is exactly the
// case `EnqueueIncompleteWorks` exists to fill, so treating it as known would
// make that queue a no-op.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RowQuerier is the single-row read both call sites need. It is satisfied by
// *sql.DB and *sql.Tx, so a caller can pass its own write handle to the mirror
// or the read-only corpus handle without this package deciding which.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// mirrorHasSummarySQL is the whole predicate. Kept as one statement so the
// column and the table are named once.
const mirrorHasSummarySQL = `SELECT summary FROM works WHERE id = ?`

// MirrorHasWork reports whether the mirror holds this work with a non-empty
// summary.
//
// A missing row (sql.ErrNoRows) is false with no error: "we do not have it" is
// the answer, not a failure. Any other error is returned, because a locked or
// unreadable mirror must not be papered over as "not present" -- that is the
// difference between re-fetching something we have and skipping something we
// never got.
func MirrorHasWork(ctx context.Context, q RowQuerier, id int64) (bool, error) {
	if q == nil || id <= 0 {
		return false, nil
	}
	var summary sql.NullString
	err := q.QueryRowContext(ctx, mirrorHasSummarySQL, id).Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mirror check for work %d: %w", id, err)
	}
	return summary.Valid && summary.String != "", nil
}
