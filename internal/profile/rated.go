// Rated profiles: a reader's 1-10 ratings turned into signed tag weights.
//
// ## Why signed weights, and not the [0,1] of BuildFromWorks
//
// BuildFromWorks answers "which tags does this reader's collection contain",
// and a weight in [0,1] answers it. A ratings list answers a different
// question -- "which tags does this reader LIKE, and which does this reader
// DISLIKE" -- and a bounded weight cannot express the second half. Clamping a
// 2-rated fic's tags to "no opinion" is not a small loss: on this reader's
// library, 2-rated works are 21 of 257 rows and their tags are exactly the
// "Major Character Death" and "Alpha/Beta/Omega" signal that keeps those fics
// out of the recommendations. A model that cannot say "no" cannot help.
//
// So these weights are signed, centred on the middle of the rating scale, and
// stored beside the [0,1] weights rather than in place of them.
//
// ## The shrinkage
//
//	weight(t) = Σ_i (rating_i − 5.5)·conf_i·[t ∈ tags(w_i)]
//	            ─────────────────────────────────────────────────────
//	                 Σ_i conf_i·[t ∈ tags(w_i)] + prior
//
// This is a shrunk mean, and the denominator is the point. Without it a tag on
// two works whose ratings happened to be 10 and 10 would carry weight +4.5 --
// indistinguishable from "Gamer", which appears on a dozen works at 8.1 and is
// the single strongest thing about this reader's taste. Dividing by the
// evidence (plus a prior) makes a two-work tag read as "two data points,
// weakly held" instead of "the strongest signal available", which is what it
// actually is.
//
// The prior is 3 rather than 1 because the units here are rating points: a tag
// with three works' worth of evidence and an average deviation of 1.5 should
// sit well short of a tag with fifty works' worth.
//
// ## What this is not
//
// It is not collaborative filtering and it does not pretend to be. Every tag
// weight here comes from ONE reader's 257 rows, so a tag they happened to read
// once moves their profile in a way that 6,261 readers' bookmarks never could.
// The measure of whether it helps is the held-out correlation in validate.go,
// and it is printed next to the weights rather than left for someone to
// assume.
package profile

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
)

// RatedWork is one rated work, already matched to a mirror work id.
type RatedWork struct {
	WorkID int64
	// Rating is the reader's 1-10 score. Zero means the reader read it but
	// expressed no numeric opinion (Calibre's `None`).
	Rating int
	// Confidence scales this row's evidence. 1.0 for an explicit rating and
	// less for an inferred one, because "the reader bookmarked this" is
	// weaker evidence than "the reader gave it an 8" and must not move the
	// weights as far.
	Confidence float64
}

// BookmarkedConfidence is the weight given to a work the reader saved but did
// not rate.
//
// A bookmark is real evidence -- they chose to keep it -- but it is a weaker
// signal than a number, because a bookmark is also "read this later" and about
// a third of a reader's bookmarks never become reads. 0.4 is deliberately
// conservative: enough that an unrated work still contributes, not enough to
// outweigh one explicit 2.
const BookmarkedConfidence = 0.4

// DefaultPseudoRating is the rating an unrated-but-read work is treated as
// carrying, before centring.
//
// 7 of 10: above neutral, because the reader finished and kept it, but well
// below the 8-10 band their explicit high ratings cluster in, so a wall of
// bookmarks cannot manufacture enthusiasm the reader never expressed.
const DefaultPseudoRating = 7.0

// ratingCentre is the rating that maps to zero weight.
//
// 5.5, the midpoint of 1-10. It is a choice with teeth: it means a 5 and a 6
// count as opposite opinions, which is right for a scale where the reader uses
// the full range, and wrong for one who never rates below 7 -- for that reader
// half their history would read as mild dislike and the profile would quietly
// steer away from fics they loved. Validate reports the centre's effect so
// this is visible rather than buried.
const ratingCentre = 5.5

// priorStrength is the pseudo-count in the shrinkage denominator.
//
// Measured against this reader's library rather than assumed: at 3, a tag seen
// on 5 works carries about 62% of its raw mean, and a tag seen on 50 about
// 94%. Without a prior at all, every single-observation tag becomes an
// equal-weight vote with the corpus's largest fandom.
const priorStrength = 3.0

// maxTagWeight bounds a stored weight.
//
// The raw shrunk mean can reach ±4.5 for a tag whose every observed work was
// rated 1 or 10, which is a real answer but not a useful one to sum over a
// work's 34 tags. Clamping keeps one pathological tag from dominating every
// candidate that carries it, and the clamp is reported in the profile's String
// output so a saturated tag is visible rather than silently clipped.
const maxTagWeight = 1.0

