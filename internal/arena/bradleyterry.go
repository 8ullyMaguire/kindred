package arena

// Bradley-Terry, fitted on the same duels Glicko-2 is fitted on (#53).
//
// WHY THIS EXISTS
//
// The arena rates with Glicko-2, which models a rating AND an uncertainty. A
// Bradley-Terry model asks the same data a cruder question -- "for each item, how
// strong is it relative to the others, assuming P(i beats j) = p_i/(p_i+p_j)" -- and
// answers it with one number per item. Neither is more correct. The reason to fit both
// is that a leaderboard nobody can cross-check is a leaderboard nobody can argue with:
// if BT's ordering on the same comparisons disagrees wildly with Glicko-2's, the
// winner is whichever model's assumptions the data actually meets, and if they agree,
// the page can say the ordering survives the choice of model rather than resting on it.
//
// WHAT BT DOES BETTER HERE: it has no notion of time, so it cannot be thrown by a
// rating that drifted through inactivity decay.
// WHAT GLICKO-2 DOES BETTER: uncertainty, and decay. BT prints a strength and no way
// to say how sure it is, which is why the page reports the agreement between the two
// rather than replacing one with the other.
//
// THE MLE AND THE ONE THING THAT BREAKS IT
//
// The maximum-likelihood strength of an item that won EVERY match it played is
// infinite: no finite p explains a perfect record. Real arenas have these -- a work
// compared once and preferred is the commonest row in the table. So the fit applies a
// Jeffreys prior, half a win and half a loss against an average-strength opponent
// (strength 1 after normalisation). Half of each is not a thumb on the scale: it is a
// draw against the field, which moves no item's ORDER and keeps every strength finite.
//
// The optimiser is the MM (minorise-maximise) iteration from Hunter (2004), which is
// the standard fixed point:
//
//	p_i <- W_i / sum_j n_ij / (p_i + p_j)
//
// where W_i counts i's weighted wins and n_ij the weighted matches between i and j. It
// is a contraction for this model and needs no step size. A Newton step would be
// faster and is the wrong trade: this runs on a page load, and a method that can
// diverge near a perfect record is exactly wrong for a table that is mostly perfect
// records.

import (
	"math"
	"sort"
)

// Duel is one decided comparison: Winner was preferred to Loser, Weight times. A draw
// becomes two half-weight duels, one in each direction.
type Duel struct {
	Winner int64
	Loser  int64
	Weight float64
}

// DefaultPriorMatches is the Jeffreys prior's weight in weighted matches: one match
// against an average-strength opponent, of which half counts as a win and half as a
// loss. It is a parameter rather than a constant because its size decides how much
// evidence an item needs before its own record outweighs the field, and a caller
// reporting a fit should be able to say which value produced it.
const DefaultPriorMatches = 1.0

// formulDraw is what a draw contributes to each side. It sits here rather than inline
// because a draw is the one outcome BT has no term for, and the half is a choice.
const drawHalf = 0.5

// DuelsFromOutcomes converts the arena's period outcomes into duels. Score is the
// caller's own score: Win when they were preferred, Loss when they were not, Draw
// otherwise.
func DuelsFromOutcomes(outcomes []PeriodOutcome) []Duel {
	out := make([]Duel, 0, len(outcomes))
	for _, o := range outcomes {
		w := o.Weight
		if w <= 0 {
			// Zero means "unset" on the arena's own scale (weightOf reads it as 1);
			// a negative weight means the caller meant to drop it, not to invert it.
			if w < 0 {
				continue
			}
			w = 1
		}
		if o.Work == o.OppWork {
			continue
		}
		switch {
		case o.Score > drawHalf:
			out = append(out, Duel{Winner: o.Work, Loser: o.OppWork, Weight: w})
		case o.Score < drawHalf:
			// The opponent won; the outcome is stated from this item's side.
			out = append(out, Duel{Winner: o.OppWork, Loser: o.Work, Weight: w})
		default:
			out = append(out, Duel{Winner: o.Work, Loser: o.OppWork, Weight: w * drawHalf})
			out = append(out, Duel{Winner: o.OppWork, Loser: o.Work, Weight: w * drawHalf})
		}
	}
	return out
}

// FitBT fits strengths by MM. The result is normalised so the geometric mean is 1,
// which fixes the model's one free scale: BT identifies only RATIOS, so p and 2p are
// the same model.
//
// iterations is the cap; the fit returns on convergence (max relative move 1e-10) long
// before it on any real table. Both are returned so a caller can report that the fit
// converged rather than assuming it.
func FitBT(duels []Duel) (map[int64]float64, int, bool) {
	return FitBTWithPrior(duels, DefaultPriorMatches)
}

