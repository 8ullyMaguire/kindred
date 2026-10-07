package store

import (
	"context"
	"strings"
	"time"
)

// SeenWork is one recorded sighting or reading.
type SeenWork struct {
	EntityID string
	Kind     string
	ShownAt  time.Time
	// ReadAt is nil until the reader says they have read it. The distinction
	// is carried all the way to the UI because "stop showing me this" and "I
	// have read this" are different requests: the first is about the
	// recommender repeating itself, the second about the work.
	ReadAt *time.Time
}

// MarkShown records that a reader was shown these entities.
//
// It is a single multi-row INSERT rather than one statement per id because a
// /recommend response is up to 100 works, and 100 statements per request on
// the write path is the difference between a feature that is invisible and
// one that is not.
//
// `shown_at` is REFRESHED on a repeat sighting rather than kept at the first.
// That is what makes "seen in the last 7 days" a useful window instead of a
// lifetime ban: a work a reader keeps being shown has not gone stale, and one
// they have not seen in a month has.
//
// Ids that are empty are skipped. An empty entity_id would otherwise become
// the primary key for a single blank row shared by every request that ever
// passed one.
func (s *Store) MarkShown(ctx context.Context, ownerKey string, ids []string) error {
	ids = cleanEntityIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is a no-op then

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO seen_works (owner_key, entity_id, shown_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT(owner_key, entity_id) DO UPDATE SET shown_at = datetime('now')`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, ownerKey, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkRead records that a reader has read an entity.
//
// Separate from MarkShown on purpose. A work the reader has read is
// permanently excluded; a work merely shown is only stale after a while, and
// keeping the two in one column would force one of those behaviours onto both.
func (s *Store) MarkRead(ctx context.Context, ownerKey, entityID string) error {
	entityID = strings.TrimSpace(entityID)
	if entityID == "" {
		return nil
	}
	// The row is created if absent: marking a work read must work for a work
	// that was never recommended to this reader, which is the common case
	// when they arrive from a link rather than from a recommendation.
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO seen_works (owner_key, entity_id, shown_at, read_at)
		VALUES (?, ?, datetime('now'), datetime('now'))
		ON CONFLICT(owner_key, entity_id) DO UPDATE SET read_at = datetime('now')`,
		ownerKey, entityID)
	return err
}

// UnmarkRead reverses MarkRead, so a reader who marked the wrong work can
// undo it. Without this, marking read is a one-way door and a mistake is
// permanent.
func (s *Store) UnmarkRead(ctx context.Context, ownerKey, entityID string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE seen_works SET read_at = NULL WHERE owner_key = ? AND entity_id = ?`,
		ownerKey, strings.TrimSpace(entityID))
	return err
}

// SeenIDs returns the entity ids to exclude for this reader.
//
// Two sets, because the exclusion rules differ:
//
//   - READ entities are excluded unconditionally. The reader said they read
//     it; that does not expire.
//   - SHOWN entities are excluded only within `within`. The window is the
//     reader's tolerance for repetition, and it is a parameter rather than a
//     constant because "I don't want to see this again today" and "not in a
//     year" are different requests.
//
// `within` of zero disables the shown-based exclusion entirely, which is the
// honest reading of "show me anything": a request that suppressed nothing
// would be a request whose parameter does nothing.
//
// The result is a set, not a list, because the caller binds it as a SQL IN
// list and deduplicating here saves the caller from having to.
func (s *Store) SeenIDs(ctx context.Context, ownerKey string, within time.Duration) (map[string]bool, error) {
	out := map[string]bool{}
	if s == nil || s.DB == nil {
		return out, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT entity_id FROM seen_works
		 WHERE owner_key = ? AND read_at IS NOT NULL`, ownerKey)
	if err != nil {
		// A store predating this table means every /recommend would fail with
		// a SQL error, because seen exclusion is applied in the pool query.
		// The seen list is an improvement to the ranking, never a
		// precondition for producing one: a reader pointed at an un-migrated
		// store should get recommendations, just with repetition.
		if isMissingTable(err) {
			return out, nil
		}
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if within <= 0 {
		return out, nil
	}
	// `-within` is rendered by Go's time formatting rather than computed in
	// SQL, so the store does not depend on SQLite's modifier syntax and the
	// window is testable without a database round trip.
	cutoff := time.Now().Add(-within).UTC().Format("2006-01-02 15:04:05")
	rows, err = s.DB.QueryContext(ctx,
		`SELECT entity_id FROM seen_works
		 WHERE owner_key = ? AND read_at IS NULL AND shown_at >= ?`, ownerKey, cutoff)
	if err != nil {
		if isMissingTable(err) {
			return out, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SeenCount reports how many entities a reader has on record, for the page
// that lets them clear it. Split into read and shown because the two are
// cleared differently: a reader who wants to be able to see a work again is
// unmarking it read, not forgetting that they were shown it.
func (s *Store) SeenCount(ctx context.Context, ownerKey string) (read, shown int, err error) {
	if s == nil || s.DB == nil {
		return 0, 0, nil
	}
	err = s.DB.QueryRowContext(ctx,
		`SELECT
		   COUNT(CASE WHEN read_at IS NOT NULL THEN 1 END),
		   COUNT(CASE WHEN read_at IS NULL     THEN 1 END)
		 FROM seen_works WHERE owner_key = ?`, ownerKey).Scan(&read, &shown)
	return read, shown, err
}

// ForgetSeen clears a reader's history entirely. Destructive and deliberate,
// which is why it is one call behind an explicit route rather than an
// option on the marking calls.
func (s *Store) ForgetSeen(ctx context.Context, ownerKey string) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM seen_works WHERE owner_key = ?`, ownerKey)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// isMissingTable reports whether err is SQLite saying a table does not exist.
//
// Matched on the message rather than a typed error because the driver surfaces
// it as a plain error carrying the SQL text, and the alternative — probing
// sqlite_master on every failure — costs a round trip to answer a question
// whose answer is already in the error.
func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "undefined table")
}

// cleanEntityIDs trims, drops empties and deduplicates.
func cleanEntityIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
