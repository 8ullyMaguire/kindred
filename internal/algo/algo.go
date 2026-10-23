// Package algo is the registry of recommendation algorithms this service can
// actually run, per entity kind.
//
// # Why a registry and not a list of tunes
//
// The site already compared TUNES (weight sets) for works, in `algo_comparisons`.
// A tune is one algorithm's knobs. The question this package answers is the
// bigger one: for a seed of kind K, WHICH MECHANISM produces the list — and may
// the reader pick it.
//
// # Why the registry carries the disabled ones
//
// The obvious ranker for this corpus is a graph neural network, and this service
// cannot run one: there is no training pipeline, no GPU, and no per-request path
// that would make a forward pass affordable on a 4-core host. A page that simply
// omits those algorithms tells the reader nothing; a page that lists one and
// answers "not available" is a page making a claim about itself. So every
// disabled algorithm carries the REASON it is disabled and, where one exists,
// the MEASUREMENT behind it. `Disabled` rows are served by the same endpoint as
// the live ones and are never selectable.
//
// # Resource-intensive means disabled, not "slow but try it"
//
// The rule applied here: an algorithm ships only if one request can produce a
// list from the data on disk, with a bounded cost, on this host. Training-based
// methods (LightGCN, ALS, implicit MF, word2vec-over-bookmarks, factorization
// machines, tag-aware MF) all fail that rule — they need an offline job whose
// output would then have to be versioned and drifted against, which is a
// project, not a page.
package algo

