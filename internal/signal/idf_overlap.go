package signal

import (
	"fmt"
	"math"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// IDFWeightedOverlap scores tag agreement between a candidate and the
// seeds, weighting each shared tag by how rare it is.
//
// The plain TagOverlap signal counts shared tags and divides by the union —
// a Jaccard ratio that treats every tag alike. That is wrong in a way that
// shows up immediately on AO3: "Fluff" and "General Audiences" are on a
// third of the corpus and match everything, while "Selkie AU" and
// "Villain Protagonist" are on a handful of works and are strong evidence
// when they do match. Counting them equally lets a candidate win on sheer
// commonality.
//
// This is the standard IR correction: weight a tag match by its inverse
// document frequency. A tag on N works out of M contributes log(M/N), so a
// ubiquitous tag contributes almost nothing and a rare one contributes a
// lot.
//
// It is a SEPARATE signal rather than a change to TagOverlap, because the
// two answer different questions and the tune exists to choose between
// them. TagOverlap asks "how much of the candidate do these seeds cover";
// this asks "how much does the candidate agree on the parts that
// discriminate". A deployment can weight both.
type IDFWeightedOverlap struct {
	// MaxTags bounds the work per candidate, exactly as TagOverlap does.
	// A work with thousands of freeform tags is a bot or a fandom dump.
	MaxTags int

	// DocFreq is the number of works carrying each tag id, and Total is the
	// number of works in the corpus. They come from the corpus's tag
	// table, computed once per request by the engine.
	//
	// They are inputs rather than something the signal computes because a
	// signal must not touch SQL: the layering rule in SPEC §5 puts the
	// query in the engine and the arithmetic here.
	DocFreq map[int32]float64
	Total   float64
}

// Name is the signal's name, and the key a tune's weight vector uses.
func (s IDFWeightedOverlap) Name() string { return "idf_overlap" }

// idf returns the inverse document frequency of a tag.
//
// A tag on every work in the corpus contributes 0, which is correct: it
// discriminates nothing. A tag on one work contributes log(M), which is the
// maximum evidence a single tag can carry.
//
// A tag absent from DocFreq is treated as maximally rare rather than as an
// error: the corpus's tag table and the candidate's tag list are built by
// different walks, and a tag present on a work but missing from the
// frequency table is a table-completeness gap, not a reason to fail the
// whole ranking.
func (s IDFWeightedOverlap) idf(id int32) float64 {
	return idfOf(s.DocFreq, s.Total, id)
}

// idfOf is the one definition of "how rare is this tag", shared by every
// signal that weights a match by rarity. Two copies of this formula would
// drift: one of them would end up treating a universal tag as evidence.
//
// A tag on every work in the corpus contributes 0 -- it discriminates nothing.
// A tag on one work contributes log(M). A tag absent from the table is treated
// as maximally rare rather than as an error: the frequency table and a work's
// tag list are built by different walks, and a gap in the table is not a
// reason to fail the whole ranking.
func idfOf(docFreq map[int32]float64, total float64, id int32) float64 {
	df, ok := docFreq[id]
	if !ok || df <= 0 {
		return math.Log(total)
	}
	if df >= total {
		return 0
	}
	return math.Log(total / df)
}

// Score returns the candidate's IDF-weighted agreement with the seeds.
//
// It scores against the BEST-matching seed, exactly as TagOverlap does: a
// candidate is a good match if it matches ONE of the seeds well, not if it
// matches all of them diffusely.
func (s IDFWeightedOverlap) Score(c rank.Candidate, seeds []rank.Candidate, _ rank.Store) (float64, string, error) {
	// No frequency table means no way to tell a rare tag from a universal
	// one. SKIP rather than score: cosine over unweighted tags is what
	// tag_overlap already does, and answering with a duplicate under a
	// different name would tell the reader two signals agreed when one of
	// them never ran.
	//
	// rank.Meta reports a signal that skips for every candidate in
	// `degraded`, so the response names this rather than hiding it. The
	// table is built by `kindred ingest`.
	if s.Total <= 1 || len(s.DocFreq) == 0 {
		return 0, "", rank.ErrSkip
	}
	if len(c.TagIDs) == 0 || len(seeds) == 0 {
		return 0, "", rank.ErrSkip
	}
	ids := c.TagIDs
	if s.MaxTags > 0 && len(ids) > s.MaxTags {
		ids = ids[:s.MaxTags]
	}
	if len(ids) == 0 {
		return 0, "", rank.ErrSkip
	}

	own := make(corpus.TagSet, len(ids))
	for _, id := range ids {
		own[id] = 1
	}
	if len(own) == 0 {
		return 0, "", rank.ErrSkip
	}

	// The candidate's own IDF mass, and the SEED's, and the mass of what
	// they share. The score is shared / union.
	//
	// The denominator matters more than it looks. The first version used
	// the CANDIDATE's mass as the denominator, which made the signal
	// measure containment: a candidate carrying every tag the seed
	// carries, plus ten more, scored 1.0 — the same as an exact match.
	// That is not similarity, and it let a work containing the seed
	// outrank one that simply matches it. A seed's own work is the
	// extreme case: it contains the seed's tags by definition, so it
	// scored a perfect 1.0 and climbed into the results.
	//
	// The union denominator makes the measure symmetric: a candidate that
	// shares all the seed's tags and adds none scores 1.0, and a
	// candidate that shares all the seed's tags and adds ten scores
	// strictly less. The seed itself, sharing all of its own tags, also
	// scores 1.0 against itself — which is correct, and is why the seed
	// is EXCLUDED from the pool rather than being scored down.
	candMass := 0.0
	for id := range own {
		candMass += s.idf(id)
	}
	if candMass <= 0 {
		// Every tag the candidate carries is on every work in the corpus.
		// There is no signal in it.
		return 0, "", rank.ErrSkip
	}

	// The seed sets are built once, outside the candidate loop, and reused
	// — the same per-candidate-recomputation lesson TagOverlap learned.
	seedSets := make([]corpus.TagSet, 0, len(seeds))
	seedMass := make([]float64, 0, len(seeds))
	for _, seed := range seeds {
		set := make(corpus.TagSet, len(seed.TagIDs))
		mass := 0.0
		for _, id := range seed.TagIDs {
			set[id] = 1
			mass += s.idf(id)
		}
		seedSets = append(seedSets, set)
		seedMass = append(seedMass, mass)
	}

	var best float64
	var bestSeed int64 = -1
	var bestTags []int32

	for si, seed := range seeds {
		sset := seedSets[si]
		shared, _ := corpus.Intersect(own, sset)
		if shared == 0 {
			continue
		}
		// Sum the IDF of the SHARED tags only.
		mass := 0.0
		var matched []int32
		for id := range own {
			if _, ok := sset[id]; ok {
				mass += s.idf(id)
				matched = append(matched, id)
			}
		}
		if mass <= 0 {
			// The tags they share are all ubiquitous. Sharing "General
			// Audiences" is not evidence, and scoring it as such would
			// put a zero-information match above a real one.
			continue
		}
		// The union's mass: everything the candidate carries plus
		// everything the seed carries, minus what they share (counted
		// once, not twice).
		union := candMass + seedMass[si] - mass
		if union <= 0 {
			continue
		}
		j := mass / union
		if j > best {
			best = j
			bestSeed = seed.ID
			bestTags = matched
		}
	}

	if bestSeed < 0 {
		return 0, "", rank.ErrSkip
	}

	// Name the rarest shared tag: it is the one that did the work, and it
	// is the tag a reader would point at to argue with the ranking.
	sort.Slice(bestTags, func(i, j int) bool {
		return s.idf(bestTags[i]) > s.idf(bestTags[j])
	})
	rarest := "none"
	if len(bestTags) > 0 {
		rarest = fmt.Sprintf("tag %d", bestTags[0])
	}
	reason := fmt.Sprintf("%d shared tags with seed %d, rarest %s (idf-weighted %.3f)",
		len(bestTags), bestSeed, rarest, best)
	return best, reason, nil
}

// Ensure the signal satisfies the interface.
var _ rank.Signal = IDFWeightedOverlap{}
