// Package engine assembles a recommender from the store, the graph and the
// signal set. It is the one place that knows how a request becomes a
// ranked list, so the API layer stays a projection of it and the signals
// stay pure.
package engine

import (
	"context"
	"database/sql"
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

	// DocFreq is the loaded document-frequency table, or nil when this
	// mirror has not been ingested. It is a table rather than a query: see
	// docFrequencies.
	DocFreq *corpus.DocFreq
	Graph   *graph.CSR

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

	// tagIdx is the tag-embedding matrix, loaded lazily by tagIndex(). See
	// embedindex.go for why it is resident rather than read per request.
	tagIdx tagIndexState

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

	// OwnerTaste is the owner cohort's signed tag vector: the global prior
	// owner_taste scores with, for every reader rather than only for the
	// owner. Nil means this deployment named no owner, which is the
	// default and a supported state.
	//
	// Guarded by ownerMu for the same reason ArenaRatings is: the refresh
	// loop (KINDRED_OWNER_SESSION deployments rebuild the vector on a
	// ticker) replaces the map while requests are reading it, and an
	// unsynchronised map read racing that assignment is a fatal runtime
	// error rather than a race the detector merely flags. Use
	// SetOwnerTaste and OwnerTasteSignal.
	OwnerTaste map[int32]float64
	ownerMu    sync.RWMutex
}

// SetOwnerTaste replaces the owner cohort's taste vector atomically with
// respect to readers.
func (e *Engine) SetOwnerTaste(weights map[int32]float64) {
	e.ownerMu.Lock()
	defer e.ownerMu.Unlock()
	e.OwnerTaste = weights
}

