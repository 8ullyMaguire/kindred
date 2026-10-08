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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/collab"
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

	// Collab is the item-item co-bookmark index over the mirror's
	// `user_work_interactions`.
	//
	// Nil means the index was never built — a lite run, or a mirror with no
	// bookmark rows at all. That is NOT a reason to leave the collab signal
	// out of the set: the signal skips for every candidate and names itself
	// in meta.degraded[], so the absence is reported rather than inferred
	// from a ranking that quietly lost a dimension.
	Collab *collab.Index
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

	// PoolMode selects how candidates enter the pool.
	PoolMode PoolMode

	// BlockedTagIDs are tags the reader has explicitly blocked. A candidate
	// carrying ANY of them cannot enter the pool.
	//
	// This is a request field rather than something the engine reads from the
	// store, for the same reason Filter is a field: the engine must be able to
	// rank with or without a store at all (the CLI ranks against a bare
	// corpus), and a reader identity is not the engine's to resolve.
	BlockedTagIDs map[int64]bool

	// SeenIDs are entities this reader has already been shown or has read.
	// They cannot enter the pool.
	//
	// Ids are namespaced strings ("ao3_work:123") rather than int64s because
	// the same field serves every kind, and a tag id 42 and a work id 42 are
	// different things that must not collide in one exclusion set.
	SeenIDs map[string]bool

	// Filters narrow the CANDIDATE POOL, inside the query, before anything
	// is scored. That placement is the whole point.
	//
	// Filtering a finished result list returns a subset of a RANKING, which
	// is not the same as a ranking over a smaller pool. Ask for the 20 best
	// works over 20k words and post-filter, and you get the top 20 of the
	// unfiltered pool that happened to be long — omitting exactly the works
	// a deeper query would have promoted into the gap. The filters belong
	// where the work is.
	//
	// Zero means "no such filter" for each, with one exception: Complete is a
	// *tri-state because `complete` is a NULLable column and NULL is UNKNOWN,
	// not false. CompleteAny means "do not filter"; CompleteOnly and
	// CompleteWIP then say which of the two known states is wanted.
	Filter Filter
}

// PoolMode selects how candidates enter the pool.
//
// It exists because tag-based pooling makes collaborative filtering nearly
// inert. A work enters a tag pool by sharing a tag with a seed, so the works
// that collaborative filtering exists to surface — the sequel your co-readers
// read but which shares no tag with the seed — can never be scored at all.
// The signal would be registered, weighted, non-degraded, and unable to
// change the ranking on exactly the cases that justify it.
//
// The modes are additive rather than exclusive: the union keeps the old
// behaviour intact when no index is present, so a mirror without bookmarks
// behaves identically to before rather than returning nothing.
type PoolMode int

const (
	// PoolTags is the historical behaviour: candidates share a tag with a
	// seed. The zero value, so a Request that never sets PoolMode behaves
	// as it always did.
	PoolTags PoolMode = iota
	// PoolTagsOrCollab adds works co-bookmarked with a seed.
	PoolTagsOrCollab
)

func (p PoolMode) String() string {
	switch p {
	case PoolTagsOrCollab:
		return "tags+collab"
	default:
		return "tags"
	}
}

// ErrUnknownTune is returned when a request names a tune that does not exist.
//
// It exists because the alternative is the accepted-and-ignored shape: before
// this, `?tune=` was recorded into the response metadata and had no effect on
// ranking whatsoever. A reader who picked a tune got a normal-looking list
// ranked by weights they had not chosen, with the tune name echoed at the
// bottom as though it had been applied.
var ErrUnknownTune = errors.New("unknown tune")

// resolveTune turns a requested tune name into the weight overrides to apply,
// or an error.
//
// "" and "default" mean the built-in weights and return nil overrides, which
// is the common case. Any other name must exist in the store: an engine with
// no store cannot resolve a named tune and says so rather than pretending the
// default is what was asked for.
func (e *Engine) resolveTune(ctx context.Context, name string) (string, map[string]float64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "default", nil, nil
	}
	if name == "default" {
		return "default", nil, nil
	}
	if e.Store == nil || e.Store.DB == nil {
		return "", nil, fmt.Errorf("%w %q: this engine has no store to load tunes from",
			ErrUnknownTune, name)
	}
	weights, err := e.Store.Tune(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil, fmt.Errorf("%w %q; available: %s",
				ErrUnknownTune, name, strings.Join(availableTunes(ctx, e.Store), ", "))
		}
		return "", nil, fmt.Errorf("tune %q: %w", name, err)
	}
	return name, weights, nil
}

