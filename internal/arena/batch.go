// The batch: turning recorded comparisons into ratings.
//
// Why a batch and not an update per click
// ---------------------------------------
// Glicko-2 is defined over a RATING PERIOD, and the paper is explicit
// that the system "works best when the number of games in a rating period
// is moderate to large, say an average of at least 10-15 games per player
// in a rating period". Updating on every click means every period contains
// exactly one game, which is the regime where Glicko-2's batch step is
// least appropriate and where the volatility term is noisiest.
//
// The practical consequence of getting this wrong is not subtle: a
// per-click implementation moves a rating several times further than the
// batch form for the same results, and the old kindling notes recorded
// exactly that mistake in their own section 2.3.
//
// So: comparisons accumulate, and the batch runs periodically. Everything
// in this file is about making that period boundary correct.

package arena

import (
	"math"
	"sort"
)

// PeriodOutcome is one comparison reduced to what the rating maths needs.
type PeriodOutcome struct {
	Work  int64
	Score float64
	// OppWork is who they were compared against. A work is its own
	// opponent only in the degenerate self-comparison, which the store's
	// CHECK constraint prevents.
	OppWork int64
}

// OpponentsFor groups a period's outcomes by work and builds the opponent
// list each work needs for its Update call.
//
// This is the heart of the batch and the reason it is a separate function:
// Glickman's step 5 is a per-PLAYER batch rule, so each work must see
// every opponent it faced in the period, at the opponents' PRE-period
// ratings. Getting that wrong in either direction is a real bug:
//
//   - using the opponents' UPDATED ratings makes the update order-dependent
//     and lets an early work's move inflate a later one's.
//   - updating works one at a time, each seeing only the results already
//     applied, is the same error with extra steps.
//
// So the pre-period ratings are captured FIRST, for every work in the
// period, and only then is anything written back.
func OpponentsFor(outcomes []PeriodOutcome, pre map[int64]Rating) map[int64][]Opponent {
	ops := make(map[int64][]Opponent, len(outcomes))
	for _, o := range outcomes {
		// The player's own result, from the opponent's point of view, is
		// the complement. A draw complements to a draw.
		oppScore := 1.0 - o.Score

		me, ok := pre[o.Work]
		if !ok {
			me = Initial()
		}
		them, ok := pre[o.OppWork]
		if !ok {
			them = Initial()
		}
		// Record the result for BOTH sides, because both worked.
		ops[o.Work] = append(ops[o.Work], Opponent{
			Mu: them.Mu, Phi: them.Phi, Score: o.Score,
		})
		ops[o.OppWork] = append(ops[o.OppWork], Opponent{
			Mu: me.Mu, Phi: me.Phi, Score: oppScore,
		})
	}
	return ops
}

// Apply runs one rating period over the outcomes, returning the new rating
// for every work that competed.
//
// pre must be the PRE-period rating map. apply is pure: it reads pre and
// returns a new map rather than mutating, so the caller cannot accidentally
// feed this period's output into the same period's input. That guarantee is
// worth the extra allocation, because the failure it prevents -- reading
// your own writes partway through a batch -- is invisible in review and
// makes ratings depend on map iteration order.
func Apply(pre map[int64]Rating, outcomes []PeriodOutcome) map[int64]Rating {
	ops := OpponentsFor(outcomes, pre)
	out := make(map[int64]Rating, len(ops))
	for work, opponents := range ops {
		cur, ok := pre[work]
		if !ok {
			cur = Initial()
		}
		out[work] = cur.Update(opponents)
	}
	return out
}

// Tally counts a period's results per work, for the leaderboard's
// win/loss/draw columns.
//
// The win and loss counts are for DISPLAY only; the rating comes from
// Apply. Keeping them separate matters because they answer different
// questions: a work can have a 3-0 record and a rating below a 1-2 work,
// and a leaderboard showing a raw record would be ranking the wrong thing.
func Tally(outcomes []PeriodOutcome) map[int64][3]int {
	out := make(map[int64][3]int, len(outcomes))
	for _, o := range outcomes {
		switch o.Score {
		case Win:
			t := out[o.Work]
			t[0]++
			out[o.Work] = t
		case Loss:
			t := out[o.Work]
			t[1]++
			out[o.Work] = t
		case Draw:
			t := out[o.Work]
			t[2]++
			out[o.Work] = t
		}
	}
	return out
}

