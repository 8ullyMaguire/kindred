// SPDX-License-Identifier: AGPL-3.0-only

// Kind-aware arena state: ratings and leaderboards for entities that are not
// works, and the materialised candidate table they are paired from.
//
// Works keep the tables they have always used (`arena_ratings`,
// `arena_rating_history`): the lead of an entity arena is not a reason to
// migrate rows whose readers are working. Everything else lives in
// `arena_entities` / `arena_entity_history`, which the schema has declared since
// the arena was designed for every kind and which nothing wrote to until now.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// LastJudgedInSessionKind is the newest judged comparison for one kind.
//
// The arena's two kinds share one session (one cookie, one browser), so the
// judgement that was just recorded has to be resolved within the kind that
// recorded it: without the filter, judging a tag pair while a work pair was the
// newest row would resolve the tag choice to the work pair's ids, and the
// rating would move the wrong entities -- silently, because both are integers.
func (s *Store) LastJudgedInSessionKind(ctx context.Context, sessionKey string, kind arena.EntityKind) (Comparison, bool, error) {
	if !kind.Valid() {
		kind = arena.KindWork
	}
	var c Comparison
	var choice string
	var kindCol string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, kind, work_a, work_b, choice, session_key, owner_key, strategy
		 FROM arena_comparisons
		 WHERE session_key = ? AND kind = ? AND judged_at IS NOT NULL
		 ORDER BY id DESC LIMIT 1`, sessionKey, string(kind)).
		Scan(&c.ID, &kindCol, &c.WorkA, &c.WorkB, &choice, &c.SessionKey, &c.OwnerKey, &c.Strategy)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("last judged in session (kind %s): %w", kind, err)
	}
	c.Kind = arena.ParseKind(kindCol)
	c.Choice = choice
	return c, true, nil
}

// ------------------------------------------------------------------ ratings

// RatingForKind reads the current ratings of entities of one kind.
//
// Ids with no row are absent from the map rather than present as a zero rating:
// a zero Glicko rating is not a thing, and a caller that cannot tell "unrated"
// from "rated 0" will rank new entities last forever.
func (s *Store) RatingForKind(ctx context.Context, kind arena.EntityKind, entityIDs []int64) (map[int64]RatingRow, error) {
	out := make(map[int64]RatingRow, len(entityIDs))
	if kind == arena.KindWork {
		// Works predate this table; see the package comment above.
		return s.RatingFor(ctx, entityIDs)
	}
	if !kind.Valid() || len(entityIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(entityIDs)+1)
	args = append(args, string(kind))
	for _, id := range entityIDs {
		args = append(args, id)
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT entity_id, mu, phi, sigma, comparisons, wins, losses, draws, period
		 FROM arena_entities
		 WHERE kind = ? AND entity_id IN (`+placeholders(len(entityIDs))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("ratings for kind %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var r RatingRow
		if err := rows.Scan(&r.WorkID, &r.Rating.Mu, &r.Rating.Phi, &r.Rating.Sigma,
			&r.Comparisons, &r.Wins, &r.Losses, &r.Draws, &r.Period); err != nil {
			return nil, fmt.Errorf("scan rating for kind %s: %w", kind, err)
		}
		out[r.WorkID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ratings for kind %s: %w", kind, err)
	}
	return out, nil
}

// SaveRatingKind writes one entity's rating and appends its history row.
//
// The entity id travels in RatingRow.WorkID, whose name is from the work-only
// era; renaming it would touch every caller for no behavioural gain.
func (s *Store) SaveRatingKind(ctx context.Context, kind arena.EntityKind, r RatingRow) error {
	if kind == arena.KindWork {
		if err := s.SaveRating(ctx, r); err != nil {
			return err
		}
		return s.RecordHistory(ctx, r)
	}
	if !kind.Valid() {
		return fmt.Errorf("save rating: %q is not an entity kind", kind)
	}
	if !isFinite(r.Rating.Mu) || !isFinite(r.Rating.Phi) || !isFinite(r.Rating.Sigma) {
		// A NaN here poisons every later comparison of this entity, and
		// SQLite will happily store it. Refusing is the only place this can
		// be caught before it spreads.
		return fmt.Errorf("save rating kind %s: non-finite rating for %d", kind, r.WorkID)
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO arena_entities
		   (kind, entity_id, mu, phi, sigma, comparisons, wins, losses, draws, period, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(kind, entity_id) DO UPDATE SET
		   mu = excluded.mu, phi = excluded.phi, sigma = excluded.sigma,
		   comparisons = excluded.comparisons, wins = excluded.wins,
		   losses = excluded.losses, draws = excluded.draws,
		   period = excluded.period, updated_at = excluded.updated_at`,
		string(kind), r.WorkID, r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma,
		r.Comparisons, r.Wins, r.Losses, r.Draws, r.Period); err != nil {
		return fmt.Errorf("save rating kind %s for %d: %w", kind, r.WorkID, err)
	}
	// OR REPLACE, not INSERT: history is keyed (kind, entity_id, period), and
	// a batch that re-runs a period must overwrite its row rather than fail.
	if _, err := s.DB.ExecContext(ctx,
		`INSERT OR REPLACE INTO arena_entity_history (kind, entity_id, period, mu, phi, sigma)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		string(kind), r.WorkID, r.Period, r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma); err != nil {
		return fmt.Errorf("record history kind %s for %d: %w", kind, r.WorkID, err)
	}
	return nil
}

// RatingHistoryKind returns one entity's rating rows, oldest period first.
func (s *Store) RatingHistoryKind(ctx context.Context, kind arena.EntityKind, entityID int64, limit int) ([]RatingRow, error) {
	if kind == arena.KindWork {
		return s.RatingHistory(ctx, entityID, limit)
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT period, mu, phi, sigma FROM arena_entity_history
		 WHERE kind = ? AND entity_id = ? ORDER BY period DESC LIMIT ?`,
		string(kind), entityID, limit)
	if err != nil {
		return nil, fmt.Errorf("rating history kind %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	var out []RatingRow
	for rows.Next() {
		r := RatingRow{WorkID: entityID}
		if err := rows.Scan(&r.Period, &r.Rating.Mu, &r.Rating.Phi, &r.Rating.Sigma); err != nil {
			return nil, fmt.Errorf("scan rating history kind %s: %w", kind, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rating history kind %s: %w", kind, err)
	}
	// Newest-first in SQL so LIMIT keeps the recent end, oldest-first out so
	// a sparkline reads left to right without the caller reversing it.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ------------------------------------------------------------- leaderboards

// LeaderboardForKind returns the top entities of one kind by effective rating.
//
// Works are delegated, so the work leaderboard's behaviour is untouched down to
// the median query it uses as its anchor. Other kinds read `arena_entities` and
// take their display names from the materialised candidate table: an arena that
// cannot name its entries is a list of integers.
func (s *Store) LeaderboardForKind(ctx context.Context, kind arena.EntityKind, limit, minComparisons int) ([]LeaderboardEntry, error) {
	if kind == arena.KindWork || !kind.Valid() {
		return s.Leaderboard(ctx, limit, minComparisons)
	}
	if limit <= 0 {
		limit = 50
	}

	// The anchor is the median of THIS kind's rated population. Using the
	// works median would pull every tag toward 1500 on a scale whose own
	// centre is somewhere else — the damping exists to be the centre of the
	// population being ranked, not of a different one.
	var rated int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arena_entities WHERE kind = ? AND comparisons > 0`,
		string(kind)).Scan(&rated); err != nil {
		return nil, fmt.Errorf("leaderboard kind %s: rated count: %w", kind, err)
	}
	median := medianOfRated(rated, func(offset int) (float64, bool) {
		var mu float64
		err := s.DB.QueryRowContext(ctx,
			`SELECT mu FROM arena_entities WHERE kind = ? AND comparisons > 0
			 ORDER BY mu, entity_id LIMIT 1 OFFSET ?`, string(kind), offset).Scan(&mu)
		return mu, err == nil
	})

	rows, err := s.DB.QueryContext(ctx,
		`SELECT e.entity_id, e.mu, e.phi, e.sigma, e.comparisons, e.wins, e.losses, e.draws,
		        COALESCE(c.name, '')
		 FROM arena_entities e
		 LEFT JOIN entity_candidates c ON c.kind = e.kind AND c.entity_id = e.entity_id
		 WHERE e.kind = ? AND e.comparisons >= ?
		 ORDER BY e.mu DESC, e.phi ASC
		 LIMIT ?`, string(kind), minComparisons, limit)
	if err != nil {
		return nil, fmt.Errorf("leaderboard kind %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	var out []LeaderboardEntry
	for rows.Next() {
		e := LeaderboardEntry{Kind: kind}
		var name string
		if err := rows.Scan(&e.WorkID, &e.Rating.Mu, &e.Rating.Phi, &e.Rating.Sigma,
			&e.Comparisons, &e.Wins, &e.Losses, &e.Draws, &name); err != nil {
			return nil, fmt.Errorf("leaderboard kind %s scan: %w", kind, err)
		}
		e.Effective = EffectiveRating(e.Rating.Mu, e.Rating.Phi, median)
		// The display name of an entity goes in Title, the field the work
		// leaderboard fills with a work's title: one field, two kinds.
		e.Title = name
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leaderboard kind %s: %w", kind, err)
	}
	return out, nil
}

// medianOfRated is the median of a rated population, given its size and a way to
// read the mu at an offset.
//
// Extracted so the work leaderboard's anchor and an entity leaderboard's anchor
// cannot be computed two slightly different ways — the previous inline version
// averaged the two middle rows for an even count and took the middle row
// otherwise, and a second copy of that rule is a second chance to get it wrong.
// A zero or negative size means "no rated rows": the caller gets MU_INIT, which
// is what the damping anchors to on an empty arena.
func medianOfRated(rated int, muAt func(offset int) (float64, bool)) float64 {
	if rated <= 0 {
		return arena.MU_INIT
	}
	if rated%2 == 0 {
		lo, okLo := muAt(rated/2 - 1)
		hi, okHi := muAt(rated / 2)
		if okLo && okHi {
			return (lo + hi) / 2
		}
		return arena.MU_INIT
	}
	mid, ok := muAt(rated / 2)
	if !ok {
		return arena.MU_INIT
	}
	return mid
}

// ----------------------------------------------------------------- candidates

// EntityCandidateCount is one kind's materialised candidate count.
//
// Every loaded kind appears, including the empty ones: "the mirror has 304
// character tags, none of which clear the evidence floor" is a fact a switcher
// should be able to act on, and it is not the same fact as "nobody has looked".
type EntityCandidateCount struct {
	Kind       arena.EntityKind
	Candidates int
	Refreshed  time.Time
}

// ReplaceEntityCandidates swaps a kind's candidate set in one transaction.
//
// Replace, not merge: a candidate that no longer clears the evidence floor —
// because the crawl found it on fewer works, or the tag was renamed — must leave
// the arena rather than linger as a row nothing refreshed. The refresh marker is
// written in the same transaction, so a crash cannot leave a kind that looks
// loaded and is not.
func (s *Store) ReplaceEntityCandidates(ctx context.Context, kind arena.EntityKind, cands []arena.EntityCandidate) (int, error) {
	if !kind.Valid() {
		return 0, fmt.Errorf("replace candidates: %q is not an entity kind", kind)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("replace candidates %s: %w", kind, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM entity_candidates WHERE kind = ?`, string(kind)); err != nil {
		return 0, fmt.Errorf("replace candidates %s: clear: %w", kind, err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO entity_candidates (kind, entity_id, name, weight) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("replace candidates %s: prepare: %w", kind, err)
	}
	defer func() { _ = stmt.Close() }()
	for _, c := range cands {
		if _, err := stmt.ExecContext(ctx, string(kind), c.ID, c.Name, c.Weight); err != nil {
			return 0, fmt.Errorf("replace candidates %s: insert %d: %w", kind, c.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entity_candidate_refreshes (kind, candidates, refreshed_at)
		 VALUES (?, ?, datetime('now'))
		 ON CONFLICT(kind) DO UPDATE SET
		   candidates = excluded.candidates, refreshed_at = excluded.refreshed_at`,
		string(kind), len(cands)); err != nil {
		return 0, fmt.Errorf("replace candidates %s: mark: %w", kind, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("replace candidates %s: commit: %w", kind, err)
	}
	return len(cands), nil
}

// EntityCandidates returns a random sample of a kind's materialised candidates.
//
// Random rather than by weight, because this feeds a pairing choice and not a
// leaderboard: the pairing step already has its own explore/exploit policy
// (`arena.ChoosePair`), and handing it a pre-sorted head would make "explore" a
// choice between the top 400 tags. The table is the head of the distribution;
// within that head, the sample is uniform.
func (s *Store) EntityCandidates(ctx context.Context, kind arena.EntityKind, limit int) ([]arena.EntityCandidate, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("entity candidates: %q is not an entity kind", kind)
	}
	if limit <= 0 {
		limit = 400
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT entity_id, name, weight FROM entity_candidates
		 WHERE kind = ? ORDER BY RANDOM() LIMIT ?`, string(kind), limit)
	if err != nil {
		return nil, fmt.Errorf("entity candidates %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]arena.EntityCandidate, 0, limit)
	for rows.Next() {
		var c arena.EntityCandidate
		if err := rows.Scan(&c.ID, &c.Name, &c.Weight); err != nil {
			return nil, fmt.Errorf("scan entity candidate %s: %w", kind, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entity candidates %s: %w", kind, err)
	}
	return out, nil
}

// EntityName resolves one entity's display name, if the candidate table knows it.
//
// An entity that has been voted on but has fallen out of the candidate table —
// because a re-crawl found it on fewer works — still has its rating, and its name
// is genuinely unknown rather than empty. The bool says which, because "" is a
// legal-ish name and a caller must not confuse it with "we do not know".
func (s *Store) EntityName(ctx context.Context, kind arena.EntityKind, entityID int64) (string, bool, error) {
	var name string
	err := s.DB.QueryRowContext(ctx,
		`SELECT name FROM entity_candidates WHERE kind = ? AND entity_id = ?`,
		string(kind), entityID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("entity name %s/%d: %w", kind, entityID, err)
	}
	return name, true, nil
}

// KindsOffered returns every kind that has been loaded, with its count.
//
// Ordered by the arena's own kind order so a switcher renders the populated
// kinds first. Empty kinds are included on purpose: see EntityCandidateCount.
func (s *Store) KindsOffered(ctx context.Context) ([]EntityCandidateCount, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT kind, candidates, refreshed_at FROM entity_candidate_refreshes`)
	if err != nil {
		return nil, fmt.Errorf("kinds offered: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byKind := map[arena.EntityKind]EntityCandidateCount{}
	for rows.Next() {
		var raw string
		var c EntityCandidateCount
		var refreshed string
		if err := rows.Scan(&raw, &c.Candidates, &refreshed); err != nil {
			return nil, fmt.Errorf("scan kinds offered: %w", err)
		}
		c.Kind = arena.ParseKind(raw)
		if t, err := parseSQLiteTime(refreshed); err == nil {
			c.Refreshed = t
		}
		byKind[c.Kind] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kinds offered: %w", err)
	}

	out := make([]EntityCandidateCount, 0, len(byKind))
	for _, k := range arena.Kinds {
		if c, ok := byKind[k]; ok {
			out = append(out, c)
			delete(byKind, k)
		}
	}
	// A kind in the table that the allowlist no longer names is reported last
	// rather than dropped: the rows exist, and hiding them would make the
	// table impossible to reason about from the outside.
	rest := make([]EntityCandidateCount, 0, len(byKind))
	for _, c := range byKind {
		rest = append(rest, c)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Kind < rest[j].Kind })
	return append(out, rest...), nil
}

// EnsureEntityCandidates loads a kind's candidates if they are missing or stale.
//
// Returns true when it loaded. The staleness marker is written even for an empty
// result, which is the whole reason entity_candidate_refreshes exists: without
// it, a kind the mirror has almost none of would re-run a 2.2s query on every
// page view, forever, and the Switcher would show "unknown" instead of "none".
func (s *Store) EnsureEntityCandidates(ctx context.Context, kind arena.EntityKind, src arena.EntityCandidateSource, limit int, ttl time.Duration) (bool, error) {
	if src == nil {
		return false, errors.New("ensure entity candidates: no source")
	}
	if !kind.Valid() {
		return false, fmt.Errorf("ensure entity candidates: %q is not an entity kind", kind)
	}
	fresh, err := s.entityCandidatesFresh(ctx, kind, ttl)
	if err != nil {
		return false, err
	}
	if fresh {
		return false, nil
	}
	cands, err := src.EntityCandidates(ctx, kind, limit)
	if err != nil {
		return false, fmt.Errorf("ensure entity candidates %s: load: %w", kind, err)
	}
	if _, err := s.ReplaceEntityCandidates(ctx, kind, cands); err != nil {
		return false, err
	}
	return true, nil
}

// entityCandidatesFresh reports whether a kind was loaded within the TTL. A TTL
// of zero or less always refreshes.
func (s *Store) entityCandidatesFresh(ctx context.Context, kind arena.EntityKind, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, nil
	}
	var refreshed string
	err := s.DB.QueryRowContext(ctx,
		`SELECT refreshed_at FROM entity_candidate_refreshes WHERE kind = ?`, string(kind)).Scan(&refreshed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("candidate freshness %s: %w", kind, err)
	}
	t, err := parseSQLiteTime(refreshed)
	if err != nil {
		// An unparsable timestamp means we cannot show the load is fresh.
		// Reloading is the safe reading of "unknown".
		return false, nil
	}
	return time.Since(t) < ttl, nil
}

// placeholders builds "?, ?, ?" for an IN clause. Named for what it returns, so
// a call site reads as SQL rather than as string building.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*3)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		b = append(b, '?')
	}
	return string(b)
}
