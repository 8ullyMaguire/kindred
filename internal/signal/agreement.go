package signal

import (
	"fmt"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// SeedAgreement scores how much of EACH seed's discriminating vocabulary a
// candidate speaks, averaged over the seeds.
//
// It exists because every other tag signal here scores against the BEST seed:
// TagOverlap and IDFWeightedOverlap both take the maximum over seeds, on the
// reasoning that a work is a good match if it matches one of your likes well.
// With one seed that is the only sensible reading. With twenty it is not:
// a reader's bookmarks are several tastes, and max() lets a work that hits one
// of them score as highly as a work that hits most of them — the pool is then
// full of single-taste matches and the list says nothing about the reader.
//
// The mean over seeds INCLUDING zeros is the fix, and the zeros are the point:
// a work matching one seed perfectly and the other nineteen not at all scores
// 1/20, while a work matching three quarters of each scores 0.75. Breadth is
// rewarded because each unmatched seed contributes nothing, and depth is still
// rewarded because a full match on a seed scores 1.
//
// Each seed's coverage is IDF-weighted, so "matching" a universal tag is worth
// nothing: a work sharing only "Fluff" with a seed has not agreed with it in
// any way that distinguishes the work.
//
// It SKIPS rather than scoring zero when there is nothing to average: fewer
// than two seeds with any rare tag of their own. One seed makes this signal a
// restatement of idf_overlap with a different name, and two copies of one
// opinion is a double weight, not a second signal.
type SeedAgreement struct {
	// MaxTags bounds the work per candidate and per seed. A work with
	// thousands of freeform tags is a fandom dump, and intersecting every
	// seed against it is quadratic for no signal.
	MaxTags int

	// DocFreq and Total describe how rare each tag is, exactly as
	// IDFWeightedOverlap takes them: the corpus's tag table, loaded once
	// per request by the engine.
	DocFreq map[int32]float64
	Total   float64
}

// Name is the signal's name, and the key a tune's weight vector uses.
func (SeedAgreement) Name() string { return "seed_agreement" }

// Score returns the mean IDF-weighted coverage of the seeds.
func (s SeedAgreement) Score(c rank.Candidate, seeds []rank.Candidate, _ rank.Store) (float64, string, error) {
	// Same refusal as idf_overlap: with no frequency table there is no way
	// to tell a rare tag from a universal one, and averaging unweighted
	// coverage under a second name would report agreement that was never
	// measured. rank.Meta names the skip in `degraded`.
	if s.Total <= 1 || len(s.DocFreq) == 0 {
		return 0, "", rank.ErrSkip
	}
	if len(c.TagIDs) == 0 || len(seeds) < 2 {
		return 0, "", rank.ErrSkip
	}
	own := make(corpus.TagSet, len(c.TagIDs))
	for i, id := range c.TagIDs {
		if s.MaxTags > 0 && i >= s.MaxTags {
			break
		}
		own[id] = 1
	}

	var sum float64
	considered, agreed := 0, 0
	for _, seed := range seeds {
		var rare, matched float64
		for i, id := range seed.TagIDs {
			if s.MaxTags > 0 && i >= s.MaxTags {
				break
			}
			w := idfOf(s.DocFreq, s.Total, id)
			if w <= 0 {
				continue
			}
			rare += w
			if _, ok := own[id]; ok {
				matched += w
			}
		}
		if rare <= 0 {
			// A seed carrying no rare tag (a tagless work, or one whose
			// tags are all universal) has nothing to agree about. Counting
			// it as a zero would punish the candidate for the seed's
			// silence.
			continue
		}
		considered++
		if matched > 0 {
			agreed++
		}
		sum += matched / rare
	}
	if considered < 2 {
		return 0, "", rank.ErrSkip
	}
	return sum / float64(considered), fmt.Sprintf("agrees with %d of %d seeds", agreed, considered), nil
}