// DecayIdle applies the paper's inactivity step to every work that has not
// been compared since a cutoff.
//
// The paper's note on step 6: an inactive player's RD widens to
// sqrt(phi^2 + sigma^2). This is what stops a work that was great two years
// ago from sitting frozen at the top of the leaderboard forever, and it is
// the single clearest reason this is Glicko-2 and not Elo.
//
// The subtlety is the CUTOFF, and getting it wrong is expensive in both
// directions:
//
//   - No cutoff: every rating widens on every run, so a work nobody has
//     ever seen is decayed toward maximum uncertainty, and the leaderboard
//     becomes uniformly meaningless.
//   - Cutoff on the wrong thing: decaying works that were compared in this
//     very period undoes the batch that just ran.
//
// So the cutoff is "not compared in this period", and it is applied to the
// same outcome set the batch used. Works that competed are skipped, which
// is why this is a separate pass rather than a flag inside Apply.
func DecayIdle(pre map[int64]Rating, competed map[int64]bool) map[int64]Rating {
	out := make(map[int64]Rating, len(pre))
	for work, r := range pre {
		if competed[work] {
			out[work] = r
			continue
		}
		out[work] = r.Inactivity()
	}
	return out
}

// TagSignals is what one comparison implies about a comparator's
// preferences over tags.
//
// The rule is asymmetric on purpose. When a user prefers work A over work
// B, that is evidence they like the tags A has and dislike the tags ONLY B
// has. A tag on BOTH works tells you nothing, because the user just chose
// between two works that shared it -- it could not have been the deciding
// factor. Crediting shared tags is the single most common way a
// tag-preference model learns that everything is equally important.
type TagSignals struct {
	// Positive is the work that was preferred, Negative the one passed
	// over. Both are the same works, in opposite roles.
	PositiveWork int64
	NegativeWork int64
	// PositiveTags and NegativeTags are DISJOINT by construction, for the
	// reason above.
	PositiveTags []int64
	NegativeTags []int64
}

// BuildTagSignals computes the tag deltas from one comparison.
//
// Delta is scaled by tag RARITY, not applied flat. A tag on 40% of the
// corpus is shared by nearly every work the user might see, so choosing
// one work over another because of it is weak evidence; a tag on 0.1% of
// the corpus is a strong signal. A flat delta would make the model learn
// "kancolle - fandom" above everything else within a few comparisons,
// because almost every work in the pool has it.
func BuildTagSignals(s TagSignals, tagCount map[int64]int, totalWorks int) map[int64]float64 {
	out := make(map[int64]float64, len(s.PositiveTags)+len(s.NegativeTags))
	if totalWorks <= 0 {
		return out
	}

	// A tag on BOTH works is not evidence in either direction. The set
	// difference is computed ONCE and both passes are filtered by it,
	// which is the whole point: filtering only the positive pass lets a
	// shared tag through as a pure negative, so the more works share a tag
	// the more a user is recorded as disliking it. The corpus's most
	// common tag would end up as everyone's least liked.
	shared := make(map[int64]bool, len(s.PositiveTags))
	for _, t := range s.PositiveTags {
		shared[t] = false
	}
	for _, t := range s.NegativeTags {
		if _, ok := shared[t]; ok {
			shared[t] = true
		}
	}

	for _, t := range s.PositiveTags {
		if shared[t] {
			continue
		}
		out[t] += rarity(t, tagCount[t], totalWorks)
	}
	for _, t := range s.NegativeTags {
		if shared[t] {
			continue
		}
		out[t] -= rarity(t, tagCount[t], totalWorks)
	}
	return out
}

// rarity maps a tag's corpus frequency to a weight in (0, 1].
//
// A tag on every work contributes ~0 (it discriminates nothing); a tag on
// one work contributes 1 (it is almost certainly why that work was chosen).
// The curve is logarithmic because tag frequency in this corpus spans five
// orders of magnitude, and a linear map would make every mainstream tag
// exactly zero and every rare tag exactly one.
func rarity(tag int64, count, total int) float64 {
	if count <= 0 || total <= 0 {
		return 0
	}
	p := float64(count) / float64(total)
	if p >= 1 {
		return 0
	}
	// -log10(p), squashed: p=0.5 -> 0.30, p=0.1 -> 0.50, p=0.01 -> 0.75,
	// p=0.001 -> 0.94, p=0.0001 -> 1.0.
	v := -math.Log10(p) / 4.0
	if v > 1 {
		return 1
	}
	return v
}

