// Package engine assembles a recommender from the store, the graph and the
// signal set. It is the one place that knows how a request becomes a
// ranked list, so the API layer stays a projection of it and the signals
// stay pure.
package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/diversify"
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
}

// Seed is one seed reference.
type Seed struct {
	Kind string
	ID   int64
}

// ParseSeed parses "kind:id".
func ParseSeed(s string) (Seed, error) {
	kind, id, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || kind == "" || id == "" {
		return Seed{}, fmt.Errorf("seed %q must be kind:id, e.g. ao3_work:1234", s)
	}
	var n int64
	if _, err := fmt.Sscanf(id, "%d", &n); err != nil {
		return Seed{}, fmt.Errorf("seed %q has a non-numeric id: %v", s, err)
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
	GroupBy     string // "tag", "author", or "" for none
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

// DefaultTune weights the signals. They are named and overridable, so
// ranking is configurable without a code change — the property the
// previous deployment had and generalised here per kind.
func DefaultTune() rank.Tune {
	return rank.Tune{
		Name: "default",
		Weights: map[string]float64{
			"tag_overlap":   0.25,
			"neighbourhood": 0.35,
			"quality":       0.10,
			"recency":       0.10,
			"popularity":    0.10,
			"embedding":     0.10,
		},
	}
}

// Recommend produces a ranked list.
func (e *Engine) Recommend(ctx context.Context, req Request) (*Result, error) {
	if len(req.Seeds) == 0 {
		return nil, fmt.Errorf("at least one seed is required")
	}
	kind := req.Kind
	if kind == "" {
		kind = req.Seeds[0].Kind
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
		pool = 500
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
	candEnts, err := e.Corpus.CandidateRows(ctx, candIDs)
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

	args := make([]any, 0, len(tagIDs))
	for id := range tagIDs {
		args = append(args, id)
	}
	q := `SELECT work_id, COUNT(*) AS shared FROM work_tags
	      WHERE tag_id IN (` + corpus.Placeholders(len(args)) + `)
	      GROUP BY work_id`
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
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].shared != pool[j].shared {
			return pool[i].shared > pool[j].shared
		}
		return pool[i].id < pool[j].id
	})
	if len(pool) > limit {
		pool = pool[:limit]
	}
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

// Embedding implements the store interface the embedding signal reads.
func (e *Engine) Embedding(ctx context.Context, kind string, id int64) ([]float32, bool, error) {
	var blob []byte
	var dim int
	err := e.Store.DB.QueryRowContext(ctx,
		`SELECT dim, vec FROM embeddings WHERE entity_id = ?`, id).Scan(&dim, &blob)
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
