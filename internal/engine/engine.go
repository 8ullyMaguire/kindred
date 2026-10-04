// Package engine assembles a recommender from the store, the graph and the
// signal set. It is the one place that knows how a request becomes a
// ranked list, so the API layer stays a projection of it and the signals
// stay pure.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/diversify"
	"git.polarisocial.xyz/kindred/kindred/internal/fandom"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/signal"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// Engine holds everything a request needs.
type Engine struct {
	Store  *store.Store
	Corpus *corpus.AO3
	Graph  *graph.CSR

	// PoolSize bounds the candidate set. Ranking tens of thousands of
	// entities per request is what the previous deployment did and it is
	// where its request latency came from.
	PoolSize int

	// TopN bounds the neighbours the neighbourhood signal votes from.
	TopN int

	// Lite drops the embedding signal, which is the one dimension that
	// needs a second index. The Pi runs in this mode.
	Lite bool

	// EmbedDim is the embedding width, for the reason string.
	EmbedDim int

	// ArenaRatings maps work id to the arena's damped rating, and
	// ArenaMedian is the centre it was damped around. Nil means the arena
	// has rated nothing, which is NOT the same as leaving peer_rating out:
	// the signal is still registered so it reports itself degraded.
	//
	// Guarded by arenaMu because a batch replaces this map while requests
	// are reading it. Assigning a map field is not atomic, and an unsynced
	// reader during a batch is a concurrent map read and write -- a fatal
	// runtime error, not a race the detector merely flags. Use
	// SetArenaRatings to replace them and ArenaSignal to read a consistent
	// pair.
	ArenaRatings map[int64]float64
	ArenaMedian  float64
	arenaMu      sync.RWMutex
}

// SetArenaRatings replaces the arena's ratings atomically with respect to
// readers.
func (e *Engine) SetArenaRatings(ratings map[int64]float64, median float64) {
	e.arenaMu.Lock()
	defer e.arenaMu.Unlock()
	e.ArenaRatings = ratings
	e.ArenaMedian = median
}

// ArenaSignal returns a consistent snapshot of the ratings and the median
// they were damped around. The two are read under one lock because the
// signal needs both to agree, and a torn read would centre the score on a
// median from a different population.
func (e *Engine) ArenaSignal() (map[int64]float64, float64) {
	e.arenaMu.RLock()
	defer e.arenaMu.RUnlock()
	return e.ArenaRatings, e.ArenaMedian
}

// Seed is one seed reference.
type Seed struct {
	Kind string
	ID   int64
}

// ParseSeed parses "kind:id".
//
// The id is rejected unless it is entirely digits. `fmt.Sscanf(id, "%d", &n)` was
// not enough: Sscanf stops at the first non-digit and reports no error, so
// `?seed=ao3_work:12abc` resolved to work 12, `?seed=ao3_work:1.5` to work 1, and
// `?seed=ao3_work: 5` to work 5. A seed is a public query parameter
// (internal/web/handlers.go, `?seed=ao3_work:1&seed=ao3_work:2`), so silently
// truncating what someone typed means the ranking is computed for an entity the
// request never named -- and the response looks entirely normal.
func ParseSeed(s string) (Seed, error) {
	kind, id, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || kind == "" || id == "" {
		return Seed{}, fmt.Errorf("seed %q must be kind:id, e.g. ao3_work:1234", s)
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return Seed{}, fmt.Errorf("seed %q has a non-numeric id %q", s, id)
	}
	return Seed{Kind: kind, ID: n}, nil
}

// Request is one recommendation request.
type Request struct {
	Seeds       []Seed
	Kind        string // the kind to recommend
	N           int
	Tune        string
	MaxPerGroup int
	GroupBy     string // "fandom", "tag", "author", or "" for none
	PoolSize    int
	// Exclude drops the seeds themselves from the results: nobody wants to
	// be recommended the work they seeded from.
	Exclude bool
}

// Result is a ranked list with its evidence and honest metadata.
type Result struct {
	Kind  string             `json:"kind"`
	Seeds []string           `json:"seeds"`
	Items []rank.Candidate   `json:"items"`
	Meta  rank.Meta          `json:"meta"`
	Tune  map[string]float64 `json:"tune"`
}

// DefaultPoolSize is how many candidates a request ranks by default.
//
// It is a memory bound, not a quality setting, and it differs by mode
// because the budget does. Measured: a 200-candidate lite request peaks at
// 58 MiB against a 60 MiB cap — the candidate set is the largest
// per-request allocation there is, so a default sized to leave headroom is
// worth more than a slightly larger pool. The caller can raise it with
// ?pool= and the budget gate walks a 1000-strong pool in full mode to prove
// the ceiling still holds.
const DefaultPoolSize = 200