// OwnerTasteSignal returns the owner cohort's vector for the signal to score
// with. Nil means there is no owner, and the signal reports itself degraded.
func (e *Engine) OwnerTasteSignal() map[int32]float64 {
	e.ownerMu.RLock()
	defer e.ownerMu.RUnlock()
	return e.OwnerTaste
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

	// Lambda is the diversifier's relevance/diversity trade-off for this
	// request, and it exists for ONE caller: the algorithm comparison.
	//
	// Zero means "the engine's default" (0.75), so every existing caller keeps
	// exactly the ranking the site serves. 1.0 is pure relevance, which is
	// what /algo and /api/v1/algo/compare ask for, because they compare WEIGHT
	// VECTORS.
	//
	// The reason is measured, not aesthetic: MMR scores a candidate as
	// lambda*score - (1-lambda)*similarity, so its novelty term is in the same
	// units as the score. A fixed 0.25 penalty therefore weighs differently
	// against a tune whose scores span 0.27-0.48 than against one whose scores
	// span 1.03-1.24. On the fixture, two very different tunes produced the
	// SAME five works in the SAME order -- the diversity pass dominated both,
	// and a page promising "only the weights change" was showing the
	// diversifier instead. Comparisons rank on relevance alone.
	Lambda   float64
	PoolSize int
	// Exclude drops the seeds themselves from the results: nobody wants to
	// be recommended the work they seeded from.
	Exclude bool

	// NoOwnerTaste opts the ranking OUT of the deployment's owner-taste
	// prior, and out of nothing else: it is the `?personal=0` escape hatch
	// for a caller who wants the mirror's own opinion of a work rather than
	// the owner cohort's opinion of it.
	//
	// It is an OPT-OUT rather than the plan's `Personal bool` because a
	// bool's zero value decides what every caller who forgets the field
	// gets, and a caller who forgets must get the ranking the site actually
	// serves. With `Personal bool` the zero value would silently switch a
	// dimension off for the CLI path, the e2e server, and every test that
	// builds a Request literally — the exact "signal absent without saying
	// so" failure this codebase has now found five times.
	NoOwnerTaste bool

	// SessionTaste is the reader's own learned tag preferences, as tag id
	// to weight. It is a REQUEST field for the same reason NoOwnerTaste
	// is: the engine ranks against a bare corpus with no store at all, and
	// a reader identity is not the engine's to resolve.
	//
	// The weights come from `arena_user_tag_weights`, which the arena
	// already writes and already reads for its own pair selection. This
	// is the wiring that makes the recommender honour them too.
	//
	// A nil map means "this reader has no learned taste", and the
	// session_taste signal SKIPS rather than scoring zero — a reader with
	// no history must get the same ranking as before, not a ranking
	// dragged down by a dimension that has nothing to say.
	SessionTaste map[int32]float64

	// SessionTasteEvidence is the number of comparisons behind each
	// learned weight, in the same key space as SessionTaste. The signal
	// reads it to damp a weight learned from one comparison against one
	// learned from forty.
	SessionTasteEvidence map[int32]float64

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
// The same argument applies one level out to the tag EMBEDDINGS: a work whose
// tags never co-occur with a seed's tags is invisible to the tag query even
// when it sits next to the seed in the 32-dimensional tag space. The embed
// source enumerates from that space instead of from the co-occurrence graph,
// which is the one thing tag pooling structurally cannot do.
//
// The modes are additive rather than exclusive: the union keeps the old
// behaviour intact when no index is present, so a mirror without bookmarks or
// without embeddings behaves identically to before rather than returning
// nothing.
//
// The values are FLAGS, not an enum: "tags+embed" and "tags+collab" are two
// independent dimensions, and the four combinations are not four behaviours
// that need four code paths.
type PoolMode int

const (
	// PoolTags is the historical behaviour: candidates share a tag with a
	// seed. The zero value, so a Request that never sets PoolMode behaves
	// as it always did.
	PoolTags PoolMode = 0
	// PoolTagsOrCollab adds works co-bookmarked with a seed.
	PoolTagsOrCollab PoolMode = 1 << 0
	// PoolTagsEmbed adds works whose tags are nearest the seeds in the
	// tag-embedding space.
	PoolTagsEmbed PoolMode = 1 << 1
	// PoolTagsCollabEmbed is both extra sources over the tag pool.
	PoolTagsCollabEmbed = PoolTagsOrCollab | PoolTagsEmbed
)

// hasCollab reports whether the mode admits co-bookmarked works.
func (p PoolMode) hasCollab() bool { return p&PoolTagsOrCollab != 0 }

// hasEmbed reports whether the mode enumerates from the tag-embedding space.
func (p PoolMode) hasEmbed() bool { return p&PoolTagsEmbed != 0 }

func (p PoolMode) String() string {
	switch {
	case p.hasCollab() && p.hasEmbed():
		return "tags+collab+embed"
	case p.hasCollab():
		return "tags+collab"
	case p.hasEmbed():
		return "tags+embed"
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

// DefaultPoolMode is what an absent pool_mode means.
//
// It was PoolTags until 2026-10-23, and it changed on a measurement rather
// than on an argument. Same 38 readers, same 495 held-out bookmarks, the same
// corpus, one flag apart:
//
//	                      pool recall@1000   recall@10   nDCG@10   coverage
//	tags+collab                  0.1895        0.1031    0.1947     0.6579
//	tags+collab+embed            0.2113        0.1050    0.1959     0.6579
//
// The rule was written down before the numbers existed: the default moves only
// if pool recall rises materially and recall@10 does not fall. Pool recall rose
// 11.5% relative, recall@10 rose with it, coverage was unchanged, and the
// request cost did not move (3.3-3.8 s on the live instance for every mode —
// the embedding source adds ~97 candidates and one join against a resident
// index).
//
// The gains are small in absolute terms. They are reported as measured, and
// the mode stays a parameter: a mirror with no embeddings and no bookmarks
// answers this default by falling back to tags alone and naming the missing
// dimensions in meta.degraded.
const DefaultPoolMode = PoolTagsCollabEmbed

// ParsePoolMode validates a pool mode name.
//
// The empty string is DefaultPoolMode, NOT PoolTags: "no opinion" and "tags
// only" are different requests, and conflating them meant the measured default
// could never be anything but the zero value.
func ParsePoolMode(s string) (PoolMode, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "":
		return DefaultPoolMode, nil
	case "tags", "tag":
		return PoolTags, nil
	case "tags+collab", "collab", "tags_or_collab":
		return PoolTagsOrCollab, nil
	case "tags+embed", "embed", "tags_or_embed":
		return PoolTagsEmbed, nil
	case "tags+collab+embed", "collab+embed", "all":
		return PoolTagsCollabEmbed, nil
	}
	// The message names the PARAMETER, not just the concept. "unknown pool
	// mode" is not actionable from a page that has several controls: the
	// reader cannot tell whether the problem is pool_mode, pool or the tune
	// select. The filter errors name their parameters and this now matches,
	// which the browser suite checks by asserting the parameter name appears
	// in the error body.
	return PoolTags, fmt.Errorf(
		"pool_mode %q is not a mode; use pool_mode=tags, tags+collab, tags+embed "+
			"or tags+collab+embed", s)
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
			// owner_taste is the owner cohort's taste as a global prior.
			// It is deliberately the smallest weight here: it is one
			// person's preferences applied to every other reader, so it
			// must nudge rather than override the signals that describe
			// the work and the reading evidence around it.
			//
			// A deployment with no owner configured pays nothing for this
			// entry: the signal skips for every candidate, and a skipped
			// signal contributes no term, so the surviving weights keep
			// their ratios exactly and the ranking is identical to one
			// compiled before this signal existed. That is why the weight
			// does not need the `renormalise` treatment embedding gets.
			"owner_taste": 0.06,
			// session_taste is the reader's OWN learned preferences,
			// read from the arena's arena_user_tag_weights. It is
			// evidence of what this reader chose, which is the same
			// kind of evidence peer_rating carries, and it earns a
			// weight just above peer_rating because it is about THIS
			// reader rather than about a work's crowd.
			//
			// A reader with no learned weights pays nothing for this
			// entry: the signal skips for every candidate, and a
			// skipped signal contributes no term, so the surviving
			// weights keep their ratios and the ranking is identical to
			// one compiled before this signal existed.
			"session_taste": 0.14,
			// idf_overlap is tag_overlap with the weight of each shared
			// tag scaled by its rarity. It answers a different question
			// from tag_overlap — "do these agree on the parts that
			// discriminate" rather than "how much do these overlap" —
			// so it gets its own weight rather than replacing one.
			//
			// It is below tag_overlap because it is the more brittle of
			// the two: with a sparse corpus it can find no rare shared
			// tag at all and skip, where tag_overlap always has an
			// opinion. A tune can raise it on a large mirror.
			"idf_overlap": 0.12,
			// seed_agreement asks a different question from idf_overlap:
			// not "match one seed well" but "speak to most of them".
			// With one seed it is a restatement of idf_overlap and skips,
			// so a one-seed request pays nothing for it.
			//
			// It starts level with idf_overlap and is measured, not
			// assumed: `kindred eval` records recall and the per-fandom
			// slices, and the tune exists to be changed on that evidence.
			"seed_agreement": 0.10,
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
	poolIDs, collabVotes, poolSources, err := e.poolFor(ctx, seeds, pool, req.Exclude, seedIDs, req)
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
				Seeds:       len(seeds),
				Tune:        tuneName,
				Shortfall:   &rank.Short{Requested: n, Returned: 0, Reason: "no candidates share any seed tag"},
				PoolSources: poolSources,
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

	// The document frequencies the IDF-weighted overlap signal needs.
	//
	// They are computed ONCE, here, over the pool's own tag ids — never
	// per candidate. The corpus's work_tags table holds tens of millions
	// of rows, and a full-table GROUP BY per request would put every
	// ranking behind a full scan.
	//
	// A failure to read them is NOT fatal: the signal then has no
	// frequencies, every tag looks maximally rare, and the signal skips
	// for candidates whose shared tags carry no mass. The ranking
	// degrades to the other signals rather than failing.
	docFreq, corpusTotal := e.docFrequencies()

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
	sigs, tune := e.signals(ctx, seeds, collabVotes, collabScale, tuneWeights, tuneName, !req.NoOwnerTaste,
		signalOptions{
			sessionTaste:         req.SessionTaste,
			sessionTasteEvidence: req.SessionTasteEvidence,
			docFreq:              docFreq,
			corpusTotal:          corpusTotal,
		})
	ranked, meta, err := rank.Score(ctx, cands, seeds, sigs, tune, e)
	if err != nil {
		return nil, err
	}
	meta.Seeds = len(seeds)
	meta.PoolSources = poolSources
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
	// 0.75 is the site's trade-off. A caller can raise it to 1.0 to rank on
	// relevance alone; see Request.Lambda for why the comparison surfaces do.
	lambda := req.Lambda
	if lambda <= 0 || lambda > 1 {
		lambda = 0.75
	}
	div := diversify.Apply(ranked, diversify.Options{
		Requested:   n,
		Lambda:      lambda,
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
func (e *Engine) poolFor(ctx context.Context, seeds []rank.Candidate, limit int, exclude bool, seedIDs []int64, req Request) ([]int64, map[int64]float64, *rank.PoolSources, error) {
	tagIDs := map[int32]bool{}
	for _, s := range seeds {
		for _, id := range s.TagIDs {
			tagIDs[id] = true
		}
	}
	f := req.Filter
	src := &rank.PoolSources{}

	// Collab neighbours are resolved FIRST, before the early return below, so
	// a seed with no tags can still be recommended from: a work sharing no
	// tag with the seed is precisely the case collaborative filtering is
	// for, and an early return on len(tagIDs)==0 would make it unreachable
	// for every tagless seed.
	var collabVotes map[int64]float64
	if req.PoolMode.hasCollab() && e.Collab != nil {
		collabVotes = signal.CollabVotes(e.Collab, seedIDs, collabNeighbourLimit(e.TopN))
	}

	// The embedding source's inputs. Resolved here for the same reason the
	// collab votes are: a seed whose tags never co-occur with anything is
	// exactly the seed the tag query cannot enumerate from, and an early
	// return would make the embed source unreachable for it too.
	//
	// The seeds' OWN tags are excluded from the nearest-tag list. They are
	// what the tag half is already built from, so spending an embed slot to
	// re-enumerate them would spend the one new source on the old answer.
	var (
		embedIdx  *tagIndex
		embedNear []tagSim
	)
	if req.PoolMode.hasEmbed() {
		idx, ierr := e.tagVectors(ctx)
		if ierr != nil {
			// A partial index still enumerates from the rows it has, which
			// is strictly better than dropping the source: the request is
			// answered and the log says the dimension is incomplete.
			slog.Warn("embedding pool source is running on a partial tag index", "err", ierr)
		}
		if idx != nil {
			if centroid := seedCentroid(idx, seeds); centroid != nil {
				skip := make(map[int32]bool, len(tagIDs))
				for id := range tagIDs {
					skip[id] = true
				}
				embedNear = idx.nearestTags(centroid, EmbedPoolTags, embedMinSim, skip)
			}
			embedIdx = idx
		}
	}

	// The pool, built by admitting one source at a time.
	//
	// De-duplication lives HERE rather than in each source, because the
	// sources overlap by construction (a co-bookmarked work usually shares a
	// tag, and that is the healthy case). The first version appended the
	// collab ids to the tag pool without checking, so a work both sources
	// found was ranked twice and the pool reported a size it did not have.
	out := make([]int64, 0, limit)
	inPool := make(map[int64]bool, limit)
	admit := func(ids []int64, bucket *int) {
		for _, id := range ids {
			// The limit is enforced HERE, not by the callers. The tail is a
			// slice of whatever the tag query returned and the extras are
			// already trimmed to the room left, so a caller-side check would
			// be right in two places and wrong in the third: measured on the
			// live mirror before this guard, a 1000-item request returned 1061
			// candidates, because the tail was admitted in full after the
			// extras had taken 98 slots.
			if len(out) >= limit {
				return
			}
			if inPool[id] {
				src.Overlap++
				continue
			}
			inPool[id] = true
			out = append(out, id)
			*bucket++
		}
	}

	// Sorted tag ids rather than map order: the argument list is part of the
	// cache key and the pool must not depend on Go's randomised map iteration.
	tagList := make([]int32, 0, len(tagIDs))
	for id := range tagIDs {
		tagList = append(tagList, id)
	}
	sort.Slice(tagList, func(a, b int) bool { return tagList[a] < tagList[b] })

	// Declared here, assigned below: the top-up after the extra sources needs
	// the same query, and a closure declared inside the `if` cannot be reached
	// from there.
	var (
		runTagPool func(budget int) ([]int64, error)
		tagTail    []int64
	)

	if len(tagIDs) > 0 {
		where := `wt.tag_id IN (` + corpus.Placeholders(len(tagList)) + `)`
		// The filter predicates are appended BEFORE the limit argument,
		// because the limit's placeholder is the last one in the statement.
		// Binding them out of order is not a compile error and not a runtime
		// error in SQLite for a well-typed query — it is a silently wrong
		// result, which is why this ordering is written as a single explicit
		// sequence rather than interleaved with the SQL string.
		//
		// The tag half of the pool is capped at the budget MINUS the room
		// reserved for the other sources.
		//
		// This is the bug that made PoolTagsOrCollab a no-op, and it is worth
		// naming precisely because the code LOOKED correct. The SQL took
		// `LIMIT limit`, so the tag pool always filled every slot, and the
		// union below computed `room := limit - len(out)` — which is then
		// always zero, so collab contributed nothing and returned. Measured
		// on the live mirror: `pool_mode=tags` and `pool_mode=tags+collab`
		// produced BYTE-IDENTICAL 8-item lists, and none of the seed's four
		// known co-bookmarked neighbours appeared in either.
		//
		// Reserving a fixed share is a deliberate trade, not a compromise to
		// paper over that. Tag pooling is the stronger half on this mirror —
		// measured neighbourhoods are 1-4 works per seed against thousands of
		// tag matches — so reserving a large share would hand most of the
		// budget to a much thinner source. The share is therefore small and
		// explicit, and the extra sources go first when the budget is tight,
		// because those works are unreachable any other way.
		tagBudget := limit
		if req.PoolMode.hasCollab() {
			tagBudget -= collabReserve(limit)
		}
		if req.PoolMode.hasEmbed() {
			tagBudget -= embedReserve(limit)
		}
		if tagBudget < 1 {
			// A pool too small to split. Tags win the slot, because on this
			// corpus they are the half that can actually rank.
			tagBudget = 1
		}
		// runTagPool fetches the tag half ONCE, for the whole budget, and the
		// caller admits it in two parts: the reserved part before the extra
		// sources and the tail after them.
		//
		// The tail exists because a reserve that is not filled shortens the
		// pool. Measured on the live mirror with the reserve alone (no tail):
		// pool_mode=all returned 848 candidates against 1000 for tags-only —
		// the tag half was capped at 750, collab found 1 and embedding 97, and
		// 152 slots went unused. The recall figure went UP, so nothing
		// measured would ever have reported those 152 slots, and they are 152
		// chances to lose the one work a reader wanted.
		//
		// Fetching them in the SAME query matters: the first version ran a
		// second query for the tail with the admitted ids excluded, which cost
		// as much as the first (~2.2 s each on a 55k-work seed, 4.6 s total
		// against 2.45 s for tags-only) and produced the identical pool.
		runTagPool = func(budget int) ([]int64, error) {
			args := make([]any, 0, len(tagList)+16)
			for _, id := range tagList {
				args = append(args, id)
			}
			q := `SELECT wt.work_id, COUNT(*) AS shared FROM work_tags wt
			      JOIN works w ON w.id = wt.work_id
			      WHERE ` + where
			q += filterSQL(f, &args)
			// Blocks are applied HERE, inside the same pool query, for the same
			// reason the filters are. Filtering the finished ranking would
			// return a subset of a ranking rather than a ranking over the works
			// the reader is willing to see: ask for 20 and get 8, with no way
			// to tell that 60 eligible works sat below the cut-off behind
			// blocked ones.
			q += blockSQL(req.BlockedTagIDs, &args)
			q += seenSQL(req.SeenIDs, &args)
			// The seeds themselves are excluded in SQL, not after the LIMIT.
			// Dropping them from the scanned rows shortened the pool by one
			// every time a seed landed in its own tag results, which on a
			// 20-item pool is 5% and on the fixture's 10 is 10% -- visible
			// as "the pool is always one short", never as an error.
			if exclude && len(seedIDs) > 0 {
				ph := corpus.Placeholders(len(seedIDs))
				q += ` AND wt.work_id NOT IN (` + ph + `)`
				for _, id := range seedIDs {
					args = append(args, id)
				}
			}
			// ORDER BY shared DESC, work_id ASC. The work_id tiebreak is what
			// makes two identical requests rank identical candidates.
			q += ` GROUP BY wt.work_id
			       ORDER BY shared DESC, wt.work_id ASC
			       LIMIT ?`
			args = append(args, budget)

			rows, err := e.Corpus.DB.QueryContext(ctx, q, args...)
			if err != nil {
				return nil, fmt.Errorf("pool query: %w", err)
			}
			defer rows.Close()
			out := make([]int64, 0, budget)
			for rows.Next() {
				var id int64
				var shared int
				if err := rows.Scan(&id, &shared); err != nil {
					return nil, err
				}
				out = append(out, id)
			}
			return out, rows.Err()
		}

		// ONE query for the whole budget, not one for the reserve plus one to
		// top up. Measured on the live mirror: the top-up's second query cost
		// the same as the first (each ~2.2 s on a 55k-work seed), so the
		// two-query version answered in 4.6 s against 2.45 s for tags-only,
		// for a pool that was identical. The ordering is the same either way,
		// so the rows past the reserve are exactly what the top-up would have
		// fetched.
		tagRows, err := runTagPool(limit)
		if err != nil {
			return nil, nil, nil, err
		}
		reserved := tagBudget
		if reserved > len(tagRows) {
			reserved = len(tagRows)
		}
		admit(tagRows[:reserved], &src.Tags)
		// The tail is admitted AFTER the extra sources, so their slots are
		// never taken by tags: the reserve guarantees the works only they can
		// reach, and the tail only fills what they leave.
		tagTail = tagRows[reserved:]
	}

	// UNION, not replacement: replacing the tag pool with the extra sources
	// would make the ranking depend entirely on whether the mirror happened
	// to sample bookmark rows or hold embeddings for this seed's
	// neighbourhood — the recommender would silently stop being
	// content-based, which is the additive behaviour that makes PoolMode
	// safe to expose as a request parameter.
	if len(collabVotes) > 0 {
		if room := limit - len(out); room > 0 {
			admit(collabCandidates(collabVotes, room, seedIDs, 0, f, req.BlockedTagIDs, req.SeenIDs, e.Corpus), &src.Collab)
		}
	}
	if embedIdx != nil && len(embedNear) > 0 {
		if room := limit - len(out); room > 0 {
			extra, err := embedCandidates(ctx, e.Corpus.DB, embedNear, room, seedIDs, f, req.BlockedTagIDs, req.SeenIDs)
			if err != nil {
				// A failed extra source is reported and dropped, not fatal:
				// the pool is a union, and a union that loses one arm still
				// ranks. Returning an error here would turn a missing
				// dimension into a failed request.
				slog.Warn("embedding pool source failed for this request", "err", err)
			} else {
				admit(extra, &src.Embedding)
			}
		}
	}
	// Whatever the extra sources left unused, from the tag rows already
	// fetched. No second query and no exclusion list: these are the rows the
	// single query returned past the reserve, in the same order it returned
	// them.
	if len(out) < limit && len(tagTail) > 0 {
		admit(tagTail, &src.Tags)
	}
	return out, collabVotes, src, nil
}

// EmbedPoolTags is how many tag neighbours the embedding source enumerates
// from.
//
// 32 is the width of the embedding space and the number of tags one seed's
// nearest neighbourhood plausibly covers; measured on the live mirror a
// seed's top-32 tags by cosine reach thousands of works between them, which
// is far more than the source's reserved share of the pool. Raising it
// raises the SQL join's fan-in linearly without adding candidates the pool
// has room for.
const EmbedPoolTags = 32

// embedMinSim is the cosine floor for a tag to count as "near the seeds".
//
// Zero would admit orthogonal tags, which by construction carry no shared
// meaning; 0.25 keeps tags that are at least somewhat aligned, and the
// source's limit is what really bounds the answer.
const embedMinSim = 0.25

// embedReserve is how many of a pool's slots are held back for the embedding
// source when PoolMode includes it. Same shape and same reason as
// collabReserve: a small explicit share, because tag pooling is the stronger
// half of this mirror's evidence and an extra source that filled half the
// pool would outvote it.
func embedReserve(limit int) int {
	if limit <= 2 {
		return 0
	}
	r := (limit + 7) / 8
	if r > limit-1 {
		return limit - 1
	}
	return r
}

// embedCandidates picks the works carrying the tags nearest the seeds in the
// embedding space, ordered by how close those tags are.
//
// This is the one enumeration this recommender cannot do from tags: a work
// whose tags never co-occur with a seed's tags is invisible to the tag query
// above, and the factorisation is what says those tags belong together. It
// is a SECOND-ORDER use of the same co-occurrence matrix rather than a new
// data source, and saying so matters — it cannot find works whose tags are
// absent from the matrix altogether, which is the same blind spot the tag
// query has.
//
// Filters, blocks and seen ids are applied in SQL for the reason poolFor
// documents at length: a source that post-filters returns fewer than asked
// even when qualifying works exist further down its own ranking.
func embedCandidates(ctx context.Context, db *sql.DB, near []tagSim, limit int, seedIDs []int64, f Filter, blocked map[int64]bool, seen map[string]bool) ([]int64, error) {
	if db == nil || len(near) == 0 || limit <= 0 {
		return nil, nil
	}
	parts := make([]string, 0, len(near))
	args := make([]any, 0, len(near)*2+16)
	for i, t := range near {
		if i == 0 {
			parts = append(parts, `SELECT ? AS tag_id, ? AS w`)
		} else {
			parts = append(parts, `UNION ALL SELECT ?, ?`)
		}
		args = append(args, t.ID, t.Sim)
	}
	q := `SELECT wt.work_id FROM (` + strings.Join(parts, " ") + `) s
	      JOIN work_tags wt ON wt.tag_id = s.tag_id
	      JOIN works w ON w.id = wt.work_id
	      WHERE 1=1`
	q += filterSQL(f, &args)
	q += blockSQL(blocked, &args)
	q += seenSQL(seen, &args)
	// Seeds are dropped unconditionally, exactly as collabCandidates does:
	// recommending the work a reader seeded from is never useful, and a flag
	// here would only ever be set to false to make a ranking look fuller.
	if len(seedIDs) > 0 {
		holders := corpus.Placeholders(len(seedIDs))
		q += ` AND wt.work_id NOT IN (` + holders + `)`
		for _, id := range seedIDs {
			args = append(args, id)
		}
	}
	q += ` GROUP BY wt.work_id
	       ORDER BY SUM(s.w) DESC, wt.work_id ASC
	       LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("embed pool query: %w", err)
	}
	defer rows.Close()
	out := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
// signalOptions carries the optional signals' inputs into signals().
//
// It is a struct passed by variadic pointer so that signals() keeps its
// existing call signature: the tests that assert WHICH signals are registered
// (internal/engine/owner_taste_test.go) call it positionally, and adding a
// parameter there would have meant rewriting the assertions that are the
// point of those tests.
type signalOptions struct {
	sessionTaste         map[int32]float64
	sessionTasteEvidence map[int32]float64
	docFreq              map[int32]float64
	corpusTotal          float64
}

func (e *Engine) signals(ctx context.Context, seeds []rank.Candidate, collabVotes map[int64]float64, collabScale float64, overrides map[string]float64, tuneName string, personal bool, opts ...signalOptions) ([]rank.Signal, rank.Tune) {
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

	// owner_taste is the owner cohort's taste as a GLOBAL prior — the one
	// signal here that is registered for every reader rather than for the
	// reader who asked. It is the mechanism behind this deployment's whole
	// request ("I want my taste to influence recommendations for all
	// users"), and it nudges rather than overrides: its weight is small and
	// explicit in the tune, so a reader's own signals still dominate.
	//
	// Registered UNCONDITIONALLY, for the same reason peer_rating is: with
	// no owner configured it skips for every candidate and names itself in
	// meta.degraded[], which is the difference between "this deployment has
	// no owner" and "the owner signal is broken".
	//
	// Graph is only handed over when it is non-nil: assigning a typed nil
	// *graph.CSR into the interface field would make it non-nil as an
	// interface and the first Name(id) call would dereference it.
	var ownerGraph signal.Graph
	if e.Graph != nil {
		ownerGraph = e.Graph
	}
	ownerWeights := e.OwnerTasteSignal()
	if !personal {
		// ?personal=0 asks for the ranking with NO personal prior applied.
		// The vector is withheld rather than the signal dropped: the signal
		// still registers, still skips, and still says so in
		// meta.degraded[]. Dropping it would make a deliberate request and a
		// broken installation indistinguishable in the response.
		ownerWeights = nil
	}
	sigs = append(sigs, signal.OwnerTaste{Weights: ownerWeights, Graph: ownerGraph})

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

	// session_taste is the reader's OWN learned preferences, read from the
	// arena's `arena_user_tag_weights` and passed in on the request.
	//
	// It is registered UNCONDITIONALLY, for the same reason collab and
	// peer_rating are: a signal that is only present when it has data is a
	// signal that is absent without saying so. With no learned weights it
	// skips for every candidate and names itself in meta.degraded[], so
	// "this reader has no history yet" is distinguishable from "the session
	// taste signal is broken".
	//
	// The option is a variadic rather than a positional parameter so the
	// existing call sites -- including the tests that assert which signals
	// are registered -- keep compiling unchanged.
	//
	// The signal is registered UNCONDITIONALLY, not only when the reader
	// has learned weights. Registering it conditionally is precisely the
	// "absent without saying so" failure: a reader with no history would
	// get a response with no session_taste in meta.degraded[] and no way
	// to tell that from the signal being broken. Registered always, the
	// signal SKIPS for every candidate and names itself.
	//
	// An empty taste map is therefore passed through as an empty map, not
	// as nil, so the signal exists and has an opinion (it has none) rather
	// than not existing at all.
	taste := map[int32]float64{}
	tasteEvidence := map[int32]float64{}
	for _, o := range opts {
		if o.sessionTaste != nil {
			taste = o.sessionTaste
			tasteEvidence = o.sessionTasteEvidence
			break
		}
	}
	sigs = append(sigs, signal.SessionTasteFrom(taste, tasteEvidence))

	// idf_overlap weights tag agreement by how RARE each shared tag is.
	// It is registered unconditionally for the same reason as the rest: a
	// signal that is only present when it has data is a signal that is
	// absent without saying so.
	//
	// The document frequencies come from the corpus, computed once per
	// request over the pool's own tags — not a scan of the whole
	// work_tags table, which holds tens of millions of rows.
	docFreq := map[int32]float64{}
	corpusTotal := 0.0
	for _, o := range opts {
		if o.docFreq != nil {
			docFreq = o.docFreq
			corpusTotal = o.corpusTotal
			break
		}
	}
	sigs = append(sigs, signal.IDFWeightedOverlap{
		DocFreq: docFreq,
		Total:   corpusTotal,
	})

	// seed_agreement is the same rarity weighting, averaged over the seeds
	// instead of maximised over them. Every other tag signal here answers
	// "how well does this work match your best seed"; with twenty bookmarks
	// that question lets a work which hits one of the reader's fandoms rank
	// beside a work which hits most of them.
	//
	// Registered unconditionally like the rest: with fewer than two usable
	// seeds it skips for every candidate and names itself in
	// meta.degraded[], which is the difference between "you gave me one
	// seed, so agreement is not a question" and "the signal is broken".
	sigs = append(sigs, signal.SeedAgreement{
		MaxTags: 200,
		DocFreq: docFreq,
		Total:   corpusTotal,
	})

	// embedding places a work in the tag-embedding space and scores its
	// cosine to the seeds' position there.
	//
	// The weight handling is the whole reason this block was rewritten. The
	// first version called renormalise INSIDE `if !e.Lite` — i.e. exactly
	// when the signal is registered — which deleted its 0.10 and rescaled
	// the others. The signal was therefore registered, weighted at zero, and
	// invisible: it appeared in neither evidence nor meta.degraded, so the
	// dimension looked present in every configuration and measured nothing.
	// Observed live on 2026-10-23 with `?tune=quality` (weight 0.0654
	// stored): seven signals per item, embedding among neither them nor the
	// degraded list.
	//
	// Now the weight is redistributed only when the dimension really is
	// absent: lite mode (no signal registered), or a mirror whose `kindred
	// embed` never ran (signal registered, skips everywhere, names itself).
	// A deployment WITH vectors keeps its weight and pays for it.
	if e.Lite {
		tune.Weights = renormalise(tune.Weights, "embedding")
	} else {
		idx, ierr := e.tagVectors(ctx)
		if ierr != nil {
			// A partially loaded index is usable for the rows it has and
			// says so in the log. The signal still skips for candidates
			// whose tags are missing, and meta.degraded names it if that is
			// all of them.
			slog.Warn("tag embeddings loaded with errors", "err", ierr)
		}
		// Registered even when there is nothing to read, which is the house
		// rule for every other signal here: no vectors means it SKIPS for
		// every candidate and names itself in meta.degraded[], so "this
		// mirror was never embedded" is distinguishable from "the dimension
		// does not exist". The weight stays in the tune — a skipped signal
		// contributes no term, so the ranking is identical to one built
		// before the signal existed, which is exactly what the deleted
		// weight was trying to achieve without saying anything.
		//
		// idx.centroid is a method value on a possibly-nil receiver: it
		// checks for nil, so a missing index degrades to "no candidate can
		// be placed" rather than a panic on the first candidate.
		sigs = append(sigs, signal.Embedding{
			SeedVec:  seedCentroid(idx, seeds),
			Centroid: idx.centroid,
		})
	}
	return sigs, tune
}

// docFrequencies returns the corpus's document-frequency table.
//
// It is LOADED, never computed. The first version queried work_tags per
// request and cost 12.5 s on the real mirror — every recommendation, not
// only /algo, which is a regression no unit test could see because the
// fixtures are small. The table is built once by `kindred ingest` and read
// from a side file beside the mirror.
//
// A nil index means this mirror has never been ingested. The IDF signal then
// skips for every candidate, which rank.Meta reports in `degraded` by name:
// the response says the signal did not run rather than ranking as if rarity
// meant nothing.
func (e *Engine) docFrequencies() (map[int32]float64, float64) {
	if e == nil || e.DocFreq == nil {
		return map[int32]float64{}, 0
	}
	return e.DocFreq.ByTag, e.DocFreq.Total
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
