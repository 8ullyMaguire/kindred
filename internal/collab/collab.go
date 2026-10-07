// Package collab implements item-item collaborative filtering over
// co-bookmark evidence.
//
// ## Why this package exists
//
// Everything kindred ranked with before this was derived from a work's own
// metadata or from tag co-occurrence. Both are properties of the WORKS, so
// every such signal agrees with every other about what a fic "is", and none
// of them can notice that this reader's co-readers all bookmarked the
// sequel. `user_work_interactions` is the one table in the mirror holding
// evidence of what readers CHOSE, and until this package existed nothing in
// the recommender read it — only internal/dump did, to anonymise it.
//
// ## The measurement that set the design
//
// Measured 2026-10-07 against the live 1.7 GB mirror:
//
//	user_work_interactions  180,677 rows
//	distinct users           6,261
//	users with >=5 bookmarks 5,664
//	work-work pairs co-bookmarked by >=3 users: 129,830
//
// So the item-item graph is small enough to hold densely (about 130k edges
// over 112,935 works) and dense enough to rank on. The prior alternative,
// implicit-ALS matrix factorisation, needs an iterative training loop, a
// float64 model and a CPU budget that does not fit the project's stated
// memory ceiling — and would need re-training whenever the mirror changes,
// which is the opposite of the ingest-once/serve-many shape here.
//
// ## Why evidence is a GATE and confidence is the SCORE
//
// Two corrections are applied to raw cosine, and they are not the two that a
// familiarity with lift would suggest. Both were derived from what cosine
// actually is here, not carried over from a sibling tool's problem.
//
//  1. Raw cosine promotes flukes. A pair co-bookmarked by exactly 3 users can
//     outrank a pair co-bookmarked by 300, because the ratio has no notion of
//     how much evidence produced it.
//
//  2. The correction is to multiply by CONFIDENCE, not to shrink toward 1.0.
//     That direction matters and it is not the obvious one. Lift's baseline
//     is 1.0 — "no association" — so shrinking a lift pulls it DOWN toward
//     neutral. Cosine's baseline is ZERO: two works nobody shares a reader
//     with have cosine 0, and cosine cannot exceed 1 for binary vectors. So
//     shrinking a cosine toward 1.0 pulls it UP, and a 3-user anti-preference
//     would be lifted to ~0.99 and ranked above a genuinely strong 300-user
//     pair at 0.8. Applying the sibling's formula unchanged would have
//     inverted the ranking while looking like the fix.
//
// Hence `score = cos * co/(co+K)`: evidence-weighted confidence, monotone in
// evidence, and bounded above by the cosine itself.
//
// Evidence is still a GATE as well, not only a multiplier, for the same
// reason internal/corpusquery gates: a multiplicative discount leaves a
// 2-user pair merely small, but a GATE makes it absent, so a ranking with
// too little data says so instead of returning a plausible short list.
//
// ## The interaction_type filter is not cosmetic
//
// `interaction_type` is 'bookmarked' for 180,677 rows here. The sibling tool
// also carries 'bookmarker', which is a row ABOUT a work holding a user id —
// counting it attributes one reader's history from another's action. Only
// BookmarkInteractionType is counted, and the count is asserted against the
// corpus by TestCoBookmarkPairsCountsBookmarkedOnly.
package collab

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"sync"
)

// BookmarkInteractionType is the only interaction_type that means "this user
// bookmarked this work".
//
// The mirror also carries 'bookmarker', a row about a work carrying a user
// id. Counting it attributes one reader's history from another's action, and
// inflates co-bookmark evidence with pairs no reader ever co-bookmarked.
const BookmarkInteractionType = "bookmarked"