// FullPoolSize is the default in full mode, where the cap is 220 MiB and
// the measurement showed 74 MiB with a 1000-strong pool.
const FullPoolSize = 1000

// DefaultTune weights the signals. They are named and overridable, so
// ranking is configurable without a code change — the property the
// previous deployment had and generalised here per kind.
func DefaultTune() rank.Tune {
	return rank.Tune{
		Name: "default",
		Weights: map[string]float64{
			"tag_overlap":   0.22,
			"neighbourhood": 0.30,
			"quality":       0.09,
			"recency":       0.09,
			"popularity":    0.09,
			"embedding":     0.10,
			// peer_rating starts small on purpose. It is the only signal
			// fed by human judgement rather than corpus analysis, so it is
			// the one whose weight should earn its place -- and an arena
			// with a handful of comparisons should nudge a ranking, not
			// steer it. `kindred tune` raises it without a code change.
			"peer_rating": 0.11,
		},
	}
}

// ErrUnsupportedKind is returned for a seed or request kind this mirror does
// not hold.
//
// It is a SENTINEL rather than a plain error so the API layer can map it to
// 400 without matching on message text: without it an unknown kind fell
// through to 500, and a caller who typo'd `book:1` was told the server had a
// fault when the fault was theirs.
var ErrUnsupportedKind = errors.New("unsupported kind")

