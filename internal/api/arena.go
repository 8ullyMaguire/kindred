// The arena service: pairing, judging, and the periodic rating batch.
//
// This is the layer between the HTTP handlers and the two stores. It
// exists so the handlers contain no logic worth testing and this file
// contains no HTTP, which is the only way both can be tested honestly.

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// ArenaService is the arena's behaviour, independent of HTTP.
type ArenaService struct {
	Store *store.Store
	Log   *slog.Logger

	// CandidatePool is how many works to consider per pairing. Large
	// enough that a good pair is nearly always available, small enough
	// that the per-candidate tag fetch stays cheap.
	CandidatePool int
	// Explored is the share of presentations that are exploratory.
	Explored float64
	// Strategy is the default pairing policy.
	Strategy arena.Strategy
}

// NewArenaService returns a service with the defaults that work on a Pi.
func NewArenaService(st *store.Store, log *slog.Logger) *ArenaService {
	return &ArenaService{
		Store:         st,
		Log:           log,
		CandidatePool: 300,
		Explored:      0.15, // a sixth of presentations probe the model's blind spots
		Strategy:      arena.StrategyMaxInfo,
	}
}

// sessionCookie is the cookie holding the arena session. It is a random
// 32 bytes; the arena's privacy guarantee means the value never reaches a
// log line, a snapshot, or the database in the clear.
const sessionCookie = "kindred_arena"

// SessionKey returns the request's arena session, minting one if absent.
//
// The identifier is hashed immediately by Store.OwnerKey, so nothing
// downstream can store it. The cookie itself is opaque and is only ever
// compared against itself.
func (a *ArenaService) SessionKey(w http.ResponseWriter, r *http.Request) (sessionKey, ownerKey string, err error) {
	if c, cerr := r.Cookie(sessionCookie); cerr == nil && c.Value != "" {
		sessionKey = c.Value
	} else {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", "", fmt.Errorf("mint session: %w", err)
		}
		sessionKey = hex.EncodeToString(buf)
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    sessionKey,
			Path:     "/",
			MaxAge:   60 * 60 * 24 * 365,
			HttpOnly: true, // never readable from JavaScript
			SameSite: http.SameSiteLaxMode,
			// Secure is deliberately NOT set: the site is served over
			// plain HTTP on 127.0.0.1 behind a TLS-terminating proxy, and
			// a Secure cookie set on a proxied plain-HTTP response is
			// silently dropped, which would mint a new session on every
			// request and make the arena useless.
		})
	}

	ownerKey, err = a.Store.OwnerKey(r.Context(), sessionKey)
	if err != nil {
		return "", "", err
	}
	return sessionKey, ownerKey, nil
}

// WorkBrief is the minimum needed to show a work in a pair.
type WorkBrief struct {
	ID     int64
	Title  string
	Author string
	Tags   []int64
	// Rating is the Glicko-2 state, for the display.
	Rating arena.Rating
	Rated  bool
	// Summary, for the arena card. Kept short deliberately: the arena
	// shows two works at a time and long summaries defeat the comparison.
	Summary string
}

// PairResult is a presented pair plus the context needed to render it.
type PairResult struct {
	A, B     WorkBrief
	Strategy arena.Strategy
	// ComparisonID identifies the presentation, so a judgement can be
	// attributed to it. The row already exists at this point -- fatigue is
	// recorded at PRESENTATION.
	ComparisonID int64
	// Resumed is true when this pair was left unjudged by a previous
	// request, so the page can say "you still owe me a choice" rather than
	// reshuffling and losing the comparison.
	Resumed bool
}

