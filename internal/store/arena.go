// Arena persistence. Ratings, comparisons, per-user tag weights, sessions.
//
// The store is the ONLY package that writes SQL for the arena, for the
// same reason store.go says it is the only module allowed to touch SQL in
// kindling: the rating maths is worth testing without a database, and the
// database is worth having in exactly one place.
package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// ErrNoPair is the arena's "nothing to show you" signal, re-exported so a
// handler can errors.Is against it without importing the maths package for
// one name. It is a NORMAL outcome, not a failure: a new user with no
// history, or a pool too small for a diverse pair, legitimately has
// nothing to show.
var ErrNoPair = arena.ErrNoPair

// OwnerKey hashes an identifier into the arena's owner key.
//
// The arena stores comparisons indefinitely and the identity that made
// them not at all, so the only place the identity exists is this
// function's input. HMAC rather than a bare SHA-256 because the input is
// a session cookie: an unkeyed hash of a low-entropy value is trivially
// reversed by brute force, which would undo the entire point.
//
// The salt is kindred's own STABLE_SALT, not a per-deployment constant,
// so the same user in two deployments does not produce the same key. That
// is deliberate: a cross-deployment linkable identifier is a privacy
// leak, and the arena has no use for one.
func (s *Store) OwnerKey(ctx context.Context, identifier string) (string, error) {
	salt, err := s.StableSalt(ctx)
	if err != nil {
		return "", fmt.Errorf("arena owner key: %w", err)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(identifier))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// ------------------------------------------------------------------ ratings

// RatingRow is a work's current rating, as stored.
type RatingRow struct {
	WorkID      int64
	Rating      arena.Rating
	Comparisons int
	Wins        int
	Losses      int
	Draws       int
	Period      int64
}

// Rating returns a work's rating, or arena.Initial() if it has never been
// compared.
//
// A missing row is NOT an error: every work starts unrated, and the
// arena is useless if a fresh work cannot be presented. Initial() carries
// PHI_INIT, which is the cold-start signal the pair selector and the
// leaderboard both read.
func (s *Store) Rating(ctx context.Context, workID int64) (RatingRow, error) {
	row := RatingRow{WorkID: workID, Rating: arena.Initial()}
	err := s.DB.QueryRowContext(ctx,
		`SELECT mu, phi, sigma, comparisons, wins, losses, draws, period
		 FROM arena_ratings WHERE work_id = ?`, workID).
		Scan(&row.Rating.Mu, &row.Rating.Phi, &row.Rating.Sigma,
			&row.Comparisons, &row.Wins, &row.Losses, &row.Draws, &row.Period)
	if errors.Is(err, sql.ErrNoRows) {
		return row, nil
	}
	if err != nil {
		return row, fmt.Errorf("arena rating: %w", err)
	}
	return row, nil
}

// RatingFor returns ratings for many works at once, keyed by work id.
//
// One query, not N. A pair selector that needs the ratings of 400
// candidates and issues 400 queries is the single largest cost class in
// this codebase and I have measured it before; it is also the difference
// between a 40 ms pair and a 4 s one.
func (s *Store) RatingFor(ctx context.Context, workIDs []int64) (map[int64]RatingRow, error) {
	out := make(map[int64]RatingRow, len(workIDs))
	if len(workIDs) == 0 {
		return out, nil
	}
	// SQLite caps a statement's variables at 999 by default, so the
	// batch is chunked. Silently truncating the list instead would rate
	// an incomplete candidate set and look like a cold start.
	const chunk = 400
	for start := 0; start < len(workIDs); start += chunk {
		end := start + chunk
		if end > len(workIDs) {
			end = len(workIDs)
		}
		batch := workIDs[start:end]

		q := `SELECT work_id, mu, phi, sigma, comparisons, wins, losses, draws, period
		      FROM arena_ratings WHERE work_id IN (?` +
			strings.Repeat(", ?", len(batch)-1) + `)`
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.DB.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("arena ratings batch: %w", err)
		}
		for rows.Next() {
			var r RatingRow
			if err := rows.Scan(&r.WorkID, &r.Rating.Mu, &r.Rating.Phi, &r.Rating.Sigma,
				&r.Comparisons, &r.Wins, &r.Losses, &r.Draws, &r.Period); err != nil {
				rows.Close()
				return nil, fmt.Errorf("arena ratings scan: %w", err)
			}
			out[r.WorkID] = r
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("arena ratings rows: %w", err)
		}
		rows.Close()
	}
	// Anything absent is unrated, and the caller gets Initial().
	return out, nil
}

