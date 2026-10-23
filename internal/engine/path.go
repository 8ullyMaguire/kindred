package engine

// Reading paths (#40): a sequential walk through the embedding space.
//
// Every other surface here ranks candidates INDEPENDENTLY against a fixed seed,
// which is exactly right for "more like this" and exactly wrong for "read me
// somewhere". A list of works all close to one seed is a fan: the tenth entry
// describes the first, and a reader who follows it learns nothing new.
//
// A path moves the reference point: each step is the work nearest the PREVIOUS
// step, so the list drifts. That drift is the product — the tenth work should
// be somewhere the first would not have reached — and it is why a path cannot be
// assembled by re-ranking a fixed pool: after the first hop the pool is wrong.
//
// Three rules, each because the alternative is a worse product:
//
//   - **A work never appears twice.** A greedy walk without that rule returns
//     A, B, A, B, and every "hop" after the second is a lie about having moved.
//
//   - **No fandom cap, and this is a finding rather than an omission.** The
//     obvious rule — "no fandom more than twice in a path" — cannot be enforced
//     on this mirror: work_tags is 3,890,504 'freeforms' rows out of 3,891,300,
//     so a cap keyed on tag_type='fandoms' would match nothing, exclude nothing,
//     and report success. That is the same defect shape documented in the
//     diversify package, where a per-group cap keyed on the same dead column
//     silently capped nothing. Instead of forcing drift with a rule that cannot
//     fire, the path MEASURES it: every step reports what it still shares with
//     the work before it, and `DriftFromSeed` says at which step the walk
//     stopped sharing any tag with the seed. A reader can see the drift; a cap
//     they cannot verify would only tell them a lie about why the list looks
//     the way it does.
//
//   - **Every hop reports its similarity to the PREVIOUS work**, not to the
//     seed. A step at 0.4 after a step at 0.9 is the interesting thing a path
//     can say: this is where the space got thin.