// Pair presents the next comparison, recording the presentation.
//
// The order here matters. The fatigue row is written BEFORE the pair is
// returned to the caller, so a client that crashes mid-render still
// prevents the same pair coming back. Writing it after would reintroduce
// the repeat-pair bug the whole design is built to avoid.
func (a *ArenaService) Pair(ctx context.Context, w http.ResponseWriter, r *http.Request, tagID int64) (PairResult, error) {
	sessionKey, ownerKey, err := a.SessionKey(w, r)
	if err != nil {
		return PairResult{}, err
	}

	// A reload resumes rather than reshuffles: the user was shown a pair
	// and did not choose, and silently replacing it discards that
	// judgement entirely.
	if c, ok, err := a.Store.UnjudgedInSession(ctx, sessionKey); err != nil {
		return PairResult{}, err
	} else if ok {
		works, err := a.briefs(ctx, []int64{c.WorkA, c.WorkB})
		if err == nil {
			return PairResult{
				A: works[0], B: works[1], Strategy: arena.Strategy(c.Strategy),
				ComparisonID: c.ID, Resumed: true,
			}, nil
		}
		// The works vanished from the corpus under us. Fall through and
		// present a fresh pair rather than erroring: a stale row should
		// not be able to wedge a session.
	}

	cands, err := a.candidates(ctx, tagID)
	if err != nil {
		return PairResult{}, err
	}
	if len(cands) < 2 {
		return PairResult{}, store.ErrNoPair
	}

	seen, err := a.Store.SeenPairs(ctx, ownerKey, 4000)
	if err != nil {
		return PairResult{}, err
	}
	preferred, err := a.Store.TagWeights(ctx, ownerKey, 40)
	if err != nil {
		return PairResult{}, err
	}
	pref := make(map[int64]float64, len(preferred))
	for _, w := range preferred {
		pref[w.TagID] = w.Weight
	}

	strategy := a.Strategy
	if s := r.URL.Query().Get("strategy"); s != "" {
		if cs := arena.Strategy(s); cs.Valid() {
			strategy = cs
		}
	}

	pair, err := arena.ChoosePair(arena.PairRequest{
		Candidates: cands,
		Seen:       seen,
		Preferred:  pref,
		Strategy:   strategy,
		Explored:   a.Explored,
		Rand:       func(n int) int { return mrand.IntN(n) },
	})
	if err != nil {
		return PairResult{}, err
	}

	id, err := a.Store.RecordPresentation(ctx, store.Comparison{
		WorkA: pair.A.ID, WorkB: pair.B.ID,
		SessionKey: sessionKey, OwnerKey: ownerKey,
		Strategy: string(pair.Strategy),
	})
	if err != nil {
		return PairResult{}, err
	}
	if _, err := a.Store.TouchSession(ctx, sessionKey, ownerKey); err != nil {
		return PairResult{}, err
	}

	briefs, err := a.briefs(ctx, []int64{pair.A.ID, pair.B.ID})
	if err != nil {
		return PairResult{}, err
	}
	return PairResult{
		A: briefs[0], B: briefs[1], Strategy: pair.Strategy, ComparisonID: id,
	}, nil
}

// Judge records a choice and learns from it.
//
// The learning happens HERE, immediately, for tag weights, but NOT for
// ratings. That asymmetry is deliberate: a tag preference is a per-user
// running average that is correct to update on every observation, while a
// Glicko rating is a period statistic and updating it per click is the
// documented mistake.
func (a *ArenaService) Judge(ctx context.Context, sessionKey, choice string) error {
	if err := a.Store.RecordJudgement(ctx, sessionKey, choice); err != nil {
		return err
	}
	if choice == "neither" {
		// A draw is a real judgement -- "these are equally uninteresting"
		// -- and it counts toward the rating, but it discriminates on no
		// tag, so there is nothing to learn from it.
		return nil
	}

	// The comparison is now judged; read it back to learn which works it
	// involved. This is the one place a choice is resolved to work ids.
	comp, ok, err := a.Store.LastJudgedInSession(ctx, sessionKey)
	if err != nil || !ok {
		return err
	}
	winner, loser := comp.WorkA, comp.WorkB
	if choice == "b" {
		winner, loser = comp.WorkB, comp.WorkA
	}

	byWork, err := a.tagsFor(ctx, []int64{winner, loser})
	if err != nil {
		return err
	}
	tagCount, total, err := a.tagStats(ctx)
	if err != nil {
		return err
	}
	deltas := arena.LearnTags(byWork, []int64{winner}, []int64{loser}, tagCount, total)

	// Apply in a stable order. Map iteration is random in Go, and while
	// each UPSERT is independent here, a stable order keeps the write
	// sequence reproducible in a log and makes a partial failure
	// reproducible too.
	for _, id := range sortedTagIDs(deltas) {
		if err := a.Store.UpsertTagWeight(ctx, comp.OwnerKey, id, deltas[id]); err != nil {
			return err
		}
	}
	return a.Store.BumpSession(ctx, sessionKey)
}

