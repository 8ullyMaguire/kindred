// Held-out validation of the rated profile.
//
// ## Why this file is the important one
//
// Everything else in the ratings path -- the matcher, the shrinkage, the
// normalisation -- can be written, shipped and be useless, and nothing about
// the resulting profile LOOKS wrong. A tag weight of +0.31 for "Angst" is
// plausible whether it is well-estimated or noise. So the only thing that
// distinguishes a model that works from a model that merely exists is measured
// here, on works the build never saw, and printed next to the weights.
//
// ## What it measures
//
// Spearman rank correlation between a work's predicted enjoyment and the
// rating the reader actually gave it, over a held-out split. Spearman because
// the question is ordinal -- "of these fics, which did they like most" -- and
// because a 1.30 predicted score against an actual 8 is not an error worth
// penalising when a predicted 0.20 against that same 8 is. Pearson on raw
// scores would mostly measure how well the model reproduces the reader's
// rating scale, which is not the question.
//
// ## The baselines it prints, and why
//
// 0.00  is what a coin flip scores. It is the number to beat before any
//
//	cleverness counts for anything.
//
// 0.10  is the popularity baseline: the same correlation using kudos instead
//
//	of the profile. This is the number that matters most, because
//	"recommend the most-kudosed fic" is what the engine does today with
//	no personalisation at all. A taste model that cannot beat kudos is
//	strictly worse than the thing it replaced, and the honest thing is to
//	ship it disabled rather than claim it works.
//
// A human oracle is not printed because there isn't one to hand: there is no
// second reader here who rated these fics, so any quoted figure would be
// invented. That is worth saying out loud, because "the ceiling is 0.7" is a
// number that circulates in write-ups of recommender systems and is usually
// someone's estimate of the theoretical maximum rather than a measurement.
package profile

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
)

// ValidationResult is one held-out measurement.
type ValidationResult struct {
	// Spearman is the rank correlation between predicted and actual.
	Spearman float64
	// Baseline is the same correlation using kudos, the no-personalisation
	// alternative.
	Baseline float64
	// HeldOut and Total are the split sizes.
	HeldOut int
	Total   int
	// PerRating holds the mean predicted and actual rating per rating band,
	// so a model that predicts everything at 6.0 -- Spearman 0, mean error
	// zero -- is visible as what it is.
	PerRating []RatingBand
	// TopPredictors and TopAntiPredictors are the tags with the largest mean
	// deviation across held-out works, which is the profile's own claim about
	// what matters, checked against the works it was not fitted on.
	TopPredictors     []TagFinding
	TopAntiPredictors []TagFinding

	// findings and findingIndex are the per-tag accumulators, built during the
	// scan and folded into the two lists by finishFindings. Unexported because
	// they are scratch: an exported one would be a snapshot of a build that
	// has not been ranked yet.
	findings     []finding
	findingIndex map[int32]int
}

// RatingBand is one rating value's prediction accuracy.
type RatingBand struct {
	Rating        int
	Count         int
	MeanPredicted float64
	MeanActual    float64
}

// TagFinding is a tag's contribution, measured on held-out works.
type TagFinding struct {
	ID       int32
	MeanDev  float64
	Count    int
	Signed   float64
	NameHint string
}

// ValidateOptions configures a held-out run.
type ValidateOptions struct {
	// Holdout is the fraction of works kept back, in [0,1]. 0.2 is the
	// default the CLI passes: enough held-out works to measure a correlation
	// with usable precision (see the note on precision below), little enough to
	// keep the training half representative.
	Holdout float64
	// Seed makes the split reproducible. The CLI passes 42 because an
	// unreproducible validation is one you cannot re-run to check whether a
	// change helped, which defeats the purpose of having a number to compare.
	Seed int64
}

