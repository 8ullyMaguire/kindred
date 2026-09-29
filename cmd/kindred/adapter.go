package main

import (
	"context"
	"database/sql"
)

// execAdapter adapts *sql.DB to the graph builder's Executor interface,
// which takes a context. *sql.DB's own Exec has no context parameter, so
// the adapter adds the one the builder wants.
type execAdapter struct{ db *sql.DB }

func (a execAdapter) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return a.db.ExecContext(ctx, query, args...)
}