// WorkTags is one work's tags, and whether it is the preferred side of a
// comparison. Used by the batch to learn weights without re-querying the
// corpus per comparison.
type WorkTags struct {
	Work int64
	Tags []int64
}

// LearnTags turns a set of judged comparisons into the per-tag deltas to
// apply to one owner.
//
// positives and negatives are parallel: negatives[i] is the work that was
// passed over, positives[i] the work that was preferred. A draw or an
// unjudged presentation appears in NEITHER, because neither expresses a
// preference -- and including them would teach the model to discriminate
// on evidence the user never gave.
//
// byWork is every work involved, with its tags. The owner key is
// deliberately absent: this is pure arithmetic, so it is testable without
// a store and without a session.
func LearnTags(byWork map[int64][]int64, positives, negatives []int64,
	tagCount map[int64]int, totalWorks int) map[int64]float64 {

	out := make(map[int64]float64)
	if totalWorks <= 0 {
		return out
	}

	// Dedupe the PAIRS, not the tags. If the same two works were compared
	// more than once in a period, each comparison is real evidence and
	// should count -- but a repeated pair should not let one work's tags
	// be applied an unbounded number of times, so the cap is on the
	// number of times any single work can be the deciding side.
	const maxPerWork = 5
	timesDecided := make(map[int64]int, len(negatives))

	for i, loser := range negatives {
		if i >= len(positives) {
			break
		}
		winner := positives[i]
		if winner == loser {
			// A self-comparison. The store's CHECK prevents it, and a
			// work compared against itself contributes no information.
			continue
		}
		if timesDecided[loser] >= maxPerWork {
			continue
		}
		timesDecided[loser]++

		sig := BuildTagSignals(TagSignals{
			PositiveWork: winner,
			NegativeWork: loser,
			PositiveTags: byWork[winner],
			NegativeTags: byWork[loser],
		}, tagCount, totalWorks)

		for tag, d := range sig {
			// A tag can appear in both roles across different
			// comparisons, and those must SUM rather than overwrite. A tag
			// the user likes on the whole and disliked on one occasion
			// should sit near zero with a low n, not be whichever
			// comparison happened to be walked last.
			out[tag] += d
		}
	}
	return out
}

// Normalise scales a tag-weight map into (-1, 1] by its own maximum
// absolute value, so a user with 200 comparisons and one with 20 produce
// comparable vectors for the recommender to combine.
func Normalise(weights map[int64]float64) map[int64]float64 {
	max := 0.0
	for _, w := range weights {
		if a := absf(w); a > max {
			max = a
		}
	}
	if max == 0 {
		return map[int64]float64{}
	}
	out := make(map[int64]float64, len(weights))
	for t, w := range weights {
		out[t] = w / max
	}
	return out
}

// TopTags returns the strongest positive and negative preferences, for the
// "what I've learned about you" panel. Sorted by absolute weight, so the
// display is stable between runs.
func TopTags(weights map[int64]float64, limit int) (liked, disliked []int64) {
	if limit <= 0 {
		limit = 6
	}
	type kv struct {
		id int64
		w  float64
	}
	all := make([]kv, 0, len(weights))
	for id, w := range weights {
		if w == 0 {
			continue
		}
		all = append(all, kv{id, w})
	}
	// Sort by weight descending, then by id so the order is deterministic:
	// two runs over the same data must produce the same panel, or a user
	// watching their own profile sees it reshuffle for no reason.
	sort.Slice(all, func(i, j int) bool {
		if all[i].w != all[j].w {
			return all[i].w > all[j].w
		}
		return all[i].id < all[j].id
	})
	// Two passes, because ONE pass cannot sort both lists: the entries are
	// already in descending order overall, but the negatives appear in
	// ASCENDING order within that sequence, so a single loop hands back a
	// "disliked" list that is not sorted at all. It was in map order, and
	// the test caught it by expecting [3 4] and getting [4 3].
	//
	// Both lists also break ties by id, so a user watching their own
	// profile does not see the panel reshuffle between identical runs.
	for _, e := range all {
		if e.w > 0 {
			liked = append(liked, e.id)
		}
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].w < 0 {
			disliked = append(disliked, all[i].id)
		}
	}
	if len(liked) > limit {
		liked = liked[:limit]
	}
	if len(disliked) > limit {
		disliked = disliked[:limit]
	}
	return liked, disliked
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