// candidates returns the eligible pool, with ratings and tags attached.
//
// The pool is drawn from the tag's works when a tag filter is given, and
// otherwise from the most-commented works -- which is the right default
// for an arena, because a work nobody has read cannot win a comparison
// and would only waste a presentation.
func (a *ArenaService) candidates(ctx context.Context, tagID int64) ([]arena.Candidate, error) {
	limit := a.CandidatePool
	if limit <= 0 {
		limit = 300
	}
	ids, err := a.Store.CandidateWorkIDs(ctx, tagID, limit)
	if err != nil {
		return nil, err
	}
	ratings, err := a.Store.RatingFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	tags, err := a.tagsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]arena.Candidate, 0, len(ids))
	for _, id := range ids {
		r := arena.Initial()
		if row, ok := ratings[id]; ok {
			r = row.Rating
		}
		out = append(out, arena.Candidate{ID: id, Rating: r, Tags: tags[id]})
	}
	return out, nil
}

func (a *ArenaService) briefs(ctx context.Context, ids []int64) ([]WorkBrief, error) {
	rows, err := a.Store.Corpus.QueryContext(ctx,
		`SELECT w.id, COALESCE(w.title,''), COALESCE(w.authors,''), COALESCE(w.summary,'')
		 FROM works w WHERE w.id IN (?,?)`, ids[0], ids[1])
	if err != nil {
		return nil, fmt.Errorf("arena briefs: %w", err)
	}
	defer rows.Close()

	tags, err := a.tagsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	ratings, err := a.Store.RatingFor(ctx, ids)
	if err != nil {
		return nil, err
	}

	byID := make(map[int64]WorkBrief, 2)
	for rows.Next() {
		var b WorkBrief
		if err := rows.Scan(&b.ID, &b.Title, &b.Author, &b.Summary); err != nil {
			return nil, fmt.Errorf("arena briefs scan: %w", err)
		}
		byID[b.ID] = b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]WorkBrief, 0, 2)
	for _, id := range ids {
		b, ok := byID[id]
		if !ok {
			// A work id the corpus does not have. Returning a placeholder
			// keeps the page renderable; the alternative is a 500 for a
			// stale arena row.
			b = WorkBrief{ID: id, Title: "(no longer in the corpus)"}
		}
		b.Tags = tags[id]
		if row, ok := ratings[id]; ok {
			b.Rating = row.Rating
		} else {
			b.Rating = arena.Initial()
		}
		out = append(out, b)
	}
	return out, nil
}

func (a *ArenaService) tagsFor(ctx context.Context, workIDs []int64) (map[int64][]int64, error) {
	return a.Store.TagsForWorks(ctx, workIDs)
}

// tagStats returns per-tag corpus frequencies and the total work count, for
// the rarity weighting in tag learning.
func (a *ArenaService) tagStats(ctx context.Context) (map[int64]int, int, error) {
	counts, total, err := a.Store.TagCounts(ctx)
	if err != nil {
		return nil, 0, err
	}
	return counts, total, nil
}