import (
	"fmt"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// Status is whether an algorithm can serve a request on this deployment.
type Status string

const (
	// Live means the mechanism runs per request from the data on disk.
	Live Status = "live"
	// Disabled means the algorithm is known, wanted, and deliberately not
	// runnable here. Every disabled row names its reason.
	Disabled Status = "disabled"
)

// ID is a stable identifier. It appears in URLs, in `reader_algos` rows, and in
// `algo_impressions`, so renaming one silently splits its record in two.
type ID string

const (
	// Hybrid is the shipped blend: the candidate pool (tags, co-bookmarks,
	// embedding) scored by the tuned signal weights. It is what every kind
	// falls back to, because it is the only mechanism that already existed.
	Hybrid ID = "hybrid"
	// Collab is item-item collaborative filtering on bookmark vectors, alone:
	// cosine of bookmarker sets, shrunk toward zero by evidence count.
	Collab ID = "collab"
	// TagIDF is IDF-weighted tag overlap: rare shared tags count more than
	// tags a third of the corpus carries. Live for authors; a tune, not an
	// algorithm, for works.
	TagIDF ID = "tagidf"
	// BFS2 is two-hop neighbourhood votes through co-bookmarkers: the literal
	// BFS, with no per-user weight and no distance dampening. Part of the hybrid
	// blend for works; a standalone algorithm only where there is no blend.
	BFS2 ID = "bfs2"
	// Adamic is Adamic-Adar over co-bookmarking users: each shared reader
	// contributes 1/log(their bookmark count), which is the principled
	// correction to BFS2's flat votes.
	Adamic ID = "adamic"
	// Popular is the baseline: the most-read works carrying the seed's tags.
	// It is here to be beaten, and a standings table without it cannot say
	// whether any personalisation is working.
	Popular ID = "popular"
	// Embed is nearest-neighbour in the tag-embedding space. For a tag seed it
	// is the "tags that sit near it" list; for a work seed it is the work's tag
	// centroid.
	Embed ID = "embed"
	// Cooccur is tag co-occurrence: how often two tags are filed on the same
	// work, normalised. It is the tag-space twin of Collab.
	Cooccur ID = "cooccur"
	// CoBookAuthors ranks authors by shared readers with the seed author.
	CoBookAuthors ID = "cobook"

	// --- known, wanted, not runnable here ---------------------------------

	// PageRank is personalized PageRank / random walk with restart over the
	// reader-work graph.
	PageRank ID = "ppr"
	// LightGCN is graph convolution over the bipartite graph.
	LightGCN ID = "lightgcn"
	// ALS is implicit-feedback matrix factorisation.
	ALS ID = "als"
	// TagMF is a tag-regularised factorisation (collaborative topic regression
	// shaped: item factors pulled toward a tag-derived content vector).
	TagMF ID = "tagmf"
	// Work2Vec is skip-gram over readers' bookmark lists.
	Work2Vec ID = "work2vec"
	// FM is factorization machines over tags plus numeric metadata.
	FM ID = "fm"
	// UserKNN is user-user k-nearest-neighbour CF.
	UserKNN ID = "userknn"
	// LiftRules is association-rule mining (support/lift over bookmark sets).
	LiftRules ID = "liftrules"
)

// Algo is one entry of the registry.
type Algo struct {
	ID     ID     `json:"id"`
	Label  string `json:"label"`
	Blurb  string `json:"blurb"`
	Status Status `json:"status"`
	// Reason is required for a Disabled row and must be empty otherwise: a live
	// algorithm with an excuse is a contradiction, and one without a reason is
	// the silent omission this registry exists to prevent.
	Reason string `json:"reason,omitempty"`
	// Cost is what one request costs, in the only unit that matters here:
	// what it has to touch. Empty for disabled rows.
	Cost string `json:"cost,omitempty"`
	// Kind is the kind this entry describes. The same ID can appear for several
	// kinds with different status: `embed` is live for tags and works, and
	// meaningless for authors.
	Kind arena.EntityKind `json:"kind"`
	// Default marks the measured best algorithm for this kind. Exactly one per
	// kind, enforced by TestEveryKindHasExactlyOneDefault.
	Default bool `json:"default"`
}

// Algorithm families, in the order the page shows them: the live ones first,
// then the ones this host cannot run, each with its reason.
//
// The disabled set is not "ideas we do not like". Every one of them is a better
// model than what runs here on a corpus with dense interactions; they are
// disabled because this corpus is sparse, this host is small, and none of them
// has a per-request path.
var disabled = []Algo{
	{
		ID:     TagIDF,
		Label:  "Tag overlap alone (IDF-weighted)",
		Blurb:  "Rare shared tags only, with the popularity and co-bookmark dimensions removed.",
		Status: Disabled,
		Reason: "for WORKS this is a TUNE, not an algorithm: one signal at weight 1.0 is exactly what /algo compares, and offering it twice in two places would let the same question be answered by two tables that can disagree. It is live for AUTHORS, where no blend exists. Compare tunes on /algo.",
	},
	{
		ID:     BFS2,
		Label:  "Two-hop co-bookmark votes alone",
		Blurb:  "The literal BFS through co-bookmarkers, with no other signal mixed in.",
		Status: Disabled,
		Reason: "for WORKS this is the `neighbourhood` signal at weight 1.0, i.e. a tune: see /algo, which is where weight vectors are compared. It runs as part of the hybrid blend today. Offered as a standalone algorithm only for kinds that have no blend to put it in — which is none of them.",
	},
	{
		ID:     PageRank,
		Label:  "Personalized PageRank / random walk with restart",
		Blurb:  "Seed the walk at the reader's works, let probability flow through co-bookmarkers, rank by stationary mass. The formal version of the two-hop BFS: the restart parameter decays distant hops instead of cutting them off at a fixed depth.",
		Status: Disabled,
		Reason: "needs the reader→work graph resident (2.1M rows, ~40 MB as a sparse matrix) and a per-request power iteration; measured on this host the iteration alone blows the request budget, and building the matrix per request is worse. The two-hop BFS is the cheap approximation that ships.",
	},
	{
		ID:     LightGCN,
		Label:  "LightGCN (graph convolutional CF)",
		Blurb:  "Learned neighbourhood aggregation over the bipartite reader–work graph. State of the art for implicit feedback when there is enough data and a training loop.",
		Status: Disabled,
		Reason: "requires a training pipeline and a GPU; nothing in this deployment trains a model, and a 4-core host with an 81 MiB tag index already resident has no room for one.",
	},
	{
		ID:     ALS,
		Label:  "Implicit ALS (weighted matrix factorisation)",
		Blurb:  "Factor the reader×work matrix, treating observed bookmarks as confidence-weighted positives and unobserved entries as weak negatives. The standard strong baseline for implicit feedback.",
		Status: Disabled,
		Reason: "needs an offline factorisation job plus a versioned factor table; a per-request solve is not feasible and a stale factor table is a drift surface this repo has no instrument for yet.",
	},
	{
		ID:     TagMF,
		Label:  "Tag-regularised factorisation (CTR-shaped)",
		Blurb:  "Item factors pulled toward a tag-derived content vector, so a five-bookmark fic with the right tags can be placed correctly without a co-bookmarker.",
		Status: Disabled,
		Reason: "same training requirement as ALS, and it needs the per-work tag matrix in a form that can be regularised; the tag space here is 634k-dimensional and 32-wide, which is the embedding we already ship, applied as a similarity rather than as a prior.",
	},
	{
		ID:     Work2Vec,
		Label:  "Work2Vec (skip-gram over bookmark lists)",
		Blurb:  "Treat each reader's bookmark list as a sentence and learn work embeddings from co-occurrence.",
		Status: Disabled,
		Reason: "needs a training pass and assumes an order that bookmark sets do not have; the co-bookmark index already captures the same signal with no training.",
	},
	{
		ID:     FM,
		Label:  "Factorization machines over tags and metadata",
		Blurb:  "Model arbitrary feature interactions (tag, rating, completion, word count) rather than only pairwise co-occurrence.",
		Status: Disabled,
		Reason: "training pipeline, plus feature engineering that this corpus cannot support honestly: `work_tags.tag_type` is a dead column (3,890,504 of 3,891,300 rows say `freeforms`), so a feature built on tag type would encode nothing.",
	},
	{
		ID:     UserKNN,
		Label:  "User-based k-nearest-neighbour CF",
		Blurb:  "Find the readers with the largest bookmark overlap, recommend what they read and you have not.",
		Status: Disabled,
		Reason: "the live table has 6,940 readers and 1,052,189 read rows: a user×user pass is not per-request work, and in sparse data two readers sharing five of five hundred bookmarks is weaker evidence than two works sharing fifty of two hundred bookmarkers — which is the direction that ships.",
	},
	{
		ID:     LiftRules,
		Label:  "Association rules (support / lift)",
		Blurb:  "Mine {A,B} → {C} rules from bookmark sets and rank candidates by lift.",
		Status: Disabled,
		Reason: "a minimum-support threshold is what kills the long tail this corpus is mostly made of; the co-bookmark index ranks the same pairs with a confidence shrink instead of a support cliff.",
	},
}

// live describes the algorithms that run per request, per kind.
//
// The same ID is described once per kind it serves, because its STATUS is a
// fact about the kind: `embed` reads the tag-embedding matrix, which says
// nothing about an author.
func live() map[arena.EntityKind][]Algo {
	works := []Algo{
		{
			ID: Hybrid, Label: "Hybrid blend (shipped)", Kind: arena.KindWork, Status: Live, Default: true,
			Blurb: "Candidate pool from tags, co-bookmarks and the tag embedding, scored by the tuned signal weights. The default because it was measured best: pool recall@1000 0.2113 against 0.1895 without the embedding source, recall@10 0.1050 against 0.1031, same 38 readers.",
			Cost:  "one pool query plus one scoring pass: 3.3-5.0 s for 1000 candidates on the live mirror",
		},
		{
			ID: Collab, Label: "Co-bookmark CF (item-item)", Kind: arena.KindWork, Status: Live,
			Blurb: "Cosine similarity of bookmarker sets, shrunk by evidence count so a three-reader pair cannot outrank a three-hundred-reader one. Captures \"readers of X also read Y\" directly.",
			Cost:  "one index lookup; the index is resident",
		},
		{
			ID: Popular, Label: "Popularity within the seed's tags", Kind: arena.KindWork, Status: Live,
			Blurb: "The baseline: the most-read works carrying the seed's tags. Not personalised, and kept for exactly that reason — a personalised list that cannot beat this is not working.",
			Cost:  "one indexed query",
		},
	}
	tags := []Algo{
		{
			ID: Embed, Label: "Nearest in the tag embedding", Kind: arena.KindTag, Status: Live, Default: true,
			Blurb: "Cosine in the 32-dimensional tag space. Measured as the default: it names the tag family a tag belongs to even when the words differ, and it reports when the tag is too central to have a meaningful neighbourhood (tag 29 sits 0.38 from the corpus centroid, and says so).",
			Cost:  "resident 81 MiB matrix, one pass over 634k rows: 0.02-0.35 s",
		},
		{
			ID: Cooccur, Label: "Tag co-occurrence", Kind: arena.KindTag, Status: Live,
			Blurb: "How often two tags are filed on the same work, normalised by how common each is. The tag-space twin of co-bookmark CF, and the only one of the two that can relate tags nobody has written about yet.",
			Cost:  "one work_tags self-join on the seed's works",
		},
		{
			ID: Popular, Label: "Most-used tags", Kind: arena.KindTag, Status: Live,
			Blurb: "The baseline: the tags carrying the most works. A neighbourhood suggestion that cannot beat this is a suggestion that means nothing.",
			Cost:  "one indexed query",
		},
	}
	authors := []Algo{
		{
			ID: CoBookAuthors, Label: "Authors sharing readers", Kind: arena.KindAuthor, Status: Live, Default: true,
			Blurb: "Authors whose works are read by the same readers as the seed author's. The only signal in this corpus that is about authors rather than about their tags.",
			Cost:  "one query over the seed author's works and their readers, capped",
		},
		{
			ID: TagIDF, Label: "Author tag overlap", Kind: arena.KindAuthor, Status: Live,
			Blurb: "Authors whose works share rare tags with the seed author's. Cheap, and it works on authors with one reader.",
			Cost:  "one work_tags query per author, cached per request",
		},
		{
			ID: Popular, Label: "Most-read authors", Kind: arena.KindAuthor, Status: Live,
			Blurb: "The baseline, by total hits across their works.",
			Cost:  "one grouped query",
		},
	}
	// The tag-ish kinds are served by the tag algorithms: in this mirror
	// characters, relationships and fandoms ARE tags filed under a tag_type that
	// says `freeforms` for all but ~600 rows, so a separate mechanism would be
	// the same query with a filter that matches nothing.
	return map[arena.EntityKind][]Algo{
		arena.KindWork:         works,
		arena.KindTag:          tags,
		arena.KindCharacter:    tags,
		arena.KindRelationship: tags,
		arena.KindFandom:       tags,
		arena.KindAuthor:       authors,
	}
}

// For returns the registry for one kind: live entries first (default first),
// then the disabled ones with their reasons.
func For(kind arena.EntityKind) []Algo {
	liveFor := live()[kind]
	out := make([]Algo, 0, len(liveFor)+len(disabled))
	out = append(out, liveFor...)
	for _, d := range disabled {
		d.Kind = kind
		out = append(out, d)
	}
	return out
}

// All returns the registry for every kind, in arena.Kinds order.
func All() map[arena.EntityKind][]Algo {
	out := make(map[arena.EntityKind][]Algo, len(arena.Kinds))
	for _, k := range arena.Kinds {
		out[k] = For(k)
	}
	return out
}

// Live returns the ids that can serve a request for this kind, default first.
func LiveIDs(kind arena.EntityKind) []ID {
	out := []ID{}
	for _, a := range For(kind) {
		if a.Status == Live {
			out = append(out, a.ID)
		}
	}
	return out
}

// DefaultFor is the measured best algorithm for a kind, or "" if the registry
// has no live entry (which the tests forbid).
func DefaultFor(kind arena.EntityKind) ID {
	for _, a := range For(kind) {
		if a.Status == Live && a.Default {
			return a.ID
		}
	}
	return ""
}

// Lookup finds one entry, and says whether it can serve this kind.
//
// A disabled algorithm is NOT found: selecting a GNN that cannot run must fall
// back to the default rather than failing a request, and the caller needs to
// know the difference between "unknown" and "known but unrunnable" when it
// reports why.
func Lookup(kind arena.EntityKind, id ID) (Algo, bool) {
	for _, a := range For(kind) {
		if a.ID == id {
			return a, a.Status == Live
		}
	}
	return Algo{}, false
}

// Parse turns user input into a live algorithm for this kind.
//
// Empty input means "no choice", and the caller decides what that means (the
// reader's stored choice, or the A/B arm). Anything invalid — unknown, or known
// but disabled — falls back to the default and returns the reason, so a page can
// say "ppr is not available here: <why>" instead of silently reordering.
func Parse(kind arena.EntityKind, s string) (ID, string) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return DefaultFor(kind), ""
	}
	for _, a := range For(kind) {
		if string(a.ID) != s {
			continue
		}
		if a.Status == Live {
			return a.ID, ""
		}
		return DefaultFor(kind), fmt.Sprintf("%s is not available on this deployment: %s", a.Label, a.Reason)
	}
	return DefaultFor(kind), fmt.Sprintf("no algorithm called %q is known for %s; using the default", s, kind.Label())
}

// Catalog is the wire shape of the registry: kinds in switcher order, each with
// its live and disabled entries.
type Catalog struct {
	Kind   arena.EntityKind `json:"kind"`
	Label  string           `json:"label"`
	Algos  []Algo           `json:"algos"`
	Active ID               `json:"default"`
}

// List is the whole registry, for the API and the page.
func List() []Catalog {
	out := make([]Catalog, 0, len(arena.Kinds))
	for _, k := range arena.Kinds {
		out = append(out, Catalog{Kind: k, Label: k.Label(), Algos: For(k), Active: DefaultFor(k)})
	}
	return out
}