import (
	"context"
	"fmt"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// PathStep is one work on the walk, with the reason it is there.
type PathStep struct {
	WorkRef
	Step int `json:"step"`
	// SimToPrev is the cosine between this work's tag centroid and the
	// previous step's (the seed's, for step 1). It is the only score a path
	// step has, and it is comparable WITHIN a path, not across paths.
	SimToPrev float64 `json:"sim_to_prev"`
	// SharedWithPrev names up to three tags this work and the previous one both
	// carry, rarest first. Empty when the hop was made in embedding space alone,
	// which is the case the path exists to reach.
	SharedWithPrev []string `json:"shared_with_prev,omitempty"`
	// SharesSeedTag is false once the walk has left the seed's own tags behind,
	// which is the point at which the path is telling the reader something the
	// "more like this" list cannot.
	SharesSeedTag bool `json:"shares_seed_tag"`
}

// ReadingPath is the answer.
type ReadingPath struct {
	Seed  WorkRef    `json:"seed"`
	Steps []PathStep `json:"steps"`
	// Requested and Returned are separate because a walk can end early: the
	// candidate set runs out, and a list of 4 that says "4 of 10" is honest
	// where a silent 4 is not.
	Requested int `json:"requested"`
	Returned  int `json:"returned"`
	// Note explains an early end in words a reader can act on.
	Note string `json:"note,omitempty"`
	// DriftFromSeed is the first step index that shares no tag with the seed,
	// or 0 when every step does. It is the path's own claim about itself: a
	// path that never leaves the seed's tags is a fan with extra steps.
	DriftFromSeed int `json:"drift_from_seed,omitempty"`
	// Missing names the axes that could not run. A path without embeddings is
	// not a path: there is no space to walk.
	Missing []string `json:"missing,omitempty"`
}

// pathCandidateLimit bounds how many works each hop considers. A hop only needs
// the nearest few; considering a thousand would make a ten-step path ten full
// pool queries, and no step's winner has ever been outside the top handful.
const pathCandidateLimit = 120

// ReadingPath walks from a work, taking `steps` hops.
func (e *Engine) ReadingPath(ctx context.Context, seedID int64, steps int) (*ReadingPath, error) {
	if steps <= 0 {
		steps = 10
	}
	if steps > 40 {
		steps = 40
	}
	seedEnts, err := e.Corpus.CandidateRowsSlim(ctx, []int64{seedID})
	if err != nil {
		return nil, fmt.Errorf("load seed: %w", err)
	}
	if len(seedEnts) == 0 {
		return nil, fmt.Errorf("%w: work %d", store.ErrNotFound, seedID)
	}
	seedCands, err := e.toCandidates(ctx, seedEnts)
	if err != nil {
		return nil, err
	}
	seed := seedCands[0]
	out := &ReadingPath{
		Seed:      WorkRef{ID: seed.ID, Title: seed.Title, URL: seed.URL, Tags: len(seed.TagIDs)},
		Requested: steps,
		Steps:     make([]PathStep, 0, steps),
	}

	idx, ierr := e.tagVectors(ctx)
	if ierr != nil || idx == nil {
		// No embeddings, no space, no path. Returning the seed's ordinary
		// recommendations here would be a different feature wearing this one's
		// name.
		out.Missing = append(out.Missing, "embed_cosine")
		out.Note = "This mirror has no tag embeddings, so there is no space to walk. " +
			"The \"more like this\" list on the work page is the surface that does not need them."
		return out, nil
	}

	visited := map[int64]bool{seedID: true}
	curTags := seed.TagIDs
	seedTags := make(map[int32]bool, len(seed.TagIDs))
	for _, t := range seed.TagIDs {
		seedTags[t] = true
	}
	curCentroid := idx.centroid(curTags)
	if len(curCentroid) == 0 {
		out.Note = "None of this work's tags has an embedding, so the walk cannot take a first step."
		return out, nil
	}

	// The site's own block list, not a reader's: a path is not a session, and
	// there is no reader here to have one.
	blocked := map[int64]bool{}
	for step := 1; step <= steps; step++ {
		// The tags nearest the CURRENT point, minus the ones it already carries:
		// the current tags are what the previous step was chosen from, so
		// re-enumerating them spends the hop on the answer we already have.
		skip := map[int32]bool{}
		for _, t := range curTags {
			skip[t] = true
		}
		near := idx.nearestTags(curCentroid, 24, 0.05, skip)
		candIDs, cerr := embedCandidates(ctx, e.Corpus.DB, near, pathCandidateLimit,
			seedIDsFrom(visited), Filter{}, blocked, nil)
		if cerr != nil {
			return nil, fmt.Errorf("path step %d: %w", step, cerr)
		}
		if len(candIDs) == 0 {
			out.Note = fmt.Sprintf("The walk stopped after %d steps: nothing further in the embedding "+
				"space is close enough to where it had got to.", step-1)
			break
		}
		ents, err := e.Corpus.CandidateRowsSlim(ctx, candIDs)
		if err != nil {
			return nil, fmt.Errorf("path step %d: %w", step, err)
		}
		cands, err := e.toCandidates(ctx, ents)
		if err != nil {
			return nil, fmt.Errorf("path step %d: %w", step, err)
		}

		type scored struct {
			cand rankCandidate
			sim  float64
		}
		best := make([]scored, 0, len(cands))
		for _, c := range cands {
			if visited[c.ID] {
				continue
			}
			v := idx.centroid(c.TagIDs)
			sim, ok := cosine32(v, curCentroid)
			if !ok || sim <= 0 {
				continue
			}
			best = append(best, scored{cand: rankCandidate{ID: c.ID, Title: c.Title, URL: c.URL,
				TagIDs: c.TagIDs, TagNames: c.TagNames}, sim: sim})
		}
		if len(best) == 0 {
			out.Note = fmt.Sprintf("The walk stopped after %d steps: every work within reach has "+
				"already been visited.", step-1)
			break
		}
		// Deterministic order: similarity desc, then id asc.
		sort.SliceStable(best, func(i, j int) bool {
			if best[i].sim != best[j].sim {
				return best[i].sim > best[j].sim
			}
			return best[i].cand.ID < best[j].cand.ID
		})

		chosen := best[0]

		visited[chosen.cand.ID] = true
		sharesSeed := false
		for _, t := range chosen.cand.TagIDs {
			if seedTags[t] {
				sharesSeed = true
				break
			}
		}
		if !sharesSeed && out.DriftFromSeed == 0 {
			out.DriftFromSeed = step
		}
		out.Steps = append(out.Steps, PathStep{
			WorkRef:        WorkRef{ID: chosen.cand.ID, Title: chosen.cand.Title, URL: chosen.cand.URL, Tags: len(chosen.cand.TagIDs)},
			Step:           step,
			SimToPrev:      chosen.sim,
			SharedWithPrev: sharedTagNames(curTags, chosen.cand),
			SharesSeedTag:  sharesSeed,
		})
		curTags = chosen.cand.TagIDs
		if v := idx.centroid(curTags); len(v) > 0 {
			curCentroid = v
		}
	}
	out.Returned = len(out.Steps)
	return out, nil
}

type rankCandidate struct {
	ID       int64
	Title    string
	URL      string
	TagIDs   []int32
	TagNames []string
}

// sharedTagNames lists up to three tags both works carry, rarest first.
//
// Rarest first because the shared "fluff" tag is the least interesting thing
// about a hop and would otherwise be the first thing a reader sees.
func sharedTagNames(prev []int32, next rankCandidate) []string {
	if len(prev) == 0 || len(next.TagIDs) == 0 {
		return nil
	}
	have := make(map[int32]bool, len(prev))
	for _, t := range prev {
		have[t] = true
	}
	names := make(map[int32]string, len(next.TagIDs))
	for i, id := range next.TagIDs {
		if i < len(next.TagNames) {
			names[id] = next.TagNames[i]
		}
	}
	out := make([]string, 0, 3)
	for _, id := range next.TagIDs {
		if !have[id] {
			continue
		}
		if n := names[id]; n != "" {
			out = append(out, n)
		}
		if len(out) == 3 {
			break
		}
	}
	return out
}

// seedIDsFrom turns the visited set into the exclusion list the pool query
// wants. Sorted, so the SQL text is stable between identical requests.
func seedIDsFrom(visited map[int64]bool) []int64 {
	out := make([]int64, 0, len(visited))
	for id := range visited {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
