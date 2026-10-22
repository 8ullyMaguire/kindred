package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"

	"git.polarisocial.xyz/kindred/kindred/internal/collab"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/eval"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// engineRecommender adapts the ranking engine to eval.Recommender.
//
// It is in cmd rather than internal/eval for the layering rule this tree
// already uses: a measurement package that imported the engine would be
// unable to measure a second ranker without depending on both, and the engine
// would then be free to import eval and make the dependency circular.
type engineRecommender struct {
	eng *engine.Engine
}

func (r engineRecommender) Recommend(ctx context.Context, seeds []int64, n int) ([]int64, error) {
	req := engine.Request{
		Kind:    corpus.AO3Kind,
		N:       n,
		Exclude: true,
		// Every training bookmark is seen, not just the seeds. The question
		// is whether the recommender finds works this reader has NOT kept;
		// leaving the other training bookmarks in the pool lets them take
		// slots from the held-out works and depresses recall for a reason
		// that has nothing to do with ranking quality.
		SeenIDs: make(map[string]bool, len(seeds)),
	}
	for _, id := range seeds {
		req.Seeds = append(req.Seeds, engine.Seed{Kind: corpus.AO3Kind, ID: id})
		req.SeenIDs[corpus.AO3Kind+":"+strconv.FormatInt(id, 10)] = true
	}
	res, err := r.eng.Recommend(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(res.Items))
	for _, item := range res.Items {
		out = append(out, item.ID)
	}
	return out, nil
}

// runEval scores the recommender against held-out bookmarks.
//
// This is the offline arm of the north star the Tier 1 spec names. The arena
// measures one reader's preference between two works at a time; this measures
// every bookmarking reader in the mirror at once, and unlike the arena it can
// run on a schedule with no readers present.
func runEval(ctx context.Context, args []string) error {
	fs := newFlagSet("eval")
	c := bindConfig(fs)
	var (
		ratio  = fs.Float64("ratio", eval.DefaultRatio, "fraction of each reader's bookmarks to hold out (0<ratio<1)")
		n      = fs.Int("n", eval.DefaultK, "recommendation depth (K) to measure at")
		users  = fs.Int("users", 0, "evaluate at most this many readers (0 = every reader; a full run is minutes, not seconds)")
		asJSON = fs.Bool("json", false, "print the report as JSON")
		pool   = fs.Int("pool", 0, "candidate pool size (0 = the mode's default)")
		lite   = fs.Bool("lite", c.Mode == "lite", "skip the embedding signal (set from --mode lite)")
	)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}
	if *ratio <= 0 || *ratio >= 1 {
		return fmt.Errorf("--ratio must be between 0 and 1 exclusive, got %v", *ratio)
	}
	if *n <= 0 {
		return fmt.Errorf("--n must be positive, got %d", *n)
	}

	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	ao3 := corpus.NewAO3(s.Corpus)

	// The graph and the collab index are optional in exactly the way they are
	// optional for `recommend`: absent means the corresponding signals skip
	// and report themselves degraded, not that the evaluation cannot run. A
	// report from a corpus with no index still measures the tags, and saying
	// which dimensions were live is what keeps the number comparable.
	var g *graph.CSR
	g, err = graph.LoadCSR(ctx, c.DB, s.Corpus,
		func() (map[int32]int64, error) { return graph.TagFrequencies(ctx, s.Corpus) },
		func() ([]string, error) { return graph.TagNames(ctx, s.Corpus, lenFromMeta(ctx, s, "node_count")) },
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval: no co-occurrence index; neighbourhood/tag similarity signals will skip")
		g = nil
	}

	var collabIdx *collab.Index
	if idx, _, cerr := (&collab.Builder{Corpus: s.Corpus}).Build(ctx); cerr != nil {
		fmt.Fprintln(os.Stderr, "eval: no collaborative-filtering index; the collab signal will skip")
	} else {
		collabIdx = idx
	}

	eng := &engine.Engine{
		Store: s, Corpus: ao3, Graph: g, Collab: collabIdx,
		PoolSize: *pool, TopN: c.TopN, Lite: *lite, EmbedDim: c.EmbedDim,
	}
	if ratings, median, rerr := s.EffectiveRatings(ctx); rerr == nil {
		eng.SetArenaRatings(ratings, median)
	} else {
		eng.SetArenaRatings(map[int64]float64{}, 0)
	}

	byUser, readers, err := eval.LoadBookmarks(ctx, s.Corpus)
	if err != nil {
		return err
	}
	if readers == 0 {
		return errors.New("the corpus holds no bookmarked interactions; nothing to evaluate")
	}

	// --users is applied before the split so a sample run does not pay for
	// splitting readers it will not evaluate. Readers are taken in id order,
	// which is arbitrary with respect to behaviour and therefore not a
	// convenience sample that flatters the result.
	if *users > 0 && len(byUser) > *users {
		kept := make(map[int64][]int64, *users)
		ids := make([]int64, 0, len(byUser))
		for id := range byUser {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids[:*users] {
			kept[id] = byUser[id]
		}
		byUser = kept
	}

	split := eval.SplitBookmarks(byUser, *ratio)
	if len(split.Test) == 0 {
		return fmt.Errorf("no reader has at least two bookmarks; cannot hold anything out (readers=%d, too_few=%d)",
			split.Users, split.TooFew)
	}

	rep, err := eval.Run(ctx, engineRecommender{eng: eng}, split, *n)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			eval.Report
			Mode string `json:"mode"`
		}{rep, c.Mode})
	}

	fmt.Printf("evaluated %d readers (%d with too few bookmarks to split)\n",
		rep.Evaluated, rep.TooFew)
	fmt.Printf("held out %d of %d bookmarks at ratio %.2f\n",
		rep.HeldOut, rep.Bookmarks, *ratio)
	fmt.Printf("recall@%d %.4f  ndcg@%d %.4f  coverage %.4f\n",
		rep.K, rep.RecallAtK, rep.K, rep.NDCGAtK, rep.Coverage)
	if rep.Failures > 0 {
		fmt.Printf("%d recommendations failed; first: %s\n", rep.Failures, rep.Errors[0])
	}
	return nil
}
