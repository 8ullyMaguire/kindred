package engine

// Work-pair similarity (#78).
//
// The recommender ranks one seed against many candidates; this answers the
// question a reader asks when two works are put side by side — HOW are these
// alike — and it answers it with the components the ranking already uses,
// separately, instead of with a single number that hides which one carried it.
//
// Three decisions worth stating:
//
//   - **Two components, and only two.** Tag-space overlap (IDF-weighted, so a
//     shared "slow burn" counts for more than a shared "fluff") and the cosine
//     between the two works' tag centroids in the embedding space. Popularity,
//     recency and author affinity are ranking terms, not similarity: two works
//     both published in 2019 are not more alike because of it, and a pair
//     report that said so would be reporting the mirror's own preferences. The
//     pair's co-bookmark evidence belongs here too and is NOT yet wired: it
//     needs a per-pair lookup the collab index does not carry (it walks pairs
//     from a seed), and inventing a scan for this endpoint would cost more than
//     the answer is worth. It is named in `missing` rather than silently absent.
//
//   - **The score is a renormalised mean of what ran.** If the embedding index
//     is missing, the score is the tag overlap alone, and `missing` says
//     `embed_cosine`. That is the house rule for a signal that cannot run, and
//     it is what makes "0.42" comparable between a mirror with embeddings and
//     one without — it is not, which is why the components travel with it.
//
//   - **A work that does not exist is an error, not a zero.** A pair report of
//     0.0 for a made-up id would read as "nothing in common".

import (
	"context"
	"fmt"
	"math"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// WorkRef is the identifying half of a pair: enough for a client to name the
// work it asked about without a second request.
type WorkRef struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	Tags  int    `json:"tags"`
}

// SharedTag is one tag both works carry, with the rarity weight it contributed.
// The weight is published because the ordering of this list IS the explanation:
// the top row is the reason the pair scored what it scored.
type SharedTag struct {
	ID   int64   `json:"id"`
	Name string  `json:"name"`
	IDF  float64 `json:"idf"`
}

// PairSimilarity is the answer.
type PairSimilarity struct {
	A WorkRef `json:"a"`
	B WorkRef `json:"b"`

	SharedTags []SharedTag `json:"shared_tags"`

	// TagOverlap is the IDF-weighted Jaccard index over the tag sets:
	// sum(idf of shared) / sum(idf of union). Bounded by [0,1], and 1.0 means
	// the two works carry the same tags, whatever they weigh.
	TagOverlap float64 `json:"tag_overlap"`

	// EmbedCosine is the cosine between the two works' tag centroids, in
	// [-1,1]. Absent (and named in Missing) when this mirror has no embeddings.
	EmbedCosine float64 `json:"embed_cosine"`

	// Score is the mean of the components that ran. It is NOT a probability and
	// not a rank: two pairs scoring 0.4 and 0.5 are more alike, nothing more.
	Score float64 `json:"score"`

	// Missing names every component that could not run, so a reader can tell
	// "these two share nothing" from "this mirror cannot see one of the axes".
	Missing []string `json:"missing,omitempty"`
}