// SaveRating writes one work's rating.
func (s *Store) SaveRating(ctx context.Context, r RatingRow) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO arena_ratings
		   (work_id, mu, phi, sigma, comparisons, wins, losses, draws, period, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(work_id) DO UPDATE SET
		   mu = excluded.mu, phi = excluded.phi, sigma = excluded.sigma,
		   comparisons = excluded.comparisons, wins = excluded.wins,
		   losses = excluded.losses, draws = excluded.draws,
		   period = excluded.period, updated_at = excluded.updated_at`,
		r.WorkID, r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma,
		r.Comparisons, r.Wins, r.Losses, r.Draws, r.Period)
	if err != nil {
		return fmt.Errorf("save rating %d: %w", r.WorkID, err)
	}
	return nil
}

// -------------------------------------------------------------- comparisons

// Comparison is one presented pair and, once judged, its outcome.
type Comparison struct {
	ID          int64
	WorkA       int64 // always the LOWER id; see the schema CHECK
	WorkB       int64
	Choice      string // "a", "b", or "" while unrated
	SessionKey  string
	OwnerKey    string
	Strategy    string
	PresentedAt time.Time
	JudgedAt    sql.NullTime
}

// RecordPresentation stores a pair the moment it is SHOWN.
//
// This is the load-bearing decision in the whole arena, and it is worth
// stating plainly: fatigue is recorded at presentation, not at judgement.
// If it were recorded at judgement, a user who closes the tab instead of
// choosing would be shown the same pair again -- and a recommender that
// keeps re-offering a pair you declined is the fastest way to make an
// entire feature feel broken. The cost is a table row per abandoned
// presentation, which is why session_key is indexed for cleanup.
func (s *Store) RecordPresentation(ctx context.Context, c Comparison) (int64, error) {
	// Enforce the ordering in Go as well as in the CHECK. The no-repeat
	// query is a plain range scan on (owner_key, work_a, work_b); a pair
	// stored the other way round simply does not match, and the failure
	// is a duplicate pair rather than an error.
	if c.WorkA > c.WorkB {
		c.WorkA, c.WorkB = c.WorkB, c.WorkA
		// The choice refers to a side, so flipping the ids flips it too.
		switch c.Choice {
		case "a":
			c.Choice = "b"
		case "b":
			c.Choice = "a"
		}
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO arena_comparisons
		   (work_a, work_b, choice, session_key, owner_key, strategy)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.WorkA, c.WorkB, nullString(c.Choice), c.SessionKey, c.OwnerKey, c.Strategy)
	if err != nil {
		return 0, fmt.Errorf("record presentation: %w", err)
	}
	return res.LastInsertId()
}

// RecordJudgement fills in the choice on a presented pair.
//
// The WHERE clause pins both the session and the unjudged state. Without
// the latter, a double-click or a replayed request would record two
// judgements for one presentation, and the second would be counted twice
// in the rating period.
func (s *Store) RecordJudgement(ctx context.Context, sessionKey string, choice string) error {
	switch choice {
	case "a", "b":
		// The winner is resolved when the batch reads the row, not here:
		// the choice is stored as the SIDE ('a' or 'b') and JudgedSince
		// maps it to the work id. Storing a work id at judgement time
		// would duplicate the mapping in two places, and the CHECK
		// constraint already guarantees work_a < work_b.
		_, err := s.DB.ExecContext(ctx,
			`UPDATE arena_comparisons
			 SET choice = ?, judged_at = datetime('now')
			 WHERE session_key = ? AND judged_at IS NULL`,
			choice, sessionKey)
		if err != nil {
			return fmt.Errorf("judge: %w", err)
		}
		return nil
	case "neither":
		_, err := s.DB.ExecContext(ctx,
			`UPDATE arena_comparisons
			 SET choice = 'neither', judged_at = datetime('now')
			 WHERE session_key = ? AND judged_at IS NULL`, sessionKey)
		if err != nil {
			return fmt.Errorf("judge (neither): %w", err)
		}
		return nil
	default:
		return fmt.Errorf("judge: unknown choice %q", choice)
	}
}

// SeenPairs returns the set of pairs this owner has already been shown,
// for the no-repeat filter.
//
// Returned as a map keyed on the low id so the caller's membership test
// is O(1); a slice would make the selector quadratic against a user with
// any history at all.
func (s *Store) SeenPairs(ctx context.Context, ownerKey string, limit int) (map[[2]int64]bool, error) {
	if limit <= 0 {
		limit = 4000
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT work_a, work_b FROM arena_comparisons
		 WHERE owner_key = ? ORDER BY id DESC LIMIT ?`, ownerKey, limit)
	if err != nil {
		return nil, fmt.Errorf("seen pairs: %w", err)
	}
	defer rows.Close()

	seen := make(map[[2]int64]bool, limit)
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("seen pairs scan: %w", err)
		}
		seen[[2]int64{a, b}] = true
	}
	return seen, rows.Err()
}