// Validate measures how well ratings-derived weights predict held-out ratings.
//
// The split is by WORK, not by tag, and that is the only defensible choice
// here. Holding out a tag would let the model score a work using the other
// tags it has never seen, which measures nothing. Holding out works leaves the
// tag vocabulary intact -- the model has heard of "Angst" from the training
// half -- which is exactly the deployment situation: at recommendation time
// every tag the model knows is already known, and the question is whether its
// weights are right.
func Validate(ctx context.Context, db *sql.DB, works []RatedWork, opts ValidateOptions) (*ValidationResult, error) {
	if len(works) < 20 {
		return nil, fmt.Errorf("validate: %d works is too few to measure anything; 20 is the floor", len(works))
	}
	if opts.Holdout <= 0 {
		opts.Holdout = 0.2
	}
	if opts.Holdout >= 1 {
		return nil, fmt.Errorf("validate: holdout %.2f leaves nothing to train on", opts.Holdout)
	}

	// Deterministic split from the seed, assigned per work index. Index-based
	// rather than by hashing the id: the input order is the reader's own
	// library order, so an index split is stable across runs with the same
	// input and needs no RNG state.
	split := newSplitter(opts.Seed)
	testIdx := make([]int, 0, int(float64(len(works))*opts.Holdout))
	trainIdx := make([]int, 0, len(works))
	for i := range works {
		if split.holdout(i) {
			testIdx = append(testIdx, i)
		} else {
			trainIdx = append(trainIdx, i)
		}
	}
	if len(testIdx) < 10 {
		return nil, fmt.Errorf("validate: %d held-out works is too few to correlate; raise the count or lower --holdout", len(testIdx))
	}

	train := make([]RatedWork, 0, len(trainIdx))
	for _, i := range trainIdx {
		if works[i].Rating > 0 {
			train = append(train, works[i])
		}
	}
	// Works the reader gave no number to cannot be scored against a rating, so
	// they are dropped from BOTH halves rather than trained on and then
	// mysteriously absent from the measurement.
	p, err := BuildFromRatings(ctx, db, train, "validate")
	if err != nil {
		return nil, err
	}

	tags, kudos, err := loadCandidateFacts(ctx, db, works, testIdx)
	if err != nil {
		return nil, err
	}

	res := &ValidationResult{HeldOut: len(testIdx), Total: len(works)}
	predicted := make([]float64, 0, len(testIdx))
	actual := make([]float64, 0, len(testIdx))
	baseline := make([]float64, 0, len(testIdx))
	bands := map[int]*RatingBand{}
	// `pos` is the candidate's index WITHIN the held-out slice, which is the
	// key loadCandidateFacts wrote. Using the global work index instead would
	// silently read another work's tags whenever a rating is 0 and skipped.
	for pos, i := range testIdx {
		w := works[i]
		if w.Rating <= 0 {
			continue
		}
		pred, hits := Score(p.Tags, tags[pos])
		predicted = append(predicted, pred)
		actual = append(actual, float64(w.Rating))
		baseline = append(baseline, kudos[pos])
		b := bands[w.Rating]
		if b == nil {
			b = &RatingBand{Rating: w.Rating}
			bands[w.Rating] = b
		}
		b.Count++
		b.MeanPredicted += pred
		b.MeanActual += float64(w.Rating)
		if hits > 0 {
			for _, tag := range tags[pos] {
				d, ok := p.Tags[tag]
				if !ok {
					continue
				}
				res.addFinding(tag, float64(w.Rating)-ratingCentre, d)
			}
		}
	}
	if len(predicted) < 5 {
		return nil, fmt.Errorf("validate: only %d rated held-out works; cannot correlate", len(predicted))
	}

	res.Spearman = Spearman(predicted, actual)
	res.Baseline = Spearman(baseline, actual)

	for _, b := range bands {
		b.MeanPredicted /= float64(b.Count)
		b.MeanActual /= float64(b.Count)
		res.PerRating = append(res.PerRating, *b)
	}
	sort.Slice(res.PerRating, func(i, j int) bool {
		return res.PerRating[i].Rating < res.PerRating[j].Rating
	})
	res.finishFindings()
	return res, nil
}

// addFinding accumulates one tag's mean deviation on held-out works.
//
// MeanDev is multiplied by the tag's weight, so a tag the reader liked AND
// which the profile rates highly produces a large positive finding. That is
// the joint claim being tested: not "this tag correlates with high ratings"
// but "this tag correlates with high ratings AND the model uses it that way".
func (r *ValidationResult) addFinding(tag int32, deviation, weight float64) {
	if r.findingIndex == nil {
		r.findingIndex = map[int32]int{}
	}
	if i, ok := r.findingIndex[tag]; ok {
		r.findings[i].devSum += deviation
		r.findings[i].wSum += weight
		r.findings[i].count++
		return
	}
	r.findingIndex[tag] = len(r.findings)
	r.findings = append(r.findings, finding{id: tag, devSum: deviation, wSum: weight, count: 1})
}

// finding is one tag's running accumulator.
type finding struct {
	id     int32
	devSum float64
	wSum   float64
	count  int
}