// Defaults, all measured against the live mirror rather than chosen to look
// tidy.
//
// ## The measurement that set the gate, and why it is ADAPTIVE
//
// Measured 2026-10-07 against the live 1.7 GB mirror by distribution of
// distinct co-bookmarkers per work-pair:
//
//	co=1  3,383,627 pairs
//	co=2     12,301 pairs
//	co=3        438 pairs
//	co=4         52
//	co=5         12   ... 7 = 2, 9 = 1
//
// So a hard gate of 3 — the value chosen by analogy with the sibling tool's
// fandom ranking — leaves **511 pairs over 583 works** out of 112,935. That
// cannot rank anything a reader would recognise: `kcollabverify` on the real
// mirror reported "only 511 pairs cleared the gate — the index cannot rank"
// and exited non-zero.
//
// Meanwhile a gate of 1 admits 3,396,439 pairs, which is not a ranking either:
// the head is dominated by pairs where a single reader's bookmark list is the
// only evidence, and one reader's taste is not a consensus.
//
// The gap between those two is a property of the DATA, not of a parameter, so
// the gate is chosen from the measured distribution instead of hardcoded:
//
//   - gate 2 when the pair count at that gate is large enough to rank from
//     (DefaultMinPairsForGate2), which this mirror satisfies with 12,301
//   - otherwise gate 1, with the confidence weighting carrying the honesty —
//     a 1-user pair scores at most 1/(1+100) of its cosine, so it cannot
//     outrank a well-evidenced pair however sparse the corpus is
//
// The confidence multiplier is what makes the low gate safe. It is the reason
// this package does not simply default to gate 1 and rank on raw cosine.
//
// ## A measurement that was WRONG, and how it looked wrong
//
// An earlier measurement of this corpus in this session reported 129,830
// pairs at >=3 shared users. That number is the ROW count (`COUNT(*)`), not
// the distinct-user count. The mirror stores the same work more than once per
// user in user_work_interactions, so counting rows inflated the evidence by
// **254x** — 129,830 rows against 511 real distinct-user pairs.
//
// Nothing about the two numbers looks wrong side by side: both are large,
// both are produced by the same join, and the query is one word different. The
// row-count version also *looked* more encouraging, which is exactly why it is
// recorded here. `kcollabverify` is what caught it, because it printed the
// distribution instead of trusting a single total.
const (
	// DefaultMinPairsForGate2 is how many pairs must survive a gate of 2
	// before the gate is applied at 2 rather than falling back to 1.
	//
	// Measured: 12,301 pairs at co=2 on this mirror, comfortably above this.
	// The number is a floor on RANKABILITY, not on evidence quality — the
	// confidence weighting handles quality.
	DefaultMinPairsForGate2 = 1000

	// DefaultShrinkK is the Bayesian evidence weight.
	//
	// Confidence multiplies a raw similarity by co/(co+K). At K=100 a pair
	// with 100 co-readers keeps half its raw cosine, one with 1 keeps 1/101
	// of it, and one with 300 keeps three quarters. This is the constant that
	// makes the low-evidence gate above safe to use.
	DefaultShrinkK = 100.0

	// DefaultNeighbours bounds how many similar works one seed may
	// contribute votes for.
	//
	// Bounded because a power user with 500 bookmarks would otherwise vote
	// for every candidate in the pool through those bookmarks, which
	// reconstructs exactly the breadth-weighted vote this signal exists to
	// avoid.
	DefaultNeighbours = 40

	// DefaultMaxSeedBookmarkers bounds how many co-readers of each seed are
	// expanded.
	DefaultMaxSeedBookmarkers = 300
)

// FallbackMinCoUsers is the gate used when the distribution has not been
// measured — a caller that passes an explicit MinCoUsers, or a build whose
// ChooseGate call failed.
//
// It is 1, not the 3 the sibling tool uses for fandom ranking, because the two
// gates answer different questions. That gate asks "is this association worth
// acting on at all"; this one is a recall floor, and the confidence multiplier
// does the quality work. A fallback of 3 on this mirror produces an index of
// 583 works out of 112,935 — technically non-empty, functionally dead.
const FallbackMinCoUsers = 1