// Similarity reports how two works are alike.
//
// Both ids are AO3 work ids in the mirror's own numbering.
func (e *Engine) Similarity(ctx context.Context, a, b int64) (*PairSimilarity, error) {
	if a == b {
		return nil, fmt.Errorf("a work is not a pair with itself")
	}
	ents, err := e.Corpus.CandidateRowsSlim(ctx, []int64{a, b})
	if err != nil {
		return nil, fmt.Errorf("load works: %w", err)
	}
	cands, err := e.toCandidates(ctx, ents)
	if err != nil {
		return nil, fmt.Errorf("load work tags: %w", err)
	}
	byID := make(map[int64]WorkRef, len(cands))
	tags := make(map[int64][]int32, len(cands))
	for _, c := range cands {
		byID[c.ID] = WorkRef{ID: c.ID, Title: c.Title, URL: c.URL, Tags: len(c.TagIDs)}
		tags[c.ID] = c.TagIDs
	}
	for _, id := range []int64{a, b} {
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("%w: work %d", store.ErrNotFound, id)
		}
	}

	// IDF over the corpus, not over the pair. The weight of a tag is a property
	// of the mirror; computing it from two works would make "slow burn" as rare
	// as whatever else these two happen to share.
	df, total := e.docFrequencies()
	idf := func(id int32) float64 {
		if total <= 0 {
			return 1 // no index: every tag weighs the same, and the score says so via Missing
		}
		n := df[id]
		return math.Log(1 + total/(1+n))
	}

	setA := make(map[int32]bool, len(tags[a]))
	for _, t := range tags[a] {
		setA[t] = true
	}
	shared := make([]int32, 0, 8)
	for _, t := range tags[b] {
		if setA[t] {
			shared = append(shared, t)
		}
	}
	union := make(map[int32]bool, len(tags[a])+len(tags[b]))
	for _, t := range tags[a] {
		union[t] = true
	}
	for _, t := range tags[b] {
		union[t] = true
	}

	var sharedMass, unionMass float64
	for _, t := range shared {
		sharedMass += idf(t)
	}
	for t := range union {
		unionMass += idf(t)
	}
	out := &PairSimilarity{A: byID[a], B: byID[b], TagOverlap: 0}
	if unionMass > 0 {
		out.TagOverlap = sharedMass / unionMass
	}
	if total <= 0 {
		// Without a document-frequency table the weight of every tag is 1, which
		// makes the "Jaccard index" a plain one — still a real number, but not the
		// IDF-weighted one this field promises. Say so rather than mislabel it.
		out.Missing = append(out.Missing, "tag_idf")
	}

	// The shared tags, rarest first: the top row is the explanation.
	if len(shared) > 0 {
		names, err := e.Corpus.TagNames(ctx, int32SliceToInt64(shared))
		if err != nil {
			names = map[int64]string{}
		}
		out.SharedTags = make([]SharedTag, 0, len(shared))
		for _, id := range shared {
			out.SharedTags = append(out.SharedTags, SharedTag{ID: int64(id), Name: names[int64(id)], IDF: idf(id)})
		}
		sort.SliceStable(out.SharedTags, func(i, j int) bool {
			if out.SharedTags[i].IDF != out.SharedTags[j].IDF {
				return out.SharedTags[i].IDF > out.SharedTags[j].IDF
			}
			return out.SharedTags[i].ID < out.SharedTags[j].ID
		})
		if len(out.SharedTags) > 20 {
			out.SharedTags = out.SharedTags[:20]
		}
	}

	// The embedding axis. A missing index is named, not silently 0.
	idx, ierr := e.tagVectors(ctx)
	if ierr != nil || idx == nil {
		out.Missing = append(out.Missing, "embed_cosine")
	} else {
		va, vb := idx.centroid(tags[a]), idx.centroid(tags[b])
		cos, ok := cosine32(va, vb)
		if !ok {
			// Both works have tags but none of their tags is in the matrix: the
			// axis exists and has nothing to say about this pair.
			out.Missing = append(out.Missing, "embed_cosine")
		} else {
			out.EmbedCosine = cos
		}
	}

	// A renormalised mean of what ran, so a mirror without embeddings answers
	// with the tag axis rather than with a score halved by a dimension it does
	// not have.
	sum, n := 0.0, 0
	sum += out.TagOverlap
	n++
	if out.EmbedCosine != 0 {
		sum += out.EmbedCosine
		n++
	}
	out.Score = sum / float64(n)
	return out, nil
}

// cosine32 is the cosine between two vectors, and false when either is empty or
// carries no magnitude — which is a different answer from "0", since a cosine of
// zero means "unrelated" and an empty vector means "unknown".
func cosine32(a, b []float32) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0, false
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb)), true
}

func int32SliceToInt64(in []int32) []int64 {
	out := make([]int64, 0, len(in))
	for _, v := range in {
		out = append(out, int64(v))
	}
	return out
}
