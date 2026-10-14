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
	"math"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
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
// EntityBrief is one side of a comparison in an arena that is not about works.
//
// Two fields rather than one shape for both: an entity is not a work with an
// empty title, and a page that renders a tag's name in a work's card is a page
// that lies about what it is showing.
type EntityBrief struct {
	ID     int64
	Name   string
	Kind   arena.EntityKind
	Rating arena.Rating
	Rated  bool
	Counts int
}

type PairResult struct {
	A, B     WorkBrief
	Strategy arena.Strategy
	// Kind is the arena this pair belongs to. Works is what an unset kind
	// has always meant, so a caller that predates kinds sees the work arena.
	Kind arena.EntityKind
	// EntityA/EntityB carry the two sides when Kind is not works, leaving the
	// work briefs zero. Which pair of fields is populated says what the pair
	// is; no field ever holds an entity dressed as a work.
	EntityA, EntityB EntityBrief
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
// ArenaKind resolves the request's arena kind.
//
// An absent parameter means works: that is what every existing caller, bookmark
// and curl invocation asks for, and it is what the arena has always been. A
// parameter that is present but unknown is an ERROR rather than a silent
// fallback, because "you asked for the tag arena and I showed you works" is a
// wrong answer that looks like a right one.
func ArenaKind(r *http.Request) (arena.EntityKind, error) {
	raw := r.URL.Query().Get("kind")
	if raw == "" {
		return arena.KindWork, nil
	}
	return ParseKind(raw)
}

// ParseKind validates a kind named in a request, from a query or a path segment.
// One function because a refusal that only one surface performs is a rule the next
// surface will not have.
func ParseKind(raw string) (arena.EntityKind, error) {
	k := arena.EntityKind(raw)
	if !k.Valid() {
		return "", fmt.Errorf("unknown arena kind %q (want one of %v)", raw, arena.Kinds)
	}
	return k, nil
}

// entityCandidateTTL is how long a materialised candidate set is trusted.
//
// A day: the corpus grows by a crawl, not by the second, and the set is the head
// of a distribution that does not reorder quickly. Set through the same kind of
// knob as CandidatePool so a test can force a reload.
var entityCandidateTTL = 24 * time.Hour

func (a *ArenaService) Pair(ctx context.Context, w http.ResponseWriter, r *http.Request, tagID int64) (PairResult, error) {
	kind, err := ArenaKind(r)
	if err != nil {
		return PairResult{}, err
	}
	if kind != arena.KindWork {
		return a.pairEntities(ctx, w, r, kind)
	}
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
				Kind: arena.KindWork, ComparisonID: c.ID, Resumed: true,
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
		A: briefs[0], B: briefs[1], Strategy: pair.Strategy, Kind: arena.KindWork,
		ComparisonID: id,
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
	return a.JudgeKind(ctx, sessionKey, choice, arena.KindWork)
}

// JudgeKind records one choice in one arena.
//
// Tag learning happens only for works. A tag-vs-tag vote feeding the same
// weights that select works would be circular -- the plan's D4 -- and ratings do
// not move here at all: they are a period statistic, and updating one per click
// is the documented mistake this service already avoids for works.
func (a *ArenaService) JudgeKind(ctx context.Context, sessionKey, choice string, kind arena.EntityKind) error {
	if err := a.Store.RecordJudgementKind(ctx, sessionKey, choice, kind); err != nil {
		return err
	}
	if kind != arena.KindWork {
		return nil
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

// agreeFloor is the influence below which a comparison is dropped from the
// rating period entirely, rather than counted at a small weight. It sits on
// the INFLUENCE (agreement squared), so it excludes voters whose taste
// cosine is below about 0.22.
const agreeFloor = 0.05

// influenceOf turns a voter's taste agreement with the owner into that
// comparison's influence on the ratings, per the owner-weighted plan:
// similar taste counts in full, mild disagreement is dampened quadratically,
// opposite taste counts for nothing (the floor then drops it).
//
// It is a package-level function rather than a closure inside RunBatch so
// that the curve is testable: it is a hand-chosen constant policy, and the
// only thing standing between "a deliberate choice" and "a number nobody
// remembers changing" is a test that pins the shape.
func influenceOf(agree float64) float64 {
	if agree <= 0 {
		// A cosine of zero or below is no agreement at all.
		return 0
	}
	return agree * agree
}

// RunBatch is the periodic rating update. It is the "nightly" in a system
// that has no night: every comparison judged since the last run is treated
// as ONE rating period, per Glickman's batch step.
// RunBatch runs one rating period per arena.
//
// The kinds are independent populations with independent watermarks: a tag
// comparison says nothing about a work's rating, and a single shared watermark
// would let one kind's quiet period discard another kind's rows. KindWork runs
// first and its keys are the bare ones it has always used, so an install that has
// been running keeps its position instead of re-folding its whole history.
func (a *ArenaService) RunBatch(ctx context.Context) (BatchResult, error) {
	res := BatchResult{ByKind: map[string]int64{}}
	var skipped []string
	kinds := []arena.EntityKind{arena.KindWork}
	for _, k := range arena.Kinds {
		if k != arena.KindWork {
			kinds = append(kinds, k)
		}
	}
	for _, kind := range kinds {
		one, err := a.runKindBatch(ctx, kind)
		if err != nil {
			// Each kind is its own transaction, so a failure here leaves the
			// others committed. The error names the kind: a batch that is
			// behind is a fact about one arena, not about the batch.
			return res, fmt.Errorf("%s arena batch: %w", kind, err)
		}
		res.Updated += one.Updated
		res.Comparisons += one.Comparisons
		res.ByKind[string(kind)] = one.Period
		if one.Skipped != "" {
			// "No new comparisons" for an entity kind is the ordinary state
			// of the world -- nothing was judged in that arena -- and is not
			// news in a report. A kind that HAD rows it could not use is.
			// The work arena always speaks, so a run with nothing anywhere
			// still reports the message callers have always read.
			if kind != arena.KindWork && one.Skipped == "no new comparisons" {
				continue
			}
			if kind == arena.KindWork {
				skipped = append(skipped, one.Skipped)
			} else {
				skipped = append(skipped, string(kind)+": "+one.Skipped)
			}
			continue
		}
		if kind == arena.KindWork {
			res.Period = one.Period
		}
	}
	// Skipped means the batch did NOTHING. A kind that had a quiet period is
	// normal and is not reported: a run that rated works and found no new tag
	// comparisons has run, and saying otherwise would read as a no-op.
	if res.Updated == 0 && len(skipped) > 0 {
		res.Skipped = strings.Join(skipped, "; ")
	}
	return res, nil
}

// runKindBatch runs one arena's period.
func (a *ArenaService) runKindBatch(ctx context.Context, kind arena.EntityKind) (BatchResult, error) {
	var res BatchResult
	// arena_last_period is a WATERMARK, and it has to be a timestamp for
	// JudgedSince to work at all. It is written as the period counter
	// (1, 2, 3...) further down, which read back as time.Unix(1, 0) --
	// 1970-01-01T00:00:01Z. Every batch therefore folded the ENTIRE
	// history again, so period 2 double-counted period 1 and a rating
	// moved further with every run that touched it.
	//
	// A rating period is the set of games since the last one; that is a
	// statement about time, and the period NUMBER is derived from it
	// afterwards. The two are different quantities and the table stores
	// the time.
	lastRun, err := a.Store.MetaInt(ctx, kindMeta(kind, "arena_last_run"))
	if err != nil {
		return res, err
	}
	since := time.Unix(0, 0).UTC()
	if lastRun > 0 {
		since = time.Unix(int64(lastRun), 0).UTC()
	}

	judged, err := a.Store.JudgedAfterKind(ctx, kind, since, 0)
	if err != nil {
		return res, err
	}
	if len(judged) == 0 {
		res.Skipped = "no new comparisons"
		return res, nil
	}

	// No fence is needed here: the rows arriving are already this kind's
	// (JudgedAfterKind), each kind has its own watermark, and each kind writes to
	// its own tables. The fence that used to live here was the interim while the
	// per-kind period did not exist -- it stalled the whole batch rather than let
	// an entity id be rated as a work.

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
	pre, err := a.kindRatingsFor(ctx, kind, ids)
	if err != nil {
		return res, err
	}
	preRatings := make(map[int64]arena.Rating, len(pre))
	for id, row := range pre {
		preRatings[id] = row.Rating
	}

	// Influence per the owner-weighted plan: similar taste counts in
	// full, mild disagreement is dampened quadratically, opposite taste
	// counts for nothing.
	//
	//	agree  0.00  0.22  0.50  0.71  1.00
	//	w      0.00  0.05  0.25  0.50  1.00
	//
	// The floor is what makes "opposite taste" mean MINIMAL change rather
	// than a change in the other direction. Below it the outcome is
	// dropped from the period entirely.
	//
	// Dropping it, not weighting it zero: Weight 0 means UNSET in the
	// rating maths (see arena.weightOf), so a zero-weight opponent would
	// be counted at FULL influence -- exactly backwards. That is why the
	// skip below is a `continue` and not a clamp.
	outcomes := make([]arena.PeriodOutcome, 0, len(judged)*2)
	for _, j := range judged {
		w := influenceOf(j.Agreement)
		if w < agreeFloor {
			continue
		}
		outcomes = append(outcomes,
			arena.PeriodOutcome{Work: j.Winner, Score: j.Score, OppWork: j.Loser, Weight: w})
		// A draw is symmetric, so recording it twice would double-count.
		// The complement is only recorded for a decisive result.
		if j.Score != arena.Draw {
			outcomes = append(outcomes,
				arena.PeriodOutcome{Work: j.Loser, Score: 1.0 - j.Score, OppWork: j.Winner, Weight: w})
		}
	}
	if len(outcomes) == 0 {
		// Every comparison in the period came from a voter whose taste
		// disagrees with the owner's. The batch has nothing to learn
		// from, and saying so is better than running an empty period:
		// an empty period would advance the watermark and the period
		// counter, permanently discarding these rows' influence should
		// the owner's vector later change.
		res.Skipped = "no influential comparisons"
		return res, nil
	}

	updated := arena.Apply(preRatings, outcomes)
	decayed := arena.DecayIdle(preRatings, seen)
	tally := arena.Tally(outcomes)

	// The period NUMBER is a counter, and is unrelated to the watermark
	// time above. Reading one as the other is the bug that made every
	// batch re-fold the whole history.
	period, err := a.Store.MetaInt(ctx, kindMeta(kind, "arena_period"))
	if err != nil {
		return res, err
	}
	period++
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
			WorkID: id, Rating: newR, Period: int64(period),
			Comparisons: base.Comparisons + t[0] + t[1] + t[2],
			Wins:        base.Wins + t[0],
			Losses:      base.Losses + t[1],
			Draws:       base.Draws + t[2],
		}
		// The args are passed positionally, NOT as the RatingRow struct.
		// database/sql has no way to marshal a struct, so a single struct
		// argument is rejected by the driver before the statement is ever
		// prepared -- and it failed the whole batch, every run, with a
		// 500 that no test hit because the tests exercised the arithmetic
		// rather than the write.
		if kind != arena.KindWork && !finiteRow(next) {
			// The store refuses a non-finite rating for an entity, because a
			// NaN poisons every later comparison of that entity and SQLite
			// stores it happily. The batch writes its own SQL, so it applies
			// the same rule rather than trusting the store to have done it.
			tx.Rollback()
			return res, fmt.Errorf("%s batch: non-finite rating for %d", kind, id)
		}
		ratingStmt, historyStmt := kindUpsert(kind)
		if _, err := tx.ExecContext(ctx, ratingStmt, kindArgs(kind, next)...); err != nil {
			tx.Rollback()
			return res, fmt.Errorf("%s batch write %d: %w", kind, id, err)
		}
		if _, err := tx.ExecContext(ctx, historyStmt, kindHistoryArgs(kind, next)...); err != nil {
			tx.Rollback()
			return res, fmt.Errorf("%s batch history %d: %w", kind, id, err)
		}
		res.Updated++
	}
	// Two different quantities, stored under two different keys because
	// conflating them is what made every batch re-fold the history:
	//
	//   arena_period    the NUMBER of the period, for labelling history
	//   arena_last_run  the TIME the previous period ended, which is the
	//                   watermark JudgedSince reads on the next batch
	//
	// The watermark is taken as "now", not as the newest judged_at in this
	// batch. Anything judged between the read and this write belongs to the
	// NEXT period; taking now can therefore double-count a game judged
	// during the run, and that is the lesser error, because Glicko's own
	// guidance is that a repeated game costs a little accuracy while a
	// dropped one silently biases a rating upward.
	if _, err := tx.ExecContext(ctx, arenaSetMeta, kindMeta(kind, "arena_period"), fmt.Sprint(period)); err != nil {
		tx.Rollback()
		return res, fmt.Errorf("%s batch period: %w", kind, err)
	}
	if _, err := tx.ExecContext(ctx, arenaSetMeta, kindMeta(kind, "arena_last_run"),
		fmt.Sprint(time.Now().UTC().Unix())); err != nil {
		tx.Rollback()
		return res, fmt.Errorf("%s batch watermark: %w", kind, err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("arena batch commit: %w", err)
	}

	res.Period = int64(period)
	res.Comparisons = len(judged)
	if a.Log != nil {
		a.Log.Info("arena batch", "kind", string(kind), "period", period,
			"comparisons", len(judged), "entities", res.Updated)
	}
	return res, nil
}

// kindMeta names the key a kind stores a watermark under. Works keep the bare
// keys they have always used; an entity kind gets its own, so the two never read
// each other's position.
func kindMeta(kind arena.EntityKind, key string) string {
	if kind == arena.KindWork {
		return key
	}
	return key + ":" + string(kind)
}

// kindRatingsFor reads a period's pre-ratings from the right table.
func (a *ArenaService) kindRatingsFor(ctx context.Context, kind arena.EntityKind, ids []int64) (map[int64]store.RatingRow, error) {
	if kind == arena.KindWork {
		return a.Store.RatingFor(ctx, ids)
	}
	return a.Store.RatingForKind(ctx, kind, ids)
}

// kindUpsert returns the upsert pair for a kind's table. The entity statements are
// the same two the store uses in SaveRatingKind, and a test pins that a batch row
// reads back through RatingForKind -- the column list has to be the same list, and
// only reading it back can say so.
func kindUpsert(kind arena.EntityKind) (rating, history string) {
	if kind == arena.KindWork {
		return arenaUpsertRating, arenaUpsertHistory
	}
	return arenaUpsertEntityRating, arenaUpsertEntityHistory
}

// kindArgs lists one row's positional arguments in its statement's order.
// Positional, not a struct: database/sql cannot marshal a struct, and a single
// struct argument is rejected by the driver before the statement is prepared.
func kindArgs(kind arena.EntityKind, r store.RatingRow) []any {
	if kind == arena.KindWork {
		return []any{r.WorkID, r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma,
			r.Comparisons, r.Wins, r.Losses, r.Draws, r.Period}
	}
	return []any{string(kind), r.WorkID, r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma,
		r.Comparisons, r.Wins, r.Losses, r.Draws, r.Period}
}

// kindHistoryArgs is kindArgs for the history statement, which carries no counts.
func kindHistoryArgs(kind arena.EntityKind, r store.RatingRow) []any {
	if kind == arena.KindWork {
		return []any{r.WorkID, r.Period, r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma}
	}
	return []any{string(kind), r.WorkID, r.Period, r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma}
}

// finiteRow reports whether a row can be stored without poisoning an entity's
// later comparisons.
func finiteRow(r store.RatingRow) bool {
	for _, v := range []float64{r.Rating.Mu, r.Rating.Phi, r.Rating.Sigma} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

const arenaUpsertEntityRating = `
INSERT INTO arena_entities
  (kind, entity_id, mu, phi, sigma, comparisons, wins, losses, draws, period, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(kind, entity_id) DO UPDATE SET
  mu = excluded.mu, phi = excluded.phi, sigma = excluded.sigma,
  comparisons = excluded.comparisons, wins = excluded.wins,
  losses = excluded.losses, draws = excluded.draws,
  period = excluded.period, updated_at = excluded.updated_at`

const arenaUpsertEntityHistory = `
INSERT OR REPLACE INTO arena_entity_history (kind, entity_id, period, mu, phi, sigma)
VALUES (?, ?, ?, ?, ?, ?)`

// BatchResult reports what a batch run did.
type BatchResult struct {
	Period      int64
	Comparisons int
	Updated     int
	Skipped     string
	// ByKind reports the period each arena advanced to. One Period cannot
	// describe a run that advanced several: they are independent counters.
	ByKind map[string]int64
}

const arenaSetMeta = `
INSERT INTO graph_meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`

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

// pairEntities presents a comparison between two entities of a kind that is not
// works.
func (a *ArenaService) pairEntities(ctx context.Context, w http.ResponseWriter, r *http.Request, kind arena.EntityKind) (PairResult, error) {
	sessionKey, ownerKey, err := a.SessionKey(w, r)
	if err != nil {
		return PairResult{}, err
	}

	// Resume, per kind: a reload must not reshuffle, and it must not resume
	// the other arena's pair either.
	if c, ok, err := a.Store.UnjudgedInSessionKind(ctx, sessionKey, kind); err != nil {
		return PairResult{}, err
	} else if ok {
		if res, err := a.entityPairResult(ctx, kind, c.WorkA, c.WorkB); err == nil {
			res.ComparisonID, res.Resumed, res.Strategy = c.ID, true, arena.Strategy(c.Strategy)
			return res, nil
		}
		// Both entities lost their names under us -- a re-crawl can drop a
		// candidate -- so present a fresh pair rather than wedge the session
		// on a row nothing can render.
	}

	pool := a.CandidatePool
	if pool <= 0 {
		pool = 300
	}
	// The candidate set lives on the corpus side (grouping the mirror's tag
	// rows measured 2.2s) and is materialised into the state database, so a page
	// view never pays it. A kind the mirror has almost none of is a normal
	// outcome: it answers zero candidates and is still recorded as loaded.
	if _, err := a.Store.EnsureEntityCandidates(ctx, kind,
		corpus.NewAO3(a.Store.Corpus), pool, entityCandidateTTL); err != nil {
		return PairResult{}, err
	}
	cands, err := a.Store.EntityCandidates(ctx, kind, pool)
	if err != nil {
		return PairResult{}, err
	}
	if len(cands) < 2 {
		return PairResult{}, store.ErrNoPair
	}
	ids := make([]int64, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	ratings, err := a.Store.RatingForKind(ctx, kind, ids)
	if err != nil {
		return PairResult{}, err
	}
	arenaCands := make([]arena.Candidate, 0, len(cands))
	for _, c := range cands {
		r := arena.Initial()
		if row, ok := ratings[c.ID]; ok {
			r = row.Rating
		}
		arenaCands = append(arenaCands, arena.Candidate{ID: c.ID, Rating: r})
	}

	seen, err := a.Store.SeenPairsKind(ctx, ownerKey, kind, 4000)
	if err != nil {
		return PairResult{}, err
	}

	strategy := a.Strategy
	if s := r.URL.Query().Get("strategy"); s != "" {
		if cs := arena.Strategy(s); cs.Valid() {
			strategy = cs
		}
	}

	// No Preferred map and no Tags on the candidates, deliberately: the learned
	// tag weights come from work choices, and feeding them back into a
	// tag-versus-tag vote is circular. ChoosePair degrades to a random pick when
	// the preference map is empty, which is the honest behaviour for an arena
	// that has no signal about its entities yet.
	pair, err := arena.ChoosePair(arena.PairRequest{
		Candidates: arenaCands,
		Seen:       seen,
		Strategy:   strategy,
		Explored:   a.Explored,
		Rand:       func(n int) int { return mrand.IntN(n) },
	})
	if err != nil {
		return PairResult{}, err
	}

	id, err := a.Store.RecordPresentation(ctx, store.Comparison{
		Kind: kind, WorkA: pair.A.ID, WorkB: pair.B.ID,
		SessionKey: sessionKey, OwnerKey: ownerKey,
		Strategy: string(pair.Strategy),
	})
	if err != nil {
		return PairResult{}, err
	}
	if _, err := a.Store.TouchSession(ctx, sessionKey, ownerKey); err != nil {
		return PairResult{}, err
	}
	res, err := a.entityPairResult(ctx, kind, pair.A.ID, pair.B.ID)
	if err != nil {
		return PairResult{}, err
	}
	res.ComparisonID, res.Strategy = id, pair.Strategy
	return res, nil
}

// entityPairResult names the two sides of one entity pair.
//
// A missing name is an ERROR, not an empty label: an entity with a rating but no
// name is one the candidate table has dropped, and rendering it as a blank card
// would hide that the crawl lost it.
func (a *ArenaService) entityPairResult(ctx context.Context, kind arena.EntityKind, aID, bID int64) (PairResult, error) {
	ratings, err := a.Store.RatingForKind(ctx, kind, []int64{aID, bID})
	if err != nil {
		return PairResult{}, err
	}
	brief := func(id int64) (EntityBrief, error) {
		name, ok, err := a.Store.EntityName(ctx, kind, id)
		if err != nil {
			return EntityBrief{}, err
		}
		if !ok {
			return EntityBrief{}, fmt.Errorf("entity %s/%d has no name: the candidate table no longer holds it", kind, id)
		}
		b := EntityBrief{ID: id, Name: name, Kind: kind, Rating: arena.Initial()}
		if row, ok := ratings[id]; ok {
			b.Rating, b.Rated, b.Counts = row.Rating, row.Comparisons > 0, row.Comparisons
		}
		return b, nil
	}
	ea, err := brief(aID)
	if err != nil {
		return PairResult{}, err
	}
	eb, err := brief(bID)
	if err != nil {
		return PairResult{}, err
	}
	return PairResult{Kind: kind, EntityA: ea, EntityB: eb}, nil
}