// FitBTWithPrior is FitBT with the prior's weight exposed. prior = 0 fits the plain
// maximum-likelihood model, which is scale-invariant and DIVERGES on any item with a
// perfect record -- the trade the prior exists to make.
func FitBTWithPrior(duels []Duel, prior float64) (strengths map[int64]float64, iterations int, converged bool) {
	// Aggregate into per-entity rows. Both sides of a duel are entities, including
	// the losers who never won anything: an item with zero wins still has a strength
	// (below the prior), and dropping it would silently shorten the comparison the
	// page reports.
	type row struct {
		wins   float64
		opps   map[int64]float64 // opponent -> weighted matches
		totalN float64
	}
	rows := make(map[int64]*row, len(duels)*2)
	get := func(id int64) *row {
		r := rows[id]
		if r == nil {
			r = &row{opps: make(map[int64]float64, 4)}
			rows[id] = r
		}
		return r
	}
	for _, d := range duels {
		if d.Winner == d.Loser || d.Weight <= 0 {
			continue
		}
		w, l := get(d.Winner), get(d.Loser)
		w.wins += d.Weight
		w.opps[d.Loser] += d.Weight
		w.totalN += d.Weight
		l.opps[d.Winner] += d.Weight
		l.totalN += d.Weight
	}
	strengths = make(map[int64]float64, len(rows))
	if len(rows) == 0 {
		return strengths, 0, true
	}
	// Start flat: every item equal. A start from win rate looks faster and biases the
	// first sweeps toward the items with the most matches.
	p := make(map[int64]float64, len(rows))
	for id := range rows {
		p[id] = 1
	}

	const maxIter = 2000
	for iterations = 1; iterations <= maxIter; iterations++ {
		next := make(map[int64]float64, len(rows))
		for id, r := range rows {
			// Jeffreys: half a win and half a loss against an opponent of strength 1,
			// i.e. one weighted match against the field. Keeping the 0.5 in the
			// numerator is what stops a perfect record running to infinity.
			num := r.wins + prior*0.5
			den := prior / (p[id] + 1.0)
			for opp, n := range r.opps {
				den += n / (p[id] + p[opp])
			}
			if den <= 0 {
				next[id] = p[id]
				continue
			}
			next[id] = num / den
		}
		// Renormalise to geometric mean 1 every sweep, not only at the end: the fixed
		// point is scale-free, but the intermediate values are not, and an
		// unnormalised sweep oscillates between magnitudes instead of converging.
		sumLn, n := 0.0, 0
		for _, v := range next {
			if v > 0 && !math.IsInf(v, 0) {
				sumLn += math.Log(v)
				n++
			}
		}
		if n > 0 {
			g := math.Exp(sumLn / float64(n))
			if g > 0 && !math.IsInf(g, 0) {
				for id := range next {
					next[id] /= g
				}
			}
		}
		move := 0.0
		for id, v := range next {
			if p[id] > 0 {
				if d := math.Abs(v-p[id]) / p[id]; d > move {
					move = d
				}
			}
			p[id] = v
		}
		if move < 1e-10 {
			converged = true
			break
		}
	}
	if iterations > maxIter {
		iterations = maxIter
	}
	for id, v := range p {
		strengths[id] = v
	}
	return strengths, iterations, converged
}

// Spearman is the rank correlation between two maps over the keys they share.
//
// Ties get the average of the ranks they span, which is the standard treatment and the
// only one that leaves a table of identical ratings correlated with anything at all
// rather than arbitrarily ordered.
//
// It returns 0 with n < 2: a correlation over one item is not a small correlation, it
// is the absence of one, and a caller printing 0.00 must say which it is.
func Spearman(a, b map[int64]float64) (rho float64, n int) {
	ids := make([]int64, 0, len(a))
	for id := range a {
		if _, ok := b[id]; ok {
			ids = append(ids, id)
		}
	}
	n = len(ids)
	if n < 2 {
		return 0, n
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	ra := ranksOf(ids, a)
	rb := ranksOf(ids, b)
	// Pearson on the ranks. Written out rather than calling a stats package so the
	// tie handling above is the only thing in play.
	var sumA, sumB float64
	for i := range ids {
		sumA += ra[i]
		sumB += rb[i]
	}
	ma, mb := sumA/float64(n), sumB/float64(n)
	var num, da, db float64
	for i := range ids {
		x, y := ra[i]-ma, rb[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		// One side is constant. A constant is not correlated with anything; report 0
		// with the n that produced it, and let the caller say "no ordering to compare".
		return 0, n
	}
	return num / math.Sqrt(da*db), n
}

// ranksOf assigns 1-based ranks with ties averaged.
func ranksOf(ids []int64, v map[int64]float64) []float64 {
	type pair struct {
		i int
		v float64
	}
	order := make([]pair, len(ids))
	for i, id := range ids {
		order[i] = pair{i, v[id]}
	}
	sort.Slice(order, func(a, b int) bool { return order[a].v < order[b].v })
	out := make([]float64, len(ids))
	for i := 0; i < len(order); {
		j := i
		for j+1 < len(order) && order[j+1].v == order[i].v {
			j++
		}
		// Average of the ranks i+1 .. j+1, inclusive.
		avg := float64(i+j+2) / 2
		for k := i; k <= j; k++ {
			out[order[k].i] = avg
		}
		i = j + 1
	}
	return out
}