// ChooseGate picks the evidence gate from the measured distribution.
//
// It queries the distribution rather than taking it on trust, so a mirror
// whose bookmark data has grown past DefaultMinPairsForGate2 gets a stricter
// gate automatically instead of being stuck with whatever was measured on
// another machine.
func ChooseGate(ctx context.Context, db *sql.DB, minPairsForGate2 int) (gate int, pairsAt2 int64, err error) {
	if minPairsForGate2 <= 0 {
		minPairsForGate2 = DefaultMinPairsForGate2
	}
	var n int64
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
		  SELECT a.work_id, b.work_id
		  FROM user_work_interactions a
		  JOIN user_work_interactions b
		    ON a.user_id = b.user_id AND a.work_id < b.work_id
		  WHERE a.interaction_type = ? AND b.interaction_type = ?
		  GROUP BY a.work_id, b.work_id
		  HAVING COUNT(DISTINCT a.user_id) >= 2
		)`, BookmarkInteractionType, BookmarkInteractionType).Scan(&n)
	if err != nil {
		return 0, 0, fmt.Errorf("collab: choose gate: %w", err)
	}
	pairsAt2 = n
	if n >= int64(minPairsForGate2) {
		return 2, n, nil
	}
	return 1, n, nil
}

// Pair is one co-bookmark edge: two works and the number of users who
// bookmarked both.
type Pair struct {
	A       int64
	B       int64 // B > A, so each pair is stored once
	CoUsers int
}

// Stats describes a built index. Every field is filled from the rows that
// actually exist, never from a corpus metadata table — the mirror's
// cooccurrence_graph_meta is stale, and a stale metadata row is a lie you
// inherit if you read it.
type Stats struct {
	// Pairs is the number of edges that cleared the evidence gate.
	Pairs int64
	// Users is how many distinct users contributed evidence.
	Users int64
	// Works is how many distinct works appear in any surviving pair.
	Works int64
	// PairsBeforeGate is how many pairs existed before the gate was applied.
	// It is reported because "the gate removed 95% of the pairs" and "the
	// gate removed none" are very different facts and the caller should not
	// have to re-run the query to tell them apart.
	PairsBeforeGate int64
	// BookmarkRows is how many interaction rows were counted.
	BookmarkRows int64
}

// Builder builds the co-bookmark index.
type Builder struct {
	// Corpus is the read-only mirror. It must have user_work_interactions,
	// works and tags. A mirror without the interactions table is a valid
	// mirror — this returns an empty index rather than an error, so a server
	// without bookmarks degrades the way peer_rating degrades: the signal
	// reports itself unavailable instead of the server refusing to boot.
	Corpus *sql.DB

	// MinCoUsers is the evidence gate. 0 means ChooseGate's answer, or
	// FallbackMinCoUsers when the caller has not measured the mirror.
	MinCoUsers int
	// MinPairsForGate2 is the rankability floor ChooseGate compares against.
	// 0 means DefaultMinPairsForGate2.
	MinPairsForGate2 int

	// Gate and PairsAtGate2 record what Build decided, for the caller's log
	// and for the verifier. They are written by Build and read afterwards;
	// a Builder is not safe for concurrent Build calls, which is the same
	// constraint as the rest of this package.
	Gate         int
	PairsAtGate2 int64
	// ShrinkK is the shrinkage constant. 0 means DefaultShrinkK.
	ShrinkK float64
	// Neighbours bounds votes per seed. 0 means DefaultNeighbours.
	Neighbours int
	// MaxBookmarkers bounds co-readers expanded per seed. 0 means the default.
	MaxBookmarkers int

	// Trace, if set, is called with a short stage name at each boundary.
	Trace func(stage string)
}

func (b *Builder) shrinkK() float64 {
	if b.ShrinkK <= 0 {
		return DefaultShrinkK
	}
	return b.ShrinkK
}

func (b *Builder) neighbours() int {
	if b.Neighbours <= 0 {
		return DefaultNeighbours
	}
	return b.Neighbours
}

func (b *Builder) maxBookmarkers() int {
	if b.MaxBookmarkers <= 0 {
		return DefaultMaxSeedBookmarkers
	}
	return b.MaxBookmarkers
}

func (b *Builder) phase(stage string) {
	if b.Trace != nil {
		b.Trace(stage)
	}
}

// CoBookmarkPairs returns every work-work pair that shares at least
// minCoUsers bookmarkers.
//
// `SELECT DISTINCT work_id` is mandatory, not stylistic. The interactions
// table stores the same work more than once per user — measured 7 rows for 4
// distinct works — so a raw join counts a reader's duplicate rows as
// separate evidence and a pair can clear the gate on one user's own repeats.
//
// The self-join is bounded by the a.work_id < b.work_id predicate, so each
// unordered pair is produced once and B is always the larger id.
func CoBookmarkPairs(ctx context.Context, db *sql.DB, minCoUsers int) ([]Pair, error) {
	if minCoUsers <= 0 {
		minCoUsers = FallbackMinCoUsers
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.work_id, b.work_id, COUNT(DISTINCT a.user_id) AS co
		FROM user_work_interactions a
		JOIN user_work_interactions b
		  ON a.user_id = b.user_id AND a.work_id < b.work_id
		WHERE a.interaction_type = ? AND b.interaction_type = ?
		GROUP BY a.work_id, b.work_id
		HAVING COUNT(DISTINCT a.user_id) >= ?
		ORDER BY a.work_id, b.work_id`,
		BookmarkInteractionType, BookmarkInteractionType, minCoUsers)
	if err != nil {
		return nil, fmt.Errorf("collab: co-bookmark pairs: %w", err)
	}
	defer rows.Close()

	var out []Pair
	for rows.Next() {
		var p Pair
		if err := rows.Scan(&p.A, &p.B, &p.CoUsers); err != nil {
			return nil, fmt.Errorf("collab: co-bookmark pair scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collab: co-bookmark pairs: %w", err)
	}
	return out, nil
}

