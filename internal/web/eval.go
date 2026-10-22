package web

import (
	"context"
	"net/http"

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
}