// Recommend produces a ranked list.
func (e *Engine) Recommend(ctx context.Context, req Request) (*Result, error) {
	if len(req.Seeds) == 0 {
		return nil, fmt.Errorf("at least one seed is required")
	}
	kind := req.Kind
	if kind == "" {
		kind = req.Seeds[0].Kind
	}
	// Reject an unknown kind up front. The seed id is looked up in the corpus
	// regardless of its kind, so `book:1` silently resolves to AO3 work 1 and
	// returns a confident, plausible, entirely wrong ranking. A caller who
	// typo'd a kind gets "0 results" or worse, results for the wrong corpus;
	// what they need to hear is that the kind is not one this mirror holds.
	if kind != corpus.AO3Kind {
		return nil, fmt.Errorf("%w %q; this mirror holds %q",
			ErrUnsupportedKind, kind, corpus.AO3Kind)
	}
	for _, s := range req.Seeds {
		if s.Kind != "" && s.Kind != kind {
			return nil, fmt.Errorf("%w: seed kind %q does not match the requested kind %q",
				ErrUnsupportedKind, s.Kind, kind)
		}
	}
	n := req.N
	if n <= 0 {
		n = 10
	}
	if n > 100 {
		n = 100
	}
	pool := req.PoolSize
	if pool <= 0 {
		pool = e.PoolSize
	}
	if pool <= 0 {
		pool = DefaultPoolSize
	}

	// Seeds.
	seedIDs := make([]int64, 0, len(req.Seeds))
	for _, s := range req.Seeds {
		seedIDs = append(seedIDs, s.ID)
	}
	seedEnts, err := e.Corpus.CandidateRows(ctx, seedIDs)
	if err != nil {
		return nil, fmt.Errorf("load seeds: %w", err)
	}
	if len(seedEnts) == 0 {
		return nil, fmt.Errorf("%w: no seed in the corpus", store.ErrNotFound)
	}
	seeds, err := e.toCandidates(ctx, seedEnts)
	if err != nil {
		return nil, fmt.Errorf("load seed tags: %w", err)
	}

	// Candidate pool. Seeding by the seeds' own tags rather than scanning
	// the corpus is what keeps this bounded: the pool is the neighbourhood
	// of what the user already likes.
	candIDs, err := e.poolFor(ctx, seeds, pool, req.Exclude, seedIDs)
	if err != nil {
		return nil, err
	}
	if len(candIDs) == 0 {
		return &Result{
			Kind:  kind,
			Seeds: seedNames(req.Seeds),
			Items: []rank.Candidate{},
			Meta: rank.Meta{
				Seeds:     len(seeds),
				Tune:      req.Tune,
				Shortfall: &rank.Short{Requested: n, Returned: 0, Reason: "no candidates share any seed tag"},
			},
		}, nil
	}
	// Rank from slim rows: stats and tags, no summary. A 39-chapter fic's
	// summary is a paragraph of prose, and loading one for every candidate
	// in a 500-strong pool cost 25 MiB per request against the 60 MiB lite
	// cap. The summary is fetched afterwards for the rows that survive
	// ranking — a different number by an order of magnitude.
	candEnts, err := e.Corpus.CandidateRowsSlim(ctx, candIDs)
	if err != nil {
		return nil, fmt.Errorf("load candidates: %w", err)
	}
	cands, err := e.toCandidates(ctx, candEnts)
	if err != nil {
		return nil, fmt.Errorf("load candidate tags: %w", err)
	}

	// Signals.
	sigs, tune := e.signals(seeds)
	ranked, meta, err := rank.Score(ctx, cands, seeds, sigs, tune, e)
	if err != nil {
		return nil, err
	}
	meta.Seeds = len(seeds)
	if meta.Tune == "" {
		meta.Tune = tune.Name
	}

	// Diversify. The result is deliberately not in score order: an item
	// chosen for diversity ranks below one it beat on score.
	var groupFn func(rank.Candidate) string
	switch req.GroupBy {
	case "tag":
		groupFn = diversify.GroupByTag
	case "author":
		groupFn = diversify.GroupByAuthor
	case "fandom":
		// The one group key that had to be built rather than borrowed.
		//
		// The obvious implementation is a `tag_type='fandoms'` filter, and
		// measured against this mirror it returns nothing useful: 51 of
		// 113,995 works carry such a row, because the blurb scraper
		// flattened every tag into freeforms. A cap keyed on that column has
		// an empty key set, excludes nothing, and reports success — which is
		// exactly what the previous deployment's --max-per-fandom did.
		//
		// This groups on fandom tag NAME SHAPE instead, so the cap actually
		// bites on the corpus as it exists.
		groupFn = func(c rank.Candidate) string { return fandom.GroupKey(c.TagNames) }
	}
	if req.MaxPerGroup > 0 && groupFn == nil {
		// A requested cap with no group function is a no-op cap, and a no-op
		// cap is the failure mode this whole feature was built to remove. Say
		// so in the metadata rather than returning a list that looks
		// diversified and is not.
		meta.Shortfall = &rank.Short{
			Requested: n, Returned: len(ranked),
			Reason: "max-per-group was set but no group was chosen; pass group_by=fandom",
		}
	}
	div := diversify.Apply(ranked, diversify.Options{
		Requested:   n,
		Lambda:      0.75,
		MaxPerGroup: req.MaxPerGroup,
		GroupOf:     groupFn,
		MMRK:        500,
	})
	if div.Shortfall != nil {
		meta.Shortfall = div.Shortfall
	}
	if len(div.Items) == 0 {
		meta.Shortfall = &rank.Short{Requested: n, Returned: 0, Reason: "no candidate had an opinion"}
	}

	for i := range div.Items {
		signal.SortEvidence(div.Items[i].Evidence)
	}
	if err := e.attachSummaries(ctx, div.Items); err != nil {
		// A missing summary is not a failed request: the ranking is done
		// and the summary is decoration on top of it. Report it in the
		// log rather than turning a good ranking into an error.
		slog.Warn("could not attach summaries", "err", err)
	}
	if div.Items == nil {
		div.Items = []rank.Candidate{}
	}

	seedNamesOut := seedNames(req.Seeds)
	return &Result{
		Kind:  kind,
		Seeds: seedNamesOut,
		Items: div.Items,
		Meta:  meta,
		Tune:  tune.Weights,
	}, nil
}

func seedNames(seeds []Seed) []string {
	out := make([]string, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, s.Kind+":"+fmt.Sprint(s.ID))
	}
	return out
}