// availableTunes lists the names to suggest when one is unknown. "default"
// leads because it always works, which makes the error message actionable
// rather than merely corrective.
func availableTunes(ctx context.Context, s *store.Store) []string {
	out := []string{"default"}
	if s == nil {
		return out
	}
	names, err := s.TuneNames(ctx)
	if err != nil {
		return out
	}
	out = append(out, names...)
	return out
}

// ParsePoolMode validates a pool mode name.
func ParsePoolMode(s string) (PoolMode, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "tags", "tag":
		return PoolTags, nil
	case "tags+collab", "collab", "tags_or_collab":
		return PoolTagsOrCollab, nil
	}
	// The message names the PARAMETER, not just the concept. "unknown pool
	// mode" is not actionable from a page that has several controls: the
	// reader cannot tell whether the problem is pool_mode, pool or the tune
	// select. The filter errors name their parameters and this now matches,
	// which the browser suite checks by asserting the parameter name appears
	// in the error body.
	return PoolTags, fmt.Errorf(
		"pool_mode %q is not a mode; use pool_mode=tags, or pool_mode=tags+collab "+
			"to admit co-bookmarked works", s)
}

// Filter narrows the candidate pool. It is a value, not a pointer, so the
// zero value is the no-filter case and cannot be confused with "filter set to
// nothing".
type Filter struct {
	// MinWords and MaxWords bound word_count. Zero disables that bound, so
	// "no minimum" and "minimum of zero" are the same request — which is
	// correct, because a work with no words is not in the corpus to begin
	// with.
	MinWords int64
	MaxWords int64

	// MinKudos is a floor on kudos. Zero disables it, and kudos=0 is a real
	// value a reader can filter for, so the honest spelling for "any kudos"
	// is an unset filter. Callers wanting kudos>=0 use no filter, which is
	// the same set.
	MinKudos int64

	// Rating is a set of AO3 rating names to keep, case-insensitively. Empty
	// means no rating filter. These are full names ("Explicit", "Teen And Up
	// Audiences"), not the slash letters the /api/v1/ao3/works endpoint
	// accepts: the engine works in the mirror's own vocabulary, and
	// translating between them in two places is how the two disagree.
	Ratings []string

	// Language is a set of language codes to keep, lowercased. Empty means
	// no language filter.
	Languages []string

	// Complete is the tri-state completion filter.
	Complete TriBool

	// NotUpdatedWithinDays keeps only works whose last revision is OLDER
	// than the window: a "settled reading" filter. The owner's use is
	// "updated more than 4 months ago" — 122 days — which excludes both
	// works still receiving updates and works revised so recently that
	// their shape may still change. Zero disables it.
	//
	// It is NotUpdated rather than Updated because the common ask is the
	// stale side: "recently revised" is one comparison away
	// (NotUpdatedWithinDays = 0 keeps the filter inert) and would need a
	// second field to express the floor. The predicate lives in filterSQL
	// beside the other pool filters, never after ranking.
	NotUpdatedWithinDays int64
}

// TriBool is a three-valued boolean: the zero value is the "unset" case.
//
// It exists because SQLite has no boolean NULL and the mirror's `complete`
// column does. Collapsing that to Go's bool forces a choice between treating
// NULL as false (hides unknown works behind a confident "in progress") and
// treating it as true (claims the mirror knows something it does not). Both
// were measured as real rows, not hypotheticals.
type TriBool int

const (
	// CompleteAny does not filter on completion.
	CompleteAny TriBool = iota
	// CompleteOnly keeps works flagged complete.
	CompleteOnly
	// CompleteWIP keeps works not flagged complete.
	CompleteWIP
)

