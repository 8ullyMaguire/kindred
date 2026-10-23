package web

import (
	"context"
	"net/http"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// The evaluation dashboard.
//
// Three measurements live here, and the page exists so they are seen together:
//
//   - OFFLINE: held-out bookmark prediction (recall@K, NDCG@K), persisted by
//     `kindred eval`. This is the only number that can say whether a change to
//     the weights made the recommender better without waiting for readers.
//   - EXPLICIT ONLINE: the arena's algorithm votes. A reader was shown two
//     lists and chose one.
//   - IMPLICIT ONLINE: interleaved credits. A reader was shown ONE list, and
//     what they read is the vote.
//
// They are on one page because they answer the same question at different
// costs, and a dashboard that shows only the cheap number is a dashboard that
// recommends optimising the cheap number.
func (d Deps) renderEval(w http.ResponseWriter, r *http.Request) {
	base := d.baseOf(r, "Evaluation", "Evaluation")
	page := EvalPage{Base: base}
	ctx := r.Context()

	if d.Eval == nil {
		page.Err = "this deployment has no state database, so there are no stored measurements to show"
	} else if runs, err := d.Eval.EvalRuns(ctx, 10); err != nil {
		page.Err = err.Error()
	} else {
		page.Runs = runs
		// The newest run's slices, weakest first, so the page answers "where
		// is it failing" and not only "how did it do overall". A run recorded
		// before slices existed simply has none: the section is omitted
		// rather than shown empty with an apology.
		if len(runs) > 0 {
			if slices, serr := d.Eval.EvalSlices(ctx, runs[0].ID); serr == nil && len(slices) > 0 {
				// Set only when there is something to show. The page keys its
				// heading off SliceRunID, so setting it for a run with no
				// slices would print "Where run #3 was weak" above nothing —
				// the test below caught exactly that.
				page.SliceRunID = runs[0].ID
				page.SliceMinN = evalSliceMinN
				for _, sl := range slices {
					switch sl.Axis {
					case "fandom":
						page.FandomSlices = append(page.FandomSlices, sl)
					case "tag":
						page.TagSlices = append(page.TagSlices, sl)
					}
				}
				// Weakest first: the store orders by hit rate descending so a
				// caller can take a head; reversing here is what makes the
				// first row the one worth looking at.
				weakestFirst := func(list []store.EvalSlice) {
					sort.SliceStable(list, func(i, j int) bool {
						if list[i].HitRate != list[j].HitRate {
							return list[i].HitRate < list[j].HitRate
						}
						return list[i].Name < list[j].Name
					})
				}
				weakestFirst(page.FandomSlices)
				weakestFirst(page.TagSlices)
				page.FandomSlices = headSlices(page.FandomSlices, evalSliceRows)
				page.TagSlices = headSlices(page.TagSlices, evalSliceRows)
			}
		}
	}

	// The global standings: ownerKey "" means every reader's votes, which is
	// what a dashboard wants. Per-reader standings are on /algo.
	if d.Algo != nil {
		if algos, err := d.Algo.AlgoStandings(ctx, ""); err == nil {
			page.Algos = algos
		}
	}
	if d.Interleave != nil {
		arms, pairs, err := d.Interleave.InterleaveStats(ctx)
		if err != nil {
			page.Err = err.Error()
		}
		page.Arms, page.Pairs = arms, pairs
	}
	if d.Engine != nil && d.Engine.Corpus != nil {
		works, _ := d.Engine.Corpus.CountWorks(ctx)
		tags, _ := d.Engine.Corpus.CountTags(ctx)
		wt, _ := d.Engine.Corpus.CountWorkTags(ctx)
		edges, _ := d.Engine.Corpus.CountEdges(ctx)
		page.Works, page.Tags, page.WorkTags, page.Edges = int64(works), int64(tags), int64(wt), int64(edges)
	}

	d.page(w, "eval.html", page, http.StatusOK)
}

// EvalStore is what the dashboard reads from storage.
//
// One method, because the other two measurements already arrive through the
// interfaces the pages that produce them use. A second read path for the same
// rows is a second definition of what a "run" is.
type EvalStore interface {
	EvalRuns(ctx context.Context, limit int) ([]store.EvalRun, error)
	EvalSlices(ctx context.Context, runID int64) ([]store.EvalSlice, error)
}

// evalSliceRows is how many groups of each axis the dashboard prints.
//
// Ten: the page's job is to point at a fandom worth investigating, not to be
// the whole table. The full set is in the database and one query away.
const evalSliceRows = 10

// evalSliceMinN mirrors the floor `kindred eval` applied when it recorded the
// slices. It is a constant of the page rather than a column because the page
// states the rule; the numbers it prints were filtered by it.
const evalSliceMinN = 20

func headSlices(list []store.EvalSlice, n int) []store.EvalSlice {
	if len(list) > n {
		return list[:n]
	}
	return list
}