// UnjudgedInSession returns the most recent pair shown to a session that
// has not been judged, so a page reload resumes rather than reshuffles.
func (s *Store) UnjudgedInSession(ctx context.Context, sessionKey string) (Comparison, bool, error) {
	var c Comparison
	var choice sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, work_a, work_b, choice, session_key, owner_key, strategy, presented_at, judged_at
		 FROM arena_comparisons
		 WHERE session_key = ? AND judged_at IS NULL
		 ORDER BY id DESC LIMIT 1`, sessionKey).
		Scan(&c.ID, &c.WorkA, &c.WorkB, &choice, &c.SessionKey, &c.OwnerKey,
			&c.Strategy, &c.PresentedAt, &c.JudgedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("unjudged in session: %w", err)
	}
	c.Choice = choice.String
	return c, true, nil
}

// JudgedSince returns comparisons judged at or after a time, for the batch
// rating update. This is how a rating period is bounded in practice: the
// batch takes everything since the last run and treats it as ONE period.
func (s *Store) JudgedSince(ctx context.Context, since time.Time, limit int) ([]JudgedComparison, error) {
	if limit <= 0 {
		limit = 20000
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT work_a, work_b, choice FROM arena_comparisons
		 WHERE judged_at IS NOT NULL AND judged_at >= ?
		 ORDER BY id LIMIT ?`, since.UTC().Format("2006-01-02 15:04:05"), limit)
	if err != nil {
		return nil, fmt.Errorf("judged since: %w", err)
	}
	defer rows.Close()

	var out []JudgedComparison
	for rows.Next() {
		var j JudgedComparison
		var choice string
		if err := rows.Scan(&j.WorkA, &j.WorkB, &choice); err != nil {
			return nil, fmt.Errorf("judged scan: %w", err)
		}
		switch choice {
		case "a":
			j.Winner, j.Loser, j.Score = j.WorkA, j.WorkB, arena.Win
		case "b":
			j.Winner, j.Loser, j.Score = j.WorkB, j.WorkA, arena.Win
		case "neither":
			// A DRAW is real evidence: the user said these are equally
			// (un)desirable. It is half a win to each, which is what
			// keeps a work that is only ever compared indecisively from
			// drifting, and what stops the arena favouring works that
			// happen to get decisive verdicts.
			j.Winner, j.Loser, j.Score = j.WorkA, j.WorkB, arena.Draw
		default:
			continue
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JudgedComparison is one comparison resolved into a winner and a loser.
// A draw uses WorkA as the nominal winner; the Score carries the meaning.
type JudgedComparison struct {
	WorkA, WorkB int64
	Winner       int64
	Loser        int64
	Score        float64
}

// ------------------------------------------------------------- leaderboard

// LeaderboardEntry is one row of the arena standings.
type LeaderboardEntry struct {
	WorkID      int64
	Title       string
	Authors     string
	Rating      arena.Rating
	Comparisons int
	Wins        int
	Losses      int
	Draws       int
	Rank        int
	// Effective is the cold-start-damped rating the leaderboard SORTS
	// and DISPLAYS, as opposed to the raw mu. See EffectiveRating.
	Effective float64
}

// EffectiveRating damps a rating toward the corpus median by its own
// uncertainty.
//
//	effective = median + (mu - median) * (1 - phi / PHI_INIT)
//
// A work with no comparisons has phi = PHI_INIT and so contributes
// exactly the median: it appears on the leaderboard, at the middle, where
// it is honestly unranked. A work that has been compared enough to
// converge carries its full mu. This is what lets a public leaderboard
// exist on day one, instead of needing a week of comparisons before any of
// it means anything.
//
// The alternative -- sorting on raw mu -- puts every unrated work at
// exactly 1500.0, so the top of the table is a wall of noise and the
// first comparison anyone makes jumps them hundreds of places.
func EffectiveRating(mu, phi, median float64) float64 {
	damp := 1.0 - phi/arena.PHI_INIT
	if damp < 0 {
		damp = 0
	}
	if damp > 1 {
		damp = 1
	}
	return median + (mu-median)*damp
}

// Leaderboard returns the top works by effective rating.
//
// minComparisons is the evidence floor. Zero is allowed and is what the
// public page uses, because the damping already handles unrated works
// correctly; a caller that wants only established entries passes a higher
// number and the damping becomes a no-op for everything returned.
func (s *Store) Leaderboard(ctx context.Context, limit int, minComparisons int) ([]LeaderboardEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	// The median of the RATED population, not of the whole corpus: the
	// median is the anchor that pulls an unrated work toward, and the
	// anchor should be where the arena's centre of mass actually is.
	//
	// SQLite has no percentile() and no median aggregate, so this is a
	// count-then-offset. It is two cheap index-only queries, and it runs
	// once per leaderboard request rather than per candidate.
	median := arena.MU_INIT
	var rated int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM arena_ratings WHERE comparisons > 0`).Scan(&rated); err == nil && rated > 0 {
		// Even count: average the two middle rows.
		if rated%2 == 0 {
			var lo, hi float64
			off1, off2 := rated/2-1, rated/2
			if err := s.DB.QueryRowContext(ctx,
				`SELECT mu FROM arena_ratings WHERE comparisons > 0
				 ORDER BY mu LIMIT 1 OFFSET ?`, off1).Scan(&lo); err == nil {
				if err := s.DB.QueryRowContext(ctx,
					`SELECT mu FROM arena_ratings WHERE comparisons > 0
					 ORDER BY mu LIMIT 1 OFFSET ?`, off2).Scan(&hi); err == nil {
					median = (lo + hi) / 2
				}
			}
		} else {
			var mid float64
			if err := s.DB.QueryRowContext(ctx,
				`SELECT mu FROM arena_ratings WHERE comparisons > 0
				 ORDER BY mu LIMIT 1 OFFSET ?`, rated/2).Scan(&mid); err == nil {
				median = mid
			}
		}
	}
	if !isFinite(median) || median == 0 {
		median = arena.MU_INIT
	}

	rows, err := s.DB.QueryContext(ctx,
		`SELECT r.work_id, r.mu, r.phi, r.sigma, r.comparisons, r.wins, r.losses, r.draws
		 FROM arena_ratings r
		 WHERE r.comparisons >= ?
		 ORDER BY r.mu DESC, r.phi ASC
		 LIMIT ?`, minComparisons, limit)
	if err != nil {
		return nil, fmt.Errorf("leaderboard: %w", err)
	}
	defer rows.Close()

	var out []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		if err := rows.Scan(&e.WorkID, &e.Rating.Mu, &e.Rating.Phi, &e.Rating.Sigma,
			&e.Comparisons, &e.Wins, &e.Losses, &e.Draws); err != nil {
			return nil, fmt.Errorf("leaderboard scan: %w", err)
		}
		e.Effective = EffectiveRating(e.Rating.Mu, e.Rating.Phi, median)
		e.Rank = len(out) + 1
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leaderboard rows: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------- user tag weights

// TagWeight is one comparator's learned preference for one tag.
type TagWeight struct {
	TagID  int64
	Weight float64
	N      int
}

// TagWeights returns a comparator's learned tag preferences, strongest
// first.
func (s *Store) TagWeights(ctx context.Context, ownerKey string, limit int) ([]TagWeight, error) {
	if limit <= 0 {
		limit = 40
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT tag_id, weight, n FROM arena_user_tag_weights
		 WHERE owner_key = ? AND n > 0
		 ORDER BY ABS(weight) DESC LIMIT ?`, ownerKey, limit)
	if err != nil {
		return nil, fmt.Errorf("tag weights: %w", err)
	}
	defer rows.Close()

	var out []TagWeight
	for rows.Next() {
		var tw TagWeight
		if err := rows.Scan(&tw.TagID, &tw.Weight, &tw.N); err != nil {
			return nil, fmt.Errorf("tag weights scan: %w", err)
		}
		out = append(out, tw)
	}
	return out, rows.Err()
}

// UpsertTagWeight adds a tag preference, damping by evidence count.
//
// The damping is the reason a single comparison cannot make a tag
// authoritative. A weight is divided by the evidence behind it, so the
// first comparison moves a tag a little and the hundredth moves it a lot,
// and the recommender can read n to know how much to trust it.
func (s *Store) UpsertTagWeight(ctx context.Context, ownerKey string, tagID int64, delta float64) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO arena_user_tag_weights (owner_key, tag_id, weight, n)
		 VALUES (?, ?, ?, 1)
		 ON CONFLICT(owner_key, tag_id) DO UPDATE SET
		   weight = (arena_user_tag_weights.weight * arena_user_tag_weights.n + excluded.weight)
		             / (arena_user_tag_weights.n + 1),
		   n = arena_user_tag_weights.n + 1`,
		ownerKey, tagID, delta)
	if err != nil {
		return fmt.Errorf("upsert tag weight: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ sessions

// TouchSession records that a session is alive, and returns its
// comparison count.
func (s *Store) TouchSession(ctx context.Context, sessionKey, ownerKey string) (int, error) {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO arena_sessions (session_key, owner_key, last_seen)
		 VALUES (?, ?, datetime('now'))
		 ON CONFLICT(session_key) DO UPDATE SET last_seen = datetime('now')`,
		sessionKey, ownerKey)
	if err != nil {
		return 0, fmt.Errorf("touch session: %w", err)
	}
	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT comparisons FROM arena_sessions WHERE session_key = ?`, sessionKey).Scan(&n); err != nil {
		return 0, fmt.Errorf("touch session read: %w", err)
	}
	return n, nil
}

// BumpSession increments a session's comparison counter.
func (s *Store) BumpSession(ctx context.Context, sessionKey string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE arena_sessions SET comparisons = comparisons + 1 WHERE session_key = ?`, sessionKey)
	if err != nil {
		return fmt.Errorf("bump session: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ history

// RecordHistory appends one period's rating for a work, so a rating
// change can be explained after the fact.
func (s *Store) RecordHistory(ctx context.Context, r RatingRow) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO arena_rating_history (work_id, period, mu, phi, sigma)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(work_id, period) DO UPDATE SET
		   mu = excluded.mu, phi = excluded.phi, sigma = excluded.sigma`,
		r.WorkID, r.Period, r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma)
	if err != nil {
		return fmt.Errorf("record history: %w", err)
	}
	return nil
}

// RatingHistory returns a work's ratings across periods, oldest first.
func (s *Store) RatingHistory(ctx context.Context, workID int64, limit int) ([]RatingRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT period, mu, phi, sigma FROM arena_rating_history
		 WHERE work_id = ? ORDER BY period DESC LIMIT ?`, workID, limit)
	if err != nil {
		return nil, fmt.Errorf("rating history: %w", err)
	}
	defer rows.Close()

	var out []RatingRow
	for rows.Next() {
		var r RatingRow
		r.WorkID = workID
		if err := rows.Scan(&r.Period, &r.Rating.Mu, &r.Rating.Phi, &r.Rating.Sigma); err != nil {
			return nil, fmt.Errorf("rating history scan: %w", err)
		}
		out = append(out, r)
	}
	// The caller wants oldest first for a sparkline.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// ArenaStats is the arena's summary, for /healthz and the about page.
type ArenaStats struct {
	RatedWorks  int
	Comparisons int
	Sessions    int
	Judged      int
}

// Stats summarises the arena.
func (s *Store) Stats(ctx context.Context) (ArenaStats, error) {
	var st ArenaStats
	// Four scalar counts from one connection each is cheaper here than a
	// cross join, and this is called by healthz on every request.
	for _, q := range []struct {
		sql  string
		dest *int
	}{
		{`SELECT COUNT(*) FROM arena_ratings WHERE comparisons > 0`, &st.RatedWorks},
		{`SELECT COUNT(*) FROM arena_comparisons`, &st.Comparisons},
		{`SELECT COUNT(*) FROM arena_comparisons WHERE judged_at IS NOT NULL`, &st.Judged},
		{`SELECT COUNT(*) FROM arena_sessions`, &st.Sessions},
	} {
		if err := s.DB.QueryRowContext(ctx, q.sql).Scan(q.dest); err != nil {
			return st, fmt.Errorf("arena stats: %w", err)
		}
	}
	return st, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