func (t TriBool) String() string {
	switch t {
	case CompleteOnly:
		return "complete"
	case CompleteWIP:
		return "in-progress"
	default:
		return "any"
	}
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
			// collab is the mirror's own collaborative filtering, and it is
			// the only signal here built from evidence of what readers CHOSE
			// rather than from the works' own metadata. It earns weight in
			// proportion to that: above embedding, below neighbourhood.
			//
			// It starts well below peer_rating on purpose. peer_rating is a
			// deliberate, current signal from a reader in this deployment;
			// collab is a historical one from whatever six thousand readers
			// the mirror happened to sample, and a sample of 6,261 out of
			// AO3's millions is a narrow slice of taste.
			"collab": 0.16,
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
	// 200: the CLI/web ask for up to 200 recommendations (the API test
	// clamps hard here; n=100000 must not materialise a million rows).
	if n > 200 {
		n = 200
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
	//
	// The pool and the signals are built from the same seed set, so the
	// collab votes are computed once here and handed to both. Computing them
	// in signals() separately would be a second walk of the same adjacency
	// for the same request.
	poolIDs, collabVotes, err := e.poolFor(ctx, seeds, pool, req.Exclude, seedIDs, req)
	if err != nil {
		return nil, err
	}
	// Normalise the vote set ONCE, here, rather than inside signals(): the
	// normaliser scans the map for its maximum, and the pool builder and the
	// signal both need the answer.
	collabScale := defaultCollabScale
	if max := (signal.CollabNormaliser{}).Normalise(collabVotes); max > 0 {
		// Normalise against the observed maximum rather than a constant, so
		// a one-seed request with four neighbours still spans the range
		// instead of scoring everything under 0.02 and reporting itself as
		// "measured and mediocre".
		collabScale = max
	}
	candIDs := poolIDs
	if len(candIDs) == 0 {
		// The tune is resolved here rather than echoed from the request. On
		// this path nothing has scored, so reporting `req.Tune` would claim
		// a tune was applied that was never looked up — and an unknown
		// tune would return an empty list rather than saying so.
		tuneName, _, terr := e.resolveTune(ctx, req.Tune)
		if terr != nil {
			return nil, terr
		}
		return &Result{
			Kind:  kind,
			Seeds: seedNames(req.Seeds),
			Items: []rank.Candidate{},
			Meta: rank.Meta{
				Seeds:     len(seeds),
				Tune:      tuneName,
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
	// The named tune is resolved HERE, before any signal runs, so an unknown
	// name is a visible error rather than a ranking by weights the reader
	// did not ask for. Previously `?tune=` was recorded into meta and
	// nothing else: signals() always used DefaultTune, so the parameter was
	// accepted, echoed back, and inert.
	tuneName, tuneWeights, err := e.resolveTune(ctx, req.Tune)
	if err != nil {
		return nil, err
	}
	sigs, tune := e.signals(seeds, collabVotes, collabScale, tuneWeights, tuneName)
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

// poolFor finds candidate ids: works sharing a tag with any seed, honouring
// the request's filters, excluding the seeds themselves.
//
// Every filter is applied in SQL against `works`, BEFORE the LIMIT. That is
// the load-bearing decision and it is not the obvious one:
//
//   - Filtering after ranking returns a subset of a ranking rather than a
//     ranking over a smaller pool, so the results are systematically the
//     top-N of the unfiltered pool rather than the best N that qualify.
//   - Applying a filter after the LIMIT is worse still: the LIMIT is taken
//     over unfiltered rows, so a strict filter can empty the pool entirely
//     even though thousands of qualifying works exist further down.
//
// Both failures report success and return a short or empty list.
func (e *Engine) poolFor(ctx context.Context, seeds []rank.Candidate, limit int, exclude bool, seedIDs []int64, req Request) ([]int64, map[int64]float64, error) {
	tagIDs := map[int32]bool{}
	for _, s := range seeds {
		for _, id := range s.TagIDs {
			tagIDs[id] = true
		}
	}
	f := req.Filter

	// Collab neighbours are resolved FIRST, before the early return below, so
	// a seed with no tags can still be recommended from: a work sharing no
	// tag with the seed is precisely the case collaborative filtering is
	// for, and an early return on len(tagIDs)==0 would make it unreachable
	// for every tagless seed.
	var collabVotes map[int64]float64
	if req.PoolMode == PoolTagsOrCollab && e.Collab != nil {
		collabVotes = signal.CollabVotes(e.Collab, seedIDs, collabNeighbourLimit(e.TopN))
	}

	if len(tagIDs) == 0 {
		if len(collabVotes) == 0 {
			return nil, nil, nil
		}
		ids := collabCandidates(collabVotes, limit, seedIDs, 0, f, req.BlockedTagIDs, req.SeenIDs, e.Corpus)
		return ids, collabVotes, nil
	}

	args := make([]any, 0, len(tagIDs)+16)
	for id := range tagIDs {
		args = append(args, id)
	}
	tagArgCount := len(tagIDs)

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
	//
	// Sorted tag ids rather than map order, for the same reason: the
	// argument list is part of the cache key and the pool must not depend
	// on Go's randomised map iteration.
	tagList := make([]int32, 0, len(tagIDs))
	for id := range tagIDs {
		tagList = append(tagList, id)
	}
	sort.Slice(tagList, func(a, b int) bool { return tagList[a] < tagList[b] })
	args = args[:0]
	for _, id := range tagList {
		args = append(args, id)
	}

	where := `wt.tag_id IN (` + corpus.Placeholders(tagArgCount) + `)`
	// The filter predicates are appended BEFORE the limit argument, because
	// the limit's placeholder is the last one in the statement. Binding them
	// out of order is not a compile error and not a runtime error in SQLite
	// for a well-typed query — it is a silently wrong result, which is why
	// this ordering is written as a single explicit sequence rather than
	// interleaved with the SQL string.
	// The tag half of the pool is capped at the budget MINUS the room
	// reserved for collaborative filtering.
	//
	// This is the bug that made PoolTagsOrCollab a no-op, and it is worth
	// naming precisely because the code LOOKED correct. The SQL took
	// `LIMIT limit`, so the tag pool always filled every slot, and the union
	// below computed `room := limit - len(out)` — which is then always zero,
	// so collab contributed nothing and returned. Measured on the live
	// mirror: `pool_mode=tags` and `pool_mode=tags+collab` produced
	// BYTE-IDENTICAL 8-item lists, and none of the seed's four known
	// co-bookmarked neighbours appeared in either.
	//
	// Reserving a fixed share is a deliberate trade, not a compromise to
	// paper over that. Tag pooling is the stronger half on this mirror —
	// measured neighbourhoods are 1-4 works per seed against thousands of
	// tag matches — so reserving a large share would hand most of the budget
	// to a much thinner signal. The share is therefore small and explicit,
	// and the collab half goes first when the budget is tight, because those
	// works are unreachable any other way.
	tagBudget := limit
	if req.PoolMode == PoolTagsOrCollab {
		tagBudget = limit - collabReserve(limit)
		if tagBudget < 1 {
			// A pool of 1 cannot be split. Tags win the slot, because on this
			// corpus they are the half that can actually rank.
			tagBudget = 1
		}
	}
	q := `SELECT wt.work_id, COUNT(*) AS shared FROM work_tags wt
	      JOIN works w ON w.id = wt.work_id
	      WHERE ` + where
	q += filterSQL(f, &args)
	// Blocks are applied HERE, inside the same pool query, for the same
	// reason the filters are. Filtering the finished ranking would return a
	// subset of a ranking rather than a ranking over the works the reader is
	// willing to see: ask for 20 and get 8, with no way to tell that 60
	// eligible works sat below the cut-off behind blocked ones.
	q += blockSQL(req.BlockedTagIDs, &args)
	q += seenSQL(req.SeenIDs, &args)
	// ORDER BY shared DESC, work_id ASC. The work_id tiebreak is what makes
	// two identical requests rank identical candidates.
	q += ` GROUP BY wt.work_id
	       ORDER BY shared DESC, wt.work_id ASC
	       LIMIT ?`
	args = append(args, tagBudget)

	rows, err := e.Corpus.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("pool query: %w", err)
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
			return nil, nil, err
		}
		if excluded[id] {
			continue
		}
		pool = append(pool, scored{id: id, shared: shared})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()

	// Most-shared-first, then by id so the pool is reproducible. Truncating
	// before scoring is what bounds the request's work.
	// SQL already ordered and limited this; the sort is not repeated here.
	// Re-sorting a truncated slice would be harmless but would suggest the
	// ordering was not guaranteed, and a reader cannot tell whether the
	// LIMIT is trustworthy.
	out := make([]int64, 0, len(pool))
	inPool := make(map[int64]bool, len(pool))
	for _, p := range pool {
		out = append(out, p.id)
		inPool[p.id] = true
	}

	if len(collabVotes) == 0 {
		return out, nil, nil
	}

	// UNION, not replacement, and the tag half keeps its full budget.
	//
	// Replacing the tag pool with collab neighbours would make the ranking
	// depend entirely on whether the mirror happened to sample bookmark rows
	// for this seed's neighbourhood — the recommender would silently stop
	// being content-based. So the tag pool keeps most of `limit` and collab
	// gets a reserved share, which is the additive behaviour that makes
	// PoolMode safe to expose as a request parameter.
	//
	// `limit - len(out)` is correct now BECAUSE the tag half was capped below
	// `limit` above. Before that split it was always zero, which is how the
	// whole union stayed unreachable while reading as a working union.
	room := limit - len(out)
	if room <= 0 {
		return out, collabVotes, nil
	}
	extra := collabCandidates(collabVotes, room, seedIDs, 0, f, req.BlockedTagIDs, req.SeenIDs, e.Corpus)
	out = append(out, extra...)
	return out, collabVotes, nil
}

// seenSQL excludes entities the reader has already been shown or has read.
//
// Applied in the pool query for the same reason the blocks are: filtering
// the finished ranking returns a subset of a ranking rather than a ranking
// over the works still worth showing. The difference is visible to a reader:
// ask for 20 with 15 already seen and post-filtering returns 5, while the
// pool query returns 20 NEW ones.
//
// The ids are TEXT because they are namespaced ("ao3_work:123"). A work id
// and a tag id share a namespace here only as strings, so comparing them as
// integers would exclude a tag-seeded request's works whenever a work with
// the same numeric id had been seen.
func seenSQL(seen map[string]bool, args *[]any) string {
	if len(seen) == 0 {
		return ""
	}
	ids := make([]string, 0, len(seen))
	for key := range seen {
		// Trimmed, and blank ids dropped, to match store.cleanEntityIDs.
		// They must agree: the store writes trimmed keys, so a predicate that
		// admitted a padded key would emit a value that can never match
		// anything — a silently useless exclusion that still costs a scan.
		if id := strings.TrimSpace(key); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	// Sorted for the same reason blockSQL sorts: identical requests must
	// compile to identical SQL so SQLite's plan cache hits.
	sort.Strings(ids)
	vals := make([]any, len(ids))
	for i, id := range ids {
		vals[i] = id
	}
	*args = append(*args, vals...)
	// `kind || ':' || CAST(id AS TEXT)` is how the seed namespacing works
	// elsewhere, so an exclusion set built from "ao3_work:1" matches a row
	// the pool selected as work 1.
	return ` AND ('ao3_work:' || CAST(w.id AS TEXT)) NOT IN (` +
		corpus.Placeholders(len(ids)) + `)`
}

// collabReserve is how many of a pool's slots are held back for
// collaborative filtering when PoolMode is PoolTagsOrCollab.
//
// Small and explicit, and the ratio is the measurement rather than a taste:
// on the live mirror a seed's co-bookmark neighbourhood is 1-4 works
// (6,261 bookmarkers spread over 112,935 works), while tag pooling offers
// thousands. Reserving a quarter of a 200-pool would hand 50 slots to a
// signal that can fill at most a handful.
//
// It is a MINIMUM share, not a cap: the tag half is reduced by this much, so
// collab can use it plus any slots the tag half left unfilled. A tag pool
// thinner than the budget therefore hands the remainder to collab, which is
// the right direction — an underfull pool is a pool with room to fill.
//
// Never exceeds limit-1, so the tag half always keeps at least one slot.
func collabReserve(limit int) int {
	if limit <= 2 {
		// Too small to divide. Tags take everything; at a pool of 2 the
		// collab half would be a coin flip rather than a ranking.
		return 0
	}
	// An eighth, rounded up, so a small pool still gets a collab slot: at a
	// pool of 8 that is one, which is enough to change a list.
	r := (limit + 7) / 8
	if r > limit-1 {
		return limit - 1
	}
	return r
}

// collabCandidates picks the top-scoring collab neighbours that are not
// already in the pool and that pass the request's filters.
//
// It filters in SQL for the same reason poolFor does: the filters narrow the
// CANDIDATE SET, so applying them after choosing the top N by vote would
// return fewer than N even when qualifying works exist further down the vote
// ranking. `skip` is how many of the highest-voted works were already in the
// pool, so they are not considered again.
//
// The SQL is only built when there is something to filter: with no filter the
// ranked votes are already the answer, and a needless join over 112,935 works
// to rediscover them would be the kind of work this package avoids.
func collabCandidates(votes map[int64]float64, limit int, seedIDs []int64, skip int, f Filter, blocked map[int64]bool, seen map[string]bool, ao3 *corpus.AO3) []int64 {
	if limit <= 0 || len(votes) == 0 {
		return nil
	}
	// Seeds are dropped unconditionally rather than behind an `exclude`
	// flag. Recommending the work a reader seeded from is never useful, and
	// a flag here would only ever have been set to false to make a ranking
	// look more interesting.
	seedSet := make(map[int64]bool, len(seedIDs))
	for _, id := range seedIDs {
		seedSet[id] = true
	}

	// Rank by vote, then id, so the same vote set always yields the same
	// candidates. Map iteration order is randomised by Go, and a pool that
	// reshuffles between identical requests is a pool that changes rankings.
	type cand struct {
		id   int64
		vote float64
	}
	all := make([]cand, 0, len(votes))
	for id, v := range votes {
		if seedSet[id] {
			continue
		}
		all = append(all, cand{id: id, vote: v})
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].vote != all[b].vote {
			return all[a].vote > all[b].vote
		}
		return all[a].id < all[b].id
	})
	if skip >= len(all) {
		return nil
	}
	all = all[skip:]

	if f.isEmpty() && len(blocked) == 0 && len(seen) == 0 {
		out := make([]int64, 0, limit)
		for i, c := range all {
			if i >= limit {
				break
			}
			out = append(out, c.id)
		}
		return out
	}

	// Filtered: bind the candidate ids and re-check the predicates in SQL.
	chunk := all
	if len(chunk) > limit*4 {
		// More than enough candidates to fill the limit, and bounding the
		// IN list keeps it under SQLite's variable limit without starving
		// the result.
		chunk = chunk[:limit*4]
	}
	if len(chunk) == 0 {
		return nil
	}
	ids := make([]any, len(chunk))
	for i, c := range chunk {
		ids[i] = c.id
	}
	args := append([]any{}, ids...)
	q := `SELECT w.id FROM works w WHERE w.id IN (` + corpus.Placeholders(len(chunk)) + `)`
	q += filterSQL(f, &args)
	q += blockSQL(blocked, &args)
	q += seenSQL(seen, &args)
	rows, err := ao3.DB.QueryContext(context.Background(), q, args...)
	if err != nil {
		// A failed filter query returns no candidates rather than
		// unfiltered ones. Returning the unfiltered set would silently
		// ignore a filter the reader asked for, which is the worse of the
		// two wrong answers.
		return nil
	}
	defer rows.Close()
	keep := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil
		}
		keep[id] = true
	}
	out := make([]int64, 0, limit)
	for _, c := range chunk {
		if len(out) >= limit {
			break
		}
		if keep[c.id] {
			out = append(out, c.id)
		}
	}
	return out
}

// blockSQL excludes candidates carrying any explicitly blocked tag.
//
// NOT EXISTS rather than NOT IN, and the reason is worth stating precisely
// because the obvious summary of it is WRONG.
//
// `x NOT IN (SELECT work_id FROM work_tags WHERE tag_id IN (...))` goes NULL
// — and therefore excludes the row — whenever the subquery yields a NULL.
// work_tags.work_id is NOT NULL, so that never happens here, and a NOT IN
// written this way is correct ON THIS SCHEMA. Measured, not assumed: with the
// subquery over work_id, an untagged work survives both forms; adding a
// `UNION ALL SELECT NULL` makes the NOT IN form drop it while NOT EXISTS
// keeps it.
//
// So this is NOT EXISTS because the predicate is then correct regardless of
// what the subquery can emit, not because NOT IN is broken on this table. A
// comment claiming the latter would be wrong, and it was — an earlier draft
// of this file asserted the NULL trap applies unconditionally. It does not.
// The cost is a correlated subquery instead of a hashed lookup, which on a
// pool query already counting work_tags rows is not the expensive half.
//
// It returns "" for an empty set so the common case adds no SQL at all.
//
// Note the tag id space is shared with the pool's own seed-tag selection:
// blocking a tag also stops a reader being recommended works that merely
// share it, which is the intent. It does NOT stop the tag being used as a
// SEED — a reader who asks "more like Death Note" while having blocked
// `death note` is contradicting themselves, and silently ignoring the seed
// would be worse than honouring it.
func blockSQL(blocked map[int64]bool, args *[]any) string {
	if len(blocked) == 0 {
		return ""
	}
	ids := make([]int64, 0, len(blocked))
	for id := range blocked {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	// Sorted so the same block set always produces byte-identical SQL, which
	// keeps SQLite's query plan cache hitting and makes the statement
	// comparable in a test.
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	vals := make([]any, len(ids))
	for i, id := range ids {
		vals[i] = id
	}
	*args = append(*args, vals...)
	return ` AND NOT EXISTS (
	            SELECT 1 FROM work_tags b
	            WHERE b.work_id = w.id AND b.tag_id IN (` +
		corpus.Placeholders(len(ids)) + `))`
}

// isEmpty reports whether a filter would narrow anything. It exists so
// collabCandidates can skip the SQL entirely in the common unfiltered case.
func (f Filter) isEmpty() bool {
	return f.MinWords == 0 && f.MaxWords == 0 && f.MinKudos == 0 &&
		f.Complete == CompleteAny && len(f.Ratings) == 0 && len(f.Languages) == 0 &&
		f.NotUpdatedWithinDays == 0
}

// filterSQL appends the filter predicates and binds their arguments.
//
// Every value is a BOUND parameter, never string-concatenated. A rating or
// language name arrives from a URL, and concatenation there is an injection
// into a read-only mirror — less dangerous than a write, but it is still the
// wrong shape and it would also break on a name containing a quote.
func filterSQL(f Filter, args *[]any) string {
	var s string
	add := func(pred string, vals ...any) {
		s += " AND " + pred
		*args = append(*args, vals...)
	}

	// word_count and kudos are NULLable in the mirror, so every comparison
	// uses COALESCE. Without it a NULL word_count makes `word_count >= ?`
	// NULL, the row vanishes, and the filter silently drops exactly the
	// uncrawled works a reader asking for "long" least wants dropped.
	if f.MinWords > 0 {
		add(`COALESCE(w.word_count,0) >= ?`, f.MinWords)
	}
	if f.MaxWords > 0 {
		add(`COALESCE(w.word_count,0) <= ?`, f.MaxWords)
	}
	if f.MinKudos > 0 {
		add(`COALESCE(w.kudos,0) >= ?`, f.MinKudos)
	}
	// `complete = 1`, not `complete != 0`: NULL means UNKNOWN, and
	// `complete != 0` is NULL for those rows so they would vanish from an
	// "in progress" request, which is precisely when a reader most wants to
	// see a work whose status is unknown.
	switch f.Complete {
	case CompleteOnly:
		add(`w.complete = 1`)
	case CompleteWIP:
		add(`w.complete = 0`)
	}
	if len(f.Ratings) > 0 {
		s += ` AND LOWER(COALESCE(w.rating,'')) IN (` + corpus.Placeholders(len(f.Ratings)) + `)`
		for _, r := range f.Ratings {
			*args = append(*args, strings.ToLower(r))
		}
	}
	if len(f.Languages) > 0 {
		s += ` AND LOWER(COALESCE(w.language,'')) IN (` + corpus.Placeholders(len(f.Languages)) + `)`
		for _, l := range f.Languages {
			*args = append(*args, strings.ToLower(l))
		}
	}
	// last_updated is ISO-8601 text, so string comparison IS chronological
	// comparison here — no julianday() conversion, no per-row function call.
	// The window is resolved by SQLite's own clock so the prepared statement
	// does not go stale as the corpus ages.
	if f.NotUpdatedWithinDays > 0 {
		add(`w.last_updated <= date('now', '-' || ? || ' days')`, f.NotUpdatedWithinDays)
	}
	return s
}

// signals assembles the signal set and the tune to score it with.
//
// The tune is resolved by NAME here rather than applied to req.Tune by the
// caller, because signals() is the one place that knows which signals exist.
// A caller-supplied weight map can name a signal that does not exist, which
// would be accepted and then have no effect — the accepted-and-ignored shape
// this project has found repeatedly.
//
// `overrides` is nil when the request named no tune.
func (e *Engine) signals(seeds []rank.Candidate, collabVotes map[int64]float64, collabScale float64, overrides map[string]float64, tuneName string) ([]rank.Signal, rank.Tune) {
	tune := DefaultTune()
	tune.Name = tuneName
	if len(overrides) > 0 {
		tune = tune.WithOverrides(overrides)
	}

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

	// collab is the mirror's own collaborative filtering: the 180,677
	// bookmark rows from 6,261 real readers, ranked item-item.
	//
	// The vote set and its normaliser are computed by the pool builder and
	// passed in, because they depend on the SEEDS and not on the candidate
	// pool — and because the pool builder needs the same walk to decide which
	// collab works enter the pool at all.
	//
	// Registered UNCONDITIONALLY, for the same reason peer_rating is and
	// with the same consequence. Guarding it on `e.Collab != nil` would
	// reproduce exactly the bug this repo has now found five times: a signal
	// that is absent without saying so. With no index it skips for every
	// candidate and names itself in meta.degraded[], so "this mirror has no
	// bookmark evidence" is distinguishable from "collaborative filtering is
	// broken".
	sigs = append(sigs, signal.Collab{Votes: collabVotes, Scale: collabScale})

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

// collabNeighbourLimit converts the engine's generic TopN into the
// neighbour limit the collab index expects.
//
// It is a separate function rather than an inline expression because TopN is
// the NEIGHBOURHOOD signal's bound — how many tag neighbours it votes from —
// and the two have different natural scales. TopN's default is 24, tuned for
// tag co-occurrence; collab's own default is 40, tuned for co-bookmark
// evidence, which is sparser per work than tag adjacency and benefits from a
// wider vote. Reusing TopN verbatim would silently import one signal's tuning
// into another's.
func collabNeighbourLimit(topN int) int {
	if topN <= 0 {
		return collab.DefaultNeighbours
	}
	// Scale rather than substitute: the ratio between the two defaults
	// (40/24) is preserved, so raising TopN raises both bounds together.
	n := topN * collab.DefaultNeighbours / 24
	if n < 8 {
		n = 8
	}
	return n
}

// defaultCollabScale is the fallback normaliser when a request's vote set has
// no observable maximum. It mirrors signal.defaultCollabScale and exists here
// so the engine can seed the value before the signal is constructed.
//
// A vote set is never actually empty when the signal is live — an empty one
// makes the signal skip and report itself degraded — so this is the value used
// when the index exists but produced no votes at all, which is the same
// observable situation by a different route.
const defaultCollabScale = 200.0

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
	// A nil Store panics below on the first query. It is reachable in
	// production as well as in tests: an engine built without a store (a
	// CLI `recommend` against a corpus with no index beside it, or an
	// embed-less lite run) would take down the whole request rather than
	// reporting that this signal has no data — which is the same
	// skip-don't-crash rule every other signal here follows.
	if e.Store == nil || e.Store.DB == nil {
		return nil, false, nil
	}
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