// poolFor finds candidate ids: works sharing a tag with any seed,
// excluding the seeds themselves.
func (e *Engine) poolFor(ctx context.Context, seeds []rank.Candidate, limit int, exclude bool, seedIDs []int64) ([]int64, error) {
	tagIDs := map[int32]bool{}
	for _, s := range seeds {
		for _, id := range s.TagIDs {
			tagIDs[id] = true
		}
	}
	if len(tagIDs) == 0 {
		return nil, nil
	}

	args := make([]any, 0, len(tagIDs)+2)
	for id := range tagIDs {
		args = append(args, id)
	}
	// The LIMIT is pushed into SQL. The first version selected every work
	// sharing any seed tag and truncated in Go: 8 seed tags over 112,935
	// works returned tens of thousands of rows, each with a tag id, and
	// the truncation threw them all away after materialising them. A
	// budget-gate trace measured that at +33 MiB on a pool of 200 — 165 KiB
	// retained per candidate, against a candidate that is a few hundred
	// bytes. The cap belongs where the work is.
	//
	// ORDER BY shared DESC, work_id makes the cut deterministic: the same
	// seeds must produce the same pool, or two requests with the same
	// input rank different candidates.
	args = append(args, limit, limit)
	q := `SELECT work_id, COUNT(*) AS shared FROM work_tags
	      WHERE tag_id IN (` + corpus.Placeholders(len(tagIDs)) + `)
	      GROUP BY work_id
	      ORDER BY shared DESC, work_id ASC
	      LIMIT ?`
	// Only the tag ids are bound before the limit; the second placeholder
	// is the offset-free limit itself, so append just once.
	args = args[:len(tagIDs)+1]
	rows, err := e.Corpus.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pool query: %w", err)
	}
	type scored struct {
		id     int64
		shared int
	}
	var pool []scored
	excluded := map[int64]bool{}
	if exclude {
		for _, id := range seedIDs {
			excluded[id] = true
		}
	}
	for rows.Next() {
		var id int64
		var shared int
		if err := rows.Scan(&id, &shared); err != nil {
			rows.Close()
			return nil, err
		}
		if excluded[id] {
			continue
		}
		pool = append(pool, scored{id: id, shared: shared})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Most-shared-first, then by id so the pool is reproducible. Truncating
	// before scoring is what bounds the request's work.
	// SQL already ordered and limited this; the sort is not repeated here.
	// Re-sorting a truncated slice would be harmless but would suggest the
	// ordering was not guaranteed, and a reader cannot tell whether the
	// LIMIT is trustworthy.
	out := make([]int64, 0, len(pool))
	for _, p := range pool {
		out = append(out, p.id)
	}
	return out, nil
}

// signals assembles the signal set and the tune to score it with.
func (e *Engine) signals(seeds []rank.Candidate) ([]rank.Signal, rank.Tune) {
	tune := DefaultTune()

	// The seed tags are gathered once and shared: recomputing them per
	// candidate is the per-candidate-recomputation cost class that
	// dominated the previous engine (profiling one seed there found
	// 123.8M generator evaluations from invariants that never varied).
	seedTagSet := map[int32]bool{}
	seedTags := make([]int32, 0, 64)
	for _, s := range seeds {
		for _, id := range s.TagIDs {
			if !seedTagSet[id] {
				seedTagSet[id] = true
				seedTags = append(seedTags, id)
			}
		}
	}

	var sigs []rank.Signal
	sigs = append(sigs, signal.TagOverlap{MaxTags: 200})

	topN := e.TopN
	if topN <= 0 {
		topN = 24
	}
	if e.Graph != nil {
		sigs = append(sigs, signal.Neighbourhood{
			G:              e.Graph,
			SeedTags:       seedTags,
			TotalWorks:     e.totalWorks(),
			NeighbourLimit: topN,
		})
	}
	sigs = append(sigs, signal.Quality{}, signal.Recency{Now: nowDays()}, signal.Popularity{})

	// peer_rating is the arena's contribution to ranking, and it is the
	// only signal here that is evidence of what a reader actually chose
	// rather than a restatement of the work's own metadata. SPEC §1 lists it
	// as a named signal and §7.1 requires its absence to be reported in
	// meta.degraded[] -- kindling's exact failure was an arena signal that
	// was inert in production and silent about it.
	//
	// Registered UNCONDITIONALLY, including when the arena has rated
	// nothing. Guarding this on `e.ArenaRatings != nil` would be the bug
	// restated: a signal that is only present when it has data is a signal
	// that is absent without saying so. With no ratings it scores
	// rank.ErrSkip for every candidate, and rank names it in
	// meta.degraded[] on every recommendation -- which is the observable
	// difference between "the arena has no opinion" and "the arena signal
	// does not work".
	arenaRatings, arenaMedian := e.ArenaSignal()
	sigs = append(sigs, signal.PeerRating{
		Ratings: arenaRatings,
		Median:  arenaMedian,
	})

	if !e.Lite {
		sigs = append(sigs, signal.Embedding{
			Store:   e,
			SeedVec: e.seedVector(seeds),
		})
		// Without embeddings the weights must still sum to something
		// sensible, so the remaining signals absorb the difference rather
		// than every score shrinking by 10%.
		tune.Weights = renormalise(tune.Weights, "embedding")
	}
	return sigs, tune
}

// renormalise rescales the surviving weights to sum to the same total, so
// removing a dimension changes the ranking's balance but not its scale.
func renormalise(w map[string]float64, drop string) map[string]float64 {
	lost := w[drop]
	delete(w, drop)
	if lost == 0 {
		return w
	}
	var total float64
	for _, v := range w {
		total += v
	}
	if total == 0 {
		return w
	}
	scale := (total + lost) / total
	for k, v := range w {
		w[k] = v * scale
	}
	return w
}