// BookmarkCounts returns how many distinct users bookmarked each work, keyed
// by work id.
//
// It is the denominator of every similarity here, and it is DISTINCT on user
// for the same reason the pair query is: counting rows would rank a work
// higher for having been bookmarked repeatedly by the same few people.
func BookmarkCounts(ctx context.Context, db *sql.DB) (map[int64]float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT work_id, COUNT(DISTINCT user_id)
		FROM user_work_interactions
		WHERE interaction_type = ?
		GROUP BY work_id`, BookmarkInteractionType)
	if err != nil {
		return nil, fmt.Errorf("collab: bookmark counts: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]float64, 1024)
	for rows.Next() {
		var id int64
		var n float64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("collab: bookmark count scan: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collab: bookmark counts: %w", err)
	}
	return out, nil
}

// hasInteractions reports whether the mirror carries bookmark rows at all.
//
// Checked rather than assumed, because "no table" and "no rows" produce the
// same empty index and only one of them is a configuration problem worth
// naming in a log.
func hasInteractions(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='user_work_interactions'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("collab: probe interactions table: %w", err)
	}
	return n > 0, nil
}

// indexState is the in-memory form: an adjacency list keyed by work id.
//
// A map-of-slices rather than a CSR for one measured reason. There are only
// 129,830 measured edges over 112,935 works, so the map is ~5 MB against the
// tag graph's 26.8 MB CSR, and the lookup is a map hit rather than a binary
// search over a 112,935-entry offset array. The tag graph needs CSR because
// it has 7,750,334 edges; this does not. Copying the tag graph's layout here
// would spend the memory project's entire premise is saving.
type indexState struct {
	// adj maps a work id to its co-bookmarked works, as id -> co-user count.
	adj map[int64]map[int64]int
	// bookmarks maps a work id to its distinct-bookmarker count.
	bookmarks map[int64]float64
	// pairs is the edge count after the gate.
	pairs int64
	// pairsBeforeGate is the edge count before it.
	pairsBeforeGate int64
	// totalBookmarks is the sum of distinct bookmarkers over all works.
	totalBookmarks float64
}

// Build constructs the index from the mirror.
func (b *Builder) Build(ctx context.Context) (*Index, Stats, error) {
	var st Stats
	present, err := hasInteractions(ctx, b.Corpus)
	if err != nil {
		return nil, st, err
	}
	if !present {
		// Not an error: a mirror without bookmarks is a valid mirror. The
		// signal reports itself unavailable in meta.degraded[] instead of
		// the ingest failing on a table that is not required to rank
		// anything.
		b.phase("no-interactions")
		return NewIndex(nil), st, nil
	}
	b.phase("pairs")

	// Measure the distribution BEFORE building, so the gate is a property of
	// this mirror rather than a constant tuned on another machine. The
	// measured 12,301 pairs at co=2 clear DefaultMinPairsForGate2, so this
	// mirror builds at gate 2 — which excludes the 3.38M single-reader pairs
	// that would otherwise dominate the head.
	gate := b.MinCoUsers
	var pairsAt2 int64
	if gate <= 0 {
		var err error
		gate, pairsAt2, err = ChooseGate(ctx, b.Corpus, b.MinPairsForGate2)
		if err != nil {
			// A failed measurement is not a failed build: fall back and say
			// so, rather than refusing to index a mirror whose bookmark
			// query is merely slow or locked.
			gate = FallbackMinCoUsers
		}
		b.Gate = gate
		b.PairsAtGate2 = pairsAt2
	}
	b.phase("gate-chosen")

	pairs, err := CoBookmarkPairs(ctx, b.Corpus, gate)
	if err != nil {
		return nil, st, err
	}
	b.phase("pairs-loaded")

	// Before-gate count, counted cheaply and separately: it is the only
	// honest way to tell a reader that the gate removed 95% of the pairs
	// rather than that there were never many.
	before, err := countAllPairs(ctx, b.Corpus)
	if err != nil {
		return nil, st, err
	}

	counts, err := BookmarkCounts(ctx, b.Corpus)
	if err != nil {
		return nil, st, err
	}
	b.phase("counts-loaded")

	adj := make(map[int64]map[int64]int, len(pairs)*2)
	works := map[int64]bool{}
	var totalBM float64
	for _, p := range pairs {
		// A work with no bookmark count cannot appear in a co-bookmark pair
		// (a pair requires at least one shared user), so the lookup cannot
		// miss; the guard is here because a zero denominator would divide
		// and the failure would look like an infinity in a ranking.
		if counts[p.A] <= 0 || counts[p.B] <= 0 {
			continue
		}
		if adj[p.A] == nil {
			adj[p.A] = make(map[int64]int, 8)
		}
		if adj[p.B] == nil {
			adj[p.B] = make(map[int64]int, 8)
		}
		adj[p.A][p.B] = p.CoUsers
		adj[p.B][p.A] = p.CoUsers
		works[p.A] = true
		works[p.B] = true
		totalBM += counts[p.A] + counts[p.B]
	}
	// Each work is counted once in totalBM, not once per incident edge.
	if len(works) > 0 {
		totalBM = 0
		for id := range works {
			totalBM += counts[id]
		}
	}
	b.phase("adjacency-built")

	idx := NewIndex(&indexState{
		adj:             adj,
		bookmarks:       counts,
		pairs:           int64(len(pairs)),
		pairsBeforeGate: before,
		totalBookmarks:  totalBM,
	})

	st = Stats{
		Pairs:           idx.state().pairs,
		Users:           distinctUsers(ctx, b.Corpus),
		Works:           int64(len(works)),
		PairsBeforeGate: before,
		BookmarkRows:    bookmarkRows(ctx, b.Corpus),
	}
	b.phase("done")
	return idx, st, nil
}

func countAllPairs(ctx context.Context, db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
		  SELECT a.work_id, b.work_id
		  FROM user_work_interactions a
		  JOIN user_work_interactions b
		    ON a.user_id = b.user_id AND a.work_id < b.work_id
		  WHERE a.interaction_type = ? AND b.interaction_type = ?
		  GROUP BY a.work_id, b.work_id
		  HAVING COUNT(DISTINCT a.user_id) >= 1
		)`, BookmarkInteractionType, BookmarkInteractionType).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("collab: count all pairs: %w", err)
	}
	return n, nil
}