// RunBatch is the periodic rating update. It is the "nightly" in a system
// that has no night: every comparison judged since the last run is treated
// as ONE rating period, per Glickman's batch step.
func (a *ArenaService) RunBatch(ctx context.Context) (BatchResult, error) {
	var res BatchResult

	last, err := a.Store.MetaInt(ctx, "arena_last_period")
	if err != nil {
		return res, err
	}
	// A period is bounded by the last run, not by a fixed duration: the
	// number of games matters more than the wall-clock span, and a fixed
	// window on a quiet night would produce a period of one game -- the
	// regime the batch rule fits worst.
	since := time.Unix(0, 0).UTC()
	if last > 0 {
		since = time.Unix(int64(last), 0).UTC()
	}

	judged, err := a.Store.JudgedSince(ctx, since, 0)
	if err != nil {
		return res, err
	}
	if len(judged) == 0 {
		res.Skipped = "no new comparisons"
		return res, nil
	}

	// Collect every work involved, then take ONE pre-period snapshot. This
	// is what makes the batch order-independent.
	ids := make([]int64, 0, len(judged)*2)
	seen := make(map[int64]bool, len(judged)*2)
	for _, j := range judged {
		for _, id := range []int64{j.Winner, j.Loser} {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	pre, err := a.Store.RatingFor(ctx, ids)
	if err != nil {
		return res, err
	}
	preRatings := make(map[int64]arena.Rating, len(pre))
	for id, row := range pre {
		preRatings[id] = row.Rating
	}

	outcomes := make([]arena.PeriodOutcome, 0, len(judged)*2)
	for _, j := range judged {
		outcomes = append(outcomes,
			arena.PeriodOutcome{Work: j.Winner, Score: j.Score, OppWork: j.Loser})
		// A draw is symmetric, so recording it twice would double-count.
		// The complement is only recorded for a decisive result.
		if j.Score != arena.Draw {
			outcomes = append(outcomes,
				arena.PeriodOutcome{Work: j.Loser, Score: 1.0 - j.Score, OppWork: j.Winner})
		}
	}

	updated := arena.Apply(preRatings, outcomes)
	decayed := arena.DecayIdle(preRatings, seen)
	tally := arena.Tally(outcomes)

	period := int64(last) + 1
	tx, err := a.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("arena batch tx: %w", err)
	}
	// One transaction for the whole period. A partial batch is worse than
	// no batch: half the works updated and half not is a rating system
	// that is internally inconsistent, and the inconsistency is invisible
	// until someone asks why work 4001 moved and work 4002 did not.
	for _, id := range sortedIDs(ids) {
		newR, ok := updated[id]
		if !ok {
			// Did not compete this period, so its rating only decays.
			newR = decayed[id]
		}
		row, hadRow := pre[id]
		base := store.RatingRow{WorkID: id, Rating: arena.Initial()}
		if hadRow {
			base = row
		}
		t := tally[id]
		next := store.RatingRow{
			WorkID: id, Rating: newR, Period: period,
			Comparisons: base.Comparisons + t[0] + t[1] + t[2],
			Wins:        base.Wins + t[0],
			Losses:      base.Losses + t[1],
			Draws:       base.Draws + t[2],
		}
		if _, err := tx.ExecContext(ctx, arenaUpsertRating, next); err != nil {
			tx.Rollback()
			return res, fmt.Errorf("arena batch write %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, arenaUpsertHistory, next); err != nil {
			tx.Rollback()
			return res, fmt.Errorf("arena batch history %d: %w", id, err)
		}
		res.Updated++
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO graph_meta (key, value) VALUES ('arena_last_period', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		fmt.Sprint(period)); err != nil {
		tx.Rollback()
		return res, fmt.Errorf("arena batch period: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("arena batch commit: %w", err)
	}

	res.Period = period
	res.Comparisons = len(judged)
	if a.Log != nil {
		a.Log.Info("arena batch", "period", period, "comparisons", len(judged),
			"works", res.Updated)
	}
	return res, nil
}

// BatchResult reports what a batch run did.
type BatchResult struct {
	Period      int64
	Comparisons int
	Updated     int
	Skipped     string
}

const arenaUpsertRating = `
INSERT INTO arena_ratings
  (work_id, mu, phi, sigma, comparisons, wins, losses, draws, period, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(work_id) DO UPDATE SET
  mu = excluded.mu, phi = excluded.phi, sigma = excluded.sigma,
  comparisons = excluded.comparisons, wins = excluded.wins,
  losses = excluded.losses, draws = excluded.draws,
  period = excluded.period, updated_at = excluded.updated_at`

const arenaUpsertHistory = `
INSERT INTO arena_rating_history (work_id, period, mu, phi, sigma)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(work_id, period) DO UPDATE SET
  mu = excluded.mu, phi = excluded.phi, sigma = excluded.sigma`

func sortedIDs(ids []int64) []int64 {
	out := make([]int64, len(ids))
	copy(out, ids)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func sortedTagIDs(m map[int64]float64) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return sortedIDs(out)
}