func (r *ValidationResult) finishFindings() {
	for _, f := range r.findings {
		if f.count == 0 {
			continue
		}
		meanDev := f.devSum / float64(f.count)
		meanW := f.wSum / float64(f.count)
		tf := TagFinding{ID: f.id, MeanDev: meanDev, Count: f.count, Signed: meanW}
		if meanDev*meanW > 0 {
			r.TopPredictors = append(r.TopPredictors, tf)
		} else if meanDev*meanW < 0 {
			r.TopAntiPredictors = append(r.TopAntiPredictors, tf)
		}
	}
	rank := func(s []TagFinding) {
		sort.Slice(s, func(i, j int) bool {
			a := math.Abs(s[i].MeanDev * s[i].Signed)
			b := math.Abs(s[j].MeanDev * s[j].Signed)
			if a != b {
				return a > b
			}
			return s[i].ID < s[j].ID
		})
	}
	rank(r.TopPredictors)
	rank(r.TopAntiPredictors)
	if len(r.TopPredictors) > 8 {
		r.TopPredictors = r.TopPredictors[:8]
	}
	if len(r.TopAntiPredictors) > 8 {
		r.TopAntiPredictors = r.TopAntiPredictors[:8]
	}
}

// String renders the validation for the CLI.
func (r *ValidationResult) String() string {
	out := fmt.Sprintf("held-out %d of %d works (%.0f%%)\n", r.HeldOut, r.Total,
		100*float64(r.HeldOut)/float64(r.Total))
	out += fmt.Sprintf("  Spearman rho (profile)   %+.3f\n", r.Spearman)
	out += fmt.Sprintf("  Spearman rho (kudos)     %+.3f  <- the no-personalisation baseline\n", r.Baseline)
	delta := r.Spearman - r.Baseline
	out += fmt.Sprintf("  gain over kudos          %+.3f\n", delta)
	switch {
	case r.Spearman < 0:
		out += "  VERDICT: negative. The profile is worse than random; do not ship it.\n"
	case delta <= 0.02:
		out += "  VERDICT: no better than recommending the most-kudosed fic. Not worth the complexity.\n"
	case r.Spearman < 0.2:
		out += "  VERDICT: weak signal. Ship disabled, or gather more ratings.\n"
	case r.Spearman < 0.35:
		out += "  VERDICT: usable. Worth enabling, but check the per-rating table for a flat predictor.\n"
	default:
		out += "  VERDICT: strong. Enable it.\n"
	}
	out += "\nper-rating prediction accuracy:\n"
	out += "  rating   n   predicted   actual\n"
	for _, b := range r.PerRating {
		out += fmt.Sprintf("    %-4d  %3d   %+8.3f  %6.2f\n",
			b.Rating, b.Count, b.MeanPredicted, b.MeanActual)
	}
	if len(r.TopPredictors) > 0 {
		out += "\ntop predictors (held-out):\n"
		for _, f := range r.TopPredictors {
			out += fmt.Sprintf("  tag %-8d weight %+.3f  mean deviation %+.3f over %d works\n",
				f.ID, f.Signed, f.MeanDev, f.Count)
		}
	}
	if len(r.TopAntiPredictors) > 0 {
		out += "\ntop anti-predictors (held-out):\n"
		for _, f := range r.TopAntiPredictors {
			out += fmt.Sprintf("  tag %-8d weight %+.3f  mean deviation %+.3f over %d works\n",
				f.ID, f.Signed, f.MeanDev, f.Count)
		}
	}
	// The precision caveat, printed because a correlation from 51 points is
	// not the same claim as one from 5,000 and the reader is entitled to know
	// which they have.
	if r.HeldOut < 60 {
		out += fmt.Sprintf("\nCAVEAT: rho from %d held-out works has a wide confidence\n", r.HeldOut)
		out += "interval (+/- ~0.2 at this sample size). Treat differences under\n"
		out += "0.1 between runs as noise, not improvement.\n"
	}
	return out
}

// Splitter assigns a deterministic holdout per work index.
type splitter struct{ seed int64 }

func newSplitter(seed int64) splitter { return splitter{seed: seed} }

func (s splitter) holdout(i int) bool {
	// SplitMix64 over the index and seed: stable, seed-sensitive, and with no
	// dependence on iteration order, so adding a work to the library does not
	// reshuffle which works were held out before.
	x := uint64(i)*0x9E3779B97F4A7C15 + uint64(s.seed)
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return float64(x%1000)/1000 < 0.2
}