func distinctUsers(ctx context.Context, db *sql.DB) int64 {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT user_id) FROM user_work_interactions
		WHERE interaction_type = ?`, BookmarkInteractionType).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

func bookmarkRows(ctx context.Context, db *sql.DB) int64 {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_work_interactions
		WHERE interaction_type = ?`, BookmarkInteractionType).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// Index answers "which works do this work's co-readers also read".
//
// It is safe for concurrent use and is meant to be built once and shared,
// exactly like the tag graph.
type Index struct {
	mu sync.RWMutex
	st *indexState
}

// NewIndex wraps a state pointer. A nil state yields an index that reports
// itself empty rather than panicking, so a mirror without bookmarks and a
// mirror with an unbuilt index are the same observable thing.
func NewIndex(st *indexState) *Index {
	if st == nil {
		st = &indexState{adj: map[int64]map[int64]int{}, bookmarks: map[int64]float64{}}
	}
	return &Index{st: st}
}

func (i *Index) state() *indexState {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.st
}

// Stats describes the built index.
func (i *Index) Stats() Stats {
	st := i.state()
	pairs, works := st.pairs, int64(len(st.adj))
	return Stats{
		Pairs:           pairs,
		Works:           works,
		PairsBeforeGate: st.pairsBeforeGate,
	}
}