// BuildFromRatings derives a signed profile from a reader's ratings.
//
// It returns a Profile whose Tags are the signed weights (normalised so the
// strongest is ±1) and whose BaseTags are the same map, because the two are
// only distinguished once online learning starts moving Tags away from what
// the ratings said. BaseTags is the memory of the library; Tags is what
// ranking reads.
func BuildFromRatings(ctx context.Context, db *sql.DB, works []RatedWork, name string) (*Profile, error) {
	if len(works) == 0 {
		return nil, fmt.Errorf("profile: no rated works to build from")
	}
	p := &Profile{
		Name:      name,
		Source:    "ratings",
		Tags:      map[int32]float64{},
		BaseTags:  map[int32]float64{},
		Evidence:  map[int32]float64{},
		Works:     len(works),
		weightSum: map[int32]float64{},
		evidSum:   map[int32]float64{},
	}

	// One query per chunk of works, not one per work. A per-work tag query
	// for 257 works is 257 round trips against a 1.7 GB mirror, and this is
	// the cost class this codebase has measured and refused three times.
	const chunk = 400
	ids := make([]any, 0, len(works))
	for _, w := range works {
		ids = append(ids, w.WorkID)
	}
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		args := make([]any, len(batch))
		copy(args, batch)
		placeholders := ""
		for i := range batch {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
		}
		rows, err := db.QueryContext(ctx,
			`SELECT DISTINCT work_id, tag_id FROM work_tags
			 WHERE work_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("profile: tags for %d works: %w", len(batch), err)
		}
		// Tags are accumulated per work first, then folded into the profile,
		// so one tag on one work contributes once no matter how many tag_types
		// it appears under. work_tags is keyed on (work_id, tag_id, tag_type),
		// so DISTINCT alone is not enough: "Gamer" can be a relationship AND
		// a freeform on the same work.
		byWork := map[int64][]int32{}
		for rows.Next() {
			var wid int64
			var tag int32
			if err := rows.Scan(&wid, &tag); err != nil {
				rows.Close()
				return nil, err
			}
			byWork[wid] = append(byWork[wid], tag)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, w := range works {
			tags, ok := byWork[w.WorkID]
			if !ok {
				continue
			}
			p.accumulate(w, tags)
		}
	}

	p.finish()
	return p, nil
}

// accumulate folds one work's rating into the running sums.
//
// tag_type is deliberately NOT filtered here, and that is a decision worth
// stating: on this mirror 3.89M of the work_tags rows are freeforms, 304 are
// relationships and 104 are fandoms, so a filter keyed on tag_type would drop
// almost everything and build a profile from 0.2% of the tags a work actually
// carries. The freeform blobs ARE the tags this mirror has -- the blurb
// scraper flattened them -- so they are what the ratings must vote on.
func (p *Profile) accumulate(w RatedWork, tags []int32) {
	rating := float64(w.Rating)
	if w.Rating == 0 {
		rating = DefaultPseudoRating
	}
	conf := w.Confidence
	if conf <= 0 {
		conf = 1.0
	}
	deviation := rating - ratingCentre

	seen := make(map[int32]bool, len(tags))
	for _, t := range tags {
		if seen[t] {
			continue
		}
		seen[t] = true
		p.weightSum[t] += deviation * conf
		p.evidSum[t] += conf
	}
}

// finish converts the running sums into shrunk, normalised weights.
func (p *Profile) finish() {
	raw := make(map[int32]float64, len(p.evidSum))
	maxAbs := 0.0
	for tag, evid := range p.evidSum {
		w := p.weightSum[tag] / (evid + priorStrength)
		raw[tag] = w
		if a := math.Abs(w); a > maxAbs {
			maxAbs = a
		}
	}
	// Normalise so the strongest tag is exactly ±1. Absolute rating points are
	// meaningless to a reader ("what does 0.62 mean?") and meaningless to the
	// blend (whose other half is rank-normalised), while "this tag is at the
	// top of your taste" is meaningful and is all the signal needs.
	scale := 1.0
	if maxAbs > 0 {
		scale = maxTagWeight / maxAbs
	}
	for tag, w := range raw {
		w *= scale
		if w > maxTagWeight {
			w = maxTagWeight
		} else if w < -maxTagWeight {
			w = -maxTagWeight
		}
		p.Tags[tag] = w
		p.BaseTags[tag] = w
		p.Evidence[tag] = p.evidSum[tag]
	}
	p.weightSum = nil
	p.evidSum = nil
}

// Top returns a profile's tags, strongest first, for display.
//
// Signed, so it is NOT the same ranking as TagIDs' "heaviest first": a tag at
// −0.9 is the reader's strongest opinion and must appear above one at +0.4.
// Sorting by magnitude is what makes the panel readable as "what the system
// thinks you love and avoid", which is the question the page exists to answer.
func (p *Profile) Top(limit int) []TagWeight {
	if limit <= 0 {
		limit = 40
	}
	out := make([]TagWeight, 0, len(p.Tags))
	for id, w := range p.Tags {
		out = append(out, TagWeight{ID: id, Weight: w, Evidence: p.Evidence[id]})
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := math.Abs(out[i].Weight), math.Abs(out[j].Weight)
		if ai != aj {
			return ai > aj
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// TagWeight is one tag's signed weight with the evidence behind it.
type TagWeight struct {
	ID       int32
	Weight   float64
	Evidence float64
}

// Weight returns a tag's signed weight and whether the profile holds it.
func (p *Profile) Weight(tagID int32) (float64, bool) {
	if p == nil {
		return 0, false
	}
	w, ok := p.Tags[tagID]
	return w, ok
}