func (e *Engine) seedVector(seeds []rank.Candidate) []float32 {
	vecs := make([][]float32, 0, len(seeds))
	for _, s := range seeds {
		v, ok, err := e.Embedding(context.Background(), s.Kind, s.ID)
		if err == nil && ok && len(v) > 0 {
			vecs = append(vecs, v)
		}
	}
	return signal.MeanVec(vecs)
}

// attachSummaries fills in the summary text for the ranked results.
//
// The pool is loaded without summaries and the survivors get them here,
// so the cost scales with the response size rather than the pool size: 20
// summaries instead of 500.
func (e *Engine) attachSummaries(ctx context.Context, items []rank.Candidate) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(items))
	index := make(map[int64]int, len(items))
	for i, it := range items {
		ids = append(ids, it.ID)
		index[it.ID] = i
	}
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := e.Corpus.DB.QueryContext(ctx,
		`SELECT id, COALESCE(summary,'') FROM works WHERE id IN (`+corpus.Placeholders(len(ids))+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var summary string
		if err := rows.Scan(&id, &summary); err != nil {
			return err
		}
		if i, ok := index[id]; ok {
			items[i].Summary = summary
		}
	}
	return rows.Err()
}

// Embedding implements the store interface the embedding signal reads.
func (e *Engine) Embedding(ctx context.Context, kind string, id int64) ([]float32, bool, error) {
	var blob []byte
	var dim int
	// Kind is part of the lookup: work 1 and tag 1 are different entities,
	// and a lookup that ignored kind would return whichever one happened
	// to be written.
	err := e.Store.DB.QueryRowContext(ctx,
		`SELECT dim, vec FROM embeddings WHERE kind = ? AND entity_id = ?`,
		kind, id).Scan(&dim, &blob)
	if err != nil {
		return nil, false, nil
	}
	if dim <= 0 || len(blob) != dim*4 {
		return nil, false, nil
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		out[i] = float32FromLe(blob[i*4:])
	}
	return out, true, nil
}

// PeerRatings is the hook for a crowd-signal source. The previous
// deployment's arena ratings live in its own tables and are read through
// an adapter if the operator wants them; absent that, the signal skips
// rather than reporting a zero.
func (e *Engine) PeerRatings(ctx context.Context, kind string, ids []int64) (map[int64]float64, error) {
	return nil, nil
}

func (e *Engine) totalWorks() float64 {
	if e.Store == nil {
		return 0
	}
	v, err := e.Store.MetaInt(context.Background(), "work_count")
	if err != nil {
		return 0
	}
	return float64(v)
}

func nowDays() float64 { return float64(time.Now().Unix() / 86400) }

// float32FromLe reads one little-endian float32. The value comes out of
// SQLite's byte-order blob, so the encoding is pinned here rather than
// assumed: a mirrored byte-order pair agrees with itself, so a round-trip
// test cannot catch this getting it wrong.
func float32FromLe(b []byte) float32 {
	return math.Float32frombits(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
}

// toCandidates converts corpus entities into rank candidates, pairing each
// entity's tag ids with their names.
//
// Entity.Tags already carries the names from the ingest join, but the
// graph is keyed by id, so the ids are read from TagPairs and matched by
// name. The match is exact and the unmatched remainder is dropped rather
// than guessed: a tag id with the wrong name attached would rank a work
// against a tag it does not carry, and the response would show a name
// that contradicts the score.
func (e *Engine) toCandidates(ctx context.Context, ents []corpus.Entity) ([]rank.Candidate, error) {
	ids := make([]int64, 0, len(ents))
	for _, en := range ents {
		ids = append(ids, en.ID)
	}
	pairs, err := e.Corpus.TagPairs(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]rank.Candidate, 0, len(ents))
	for _, en := range ents {
		tagIDs := make([]int32, 0, len(pairs[en.ID]))
		tagNames := make([]string, 0, len(pairs[en.ID]))
		for _, p := range pairs[en.ID] {
			tagIDs = append(tagIDs, p.ID)
			tagNames = append(tagNames, p.Name)
		}
		out = append(out, rank.Candidate{
			ID:       en.ID,
			Kind:     en.Kind,
			Title:    en.Title,
			URL:      en.URL,
			Summary:  en.Summary,
			Stats:    en.Stats,
			TagNames: tagNames,
			TagIDs:   tagIDs,
		})
	}
	return out, nil
}