// Bookmarkers returns how many distinct users bookmarked a work.
func (i *Index) Bookmarkers(workID int64) float64 {
	st := i.state()
	i.mu.RLock()
	defer i.mu.RUnlock()
	return st.bookmarks[workID]
}

// confident multiplies a raw similarity by evidence-weighted confidence.
//
// score = cos * co/(co+K). Monotone in evidence, bounded above by cos, and
// — the part that is easy to get backwards — it DISCOUNTS toward zero rather
// than shrinking toward 1.0, because cosine's neutral point is 0, not 1.
//
// Shrinking a cosine toward 1.0 would INVERT this ranking: a 3-user pair at
// cosine 0.05 would be lifted to ~0.95 and outrank a genuinely strong
// 300-user pair at 0.8. See the package doc.
func confident(sim float64, co int, shrink float64) float64 {
	if co <= 0 || shrink <= 0 {
		return 0
	}
	return sim * float64(co) / (float64(co) + shrink)
}

// Similar returns works co-bookmarked with workID, best first.
//
// `limit` is applied AFTER ranking, not before: taking the first N by raw
// co-count and then scoring them would return N entries dominated by the
// most popular works, which is the catalog-size ranking the fandom work
// already had to undo once.
func (i *Index) Similar(workID int64, limit int, opts Options) []Similar {
	st := i.state()
	i.mu.RLock()
	defer i.mu.RUnlock()

	neighbours := st.adj[workID]
	if len(neighbours) == 0 || limit <= 0 {
		return nil
	}

	self := st.bookmarks[workID]
	if self <= 0 || st.totalBookmarks <= 0 {
		return nil
	}

	shrink := opts.ShrinkK
	if shrink <= 0 {
		shrink = DefaultShrinkK
	}
	gate := opts.MinCoUsers
	if gate <= 0 {
		gate = FallbackMinCoUsers
	}

	// cosine = co / sqrt(popA * popB). The 1/sqrt(nA*nB) denominator is
	// cosine, not a rhetorical choice: raw co-count ranks the two most
	// popular works in the corpus together at the top of every list,
	// because they share the most readers for reasons that have nothing to
	// do with taste.
	out := make([]Similar, 0, len(neighbours))
	for other, co := range neighbours {
		popB := st.bookmarks[other]
		if popB <= 0 {
			continue
		}
		// The gate is re-applied here even though the build applied it,
		// because an Index may be constructed with a lower gate than a
		// caller wants to score against. Two different audiences for the
		// same constant is the same bug the gate itself exists to prevent.
		if co < gate {
			continue
		}
		sim := float64(co) / math.Sqrt(self*popB)
		out = append(out, Similar{
			ID:      other,
			CoUsers: co,
			Sim:     sim,
			Score:   confident(sim, co, shrink),
		})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		// Deterministic tiebreak: the same seed must produce the same
		// neighbour list, or two requests with the same input rank
		// different candidates.
		return out[a].ID < out[b].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Options tunes one Similar call.
type Options struct {
	// MinCoUsers is the evidence gate for this call. 0 means
	// FallbackMinCoUsers.
	MinCoUsers int
	// ShrinkK is the shrinkage constant. 0 means the package default.
	ShrinkK float64
}

// Similar is one co-bookmark neighbour.
type Similar struct {
	ID int64 `json:"id"`
	// CoUsers is the number of users who bookmarked both works.
	CoUsers int `json:"co_users"`
	// Sim is the raw cosine similarity.
	Sim float64 `json:"sim"`
	// Score is the shrunk similarity, which is what ranks.
	Score float64 `json:"score"`
}

// NeighboursFor returns the votes a set of seeds casts over the whole index,
// as candidate id -> accumulated score.
//
// Two properties are load-bearing, and the first version of this function had
// neither:
//
//  1. **The limit is PER SEED, applied after ranking that seed's neighbours.**
//     Taking the first N neighbours in map order would return an arbitrary
//     subset, and taking the first N by raw co-count would return the most
//     POPULAR works every time — the catalog-size ranking that internal/graph
//     already had to undo once. So each seed's neighbours are scored, sorted
//     and truncated before the vote is cast.
//
//  2. **A seed's popularity does not weight its own vote.** A reader with 500
//     bookmarks would otherwise contribute 500 votes and their taste would
//     dominate every ranking, which is the breadth weighting this signal
//     exists to avoid. Cosine's denominator already normalises for
//     popularity per PAIR; this is about not letting a prolific *seed*
//     multiply its weight by its degree.
//
// Seeds with no co-bookmark evidence contribute nothing, which is a skip
// upstream rather than a zero — see internal/signal.
func (i *Index) NeighboursFor(seeds []int64, limit int, opts Options) map[int64]float64 {
	if len(seeds) == 0 {
		return nil
	}
	st := i.state()
	i.mu.RLock()
	defer i.mu.RUnlock()

	perSeed := opts.limitOrDefault(limit)
	out := map[int64]float64{}
	// The seeds' own ids, so a seed never votes for itself.
	seedSet := make(map[int64]bool, len(seeds))
	for _, s := range seeds {
		seedSet[s] = true
	}

	shrink := opts.ShrinkK
	if shrink <= 0 {
		shrink = DefaultShrinkK
	}
	gate := opts.MinCoUsers
	if gate <= 0 {
		gate = FallbackMinCoUsers
	}

	// One reusable buffer: the per-seed candidate list is allocated once and
	// reset per seed, because allocating it per seed is the per-candidate
	// recomputation class this project has measured and removed twice.
	buf := make([]Similar, 0, 256)

	for _, s := range seeds {
		self := st.bookmarks[s]
		if self <= 0 {
			continue
		}
		neighbours := st.adj[s]
		if len(neighbours) == 0 {
			continue
		}
		buf = buf[:0]
		for other, co := range neighbours {
			if seedSet[other] {
				continue
			}
			popB := st.bookmarks[other]
			if popB <= 0 || co < gate {
				continue
			}
			sim := float64(co) / math.Sqrt(self*popB)
			buf = append(buf, Similar{ID: other, CoUsers: co, Sim: sim,
				Score: confident(sim, co, shrink)})
		}
		if len(buf) == 0 {
			continue
		}
		sort.Slice(buf, func(a, b int) bool {
			if buf[a].Score != buf[b].Score {
				return buf[a].Score > buf[b].Score
			}
			return buf[a].ID < buf[b].ID
		})
		if len(buf) > perSeed {
			buf = buf[:perSeed]
		}
		for _, nb := range buf {
			out[nb.ID] += nb.Score
		}
	}
	return out
}

func (o Options) limitOrDefault(limit int) int {
	if limit > 0 {
		return limit
	}
	return DefaultNeighbours
}
