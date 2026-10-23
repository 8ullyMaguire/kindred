package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/collab"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/eval"
	"git.polarisocial.xyz/kindred/kindred/internal/fandom"
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
	// tune is the weight tune to rank with; empty means the built-in default.
	// It is a field rather than a package setting so an ablation is one flag
	// (`--tune no_agreement`) instead of a rebuild: the whole point of the
	// evaluator is comparing two rankers on the same split.
	tune string
	// poolMode selects the candidate sources. It is a field for the same
	// reason tune is: widening the pool is an ablation (`--pool-mode
	// tags+collab+embed`), and an ablation must not need a rebuild.
	poolMode engine.PoolMode
	// lambda is the relevance/diversity trade-off the diversification pass
	// uses. 0 means the engine's own default (0.75), so an unchanged command
	// line keeps today's behaviour; 1.0 turns diversification off, which is
	// the ablation that gives the diversity metric something to be compared
	// against.
	lambda float64
	// degraded records the signals that skipped for every candidate, from the
	// first request that completed. One request is enough -- meta.degraded is
	// a property of the engine's configuration, not of a particular reader --
	// and it must be captured here because eval.Recommender returns ids only.
	degraded []string
}

// Degraded returns the signals that never executed, sorted and deduplicated.
func (r engineRecommender) Degraded() []string { return r.degraded }

// TagsFor implements eval.TagSource: the tags a returned work carries, so the
// evaluation can measure how much one list repeats itself.
//
// It is on the recommender rather than in the evaluator because eval must not
// import the engine or the corpus (it is the offline arm and has to work against
// any Recommender), and because the ids the recommender just returned are the
// only list whose diversity is being measured.
func (r engineRecommender) TagsFor(ctx context.Context, ids []int64) (map[int64]eval.WorkTags, error) {
	if r.eng == nil || r.eng.Corpus == nil {
		return nil, fmt.Errorf("no corpus to read tags from")
	}
	pairs, err := r.eng.Corpus.TagPairs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]eval.WorkTags, len(pairs))
	for id, list := range pairs {
		wt := eval.WorkTags{IDs: make([]int32, 0, len(list)), Names: make([]string, 0, len(list))}
		for _, p := range list {
			wt.IDs = append(wt.IDs, p.ID)
			wt.Names = append(wt.Names, p.Name)
		}
		out[id] = wt
	}
	return out, nil
}

func (r engineRecommender) Recommend(ctx context.Context, seeds []int64, n int) ([]int64, error) {
	req := engine.Request{
		Kind:     corpus.AO3Kind,
		N:        n,
		Exclude:  true,
		Tune:     r.tune,
		PoolMode: r.poolMode,
		Lambda:   r.lambda,
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
	if r.degraded == nil && len(res.Meta.Degraded) > 0 {
		r.degraded = append([]string(nil), res.Meta.Degraded...)
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
		ratio    = fs.Float64("ratio", eval.DefaultRatio, "fraction of each reader's bookmarks to hold out (0<ratio<1)")
		n        = fs.Int("n", eval.DefaultK, "recommendation depth (K) to measure at")
		users    = fs.Int("users", 0, "evaluate at most this many readers (0 = every reader; a full run is minutes, not seconds)")
		asJSON   = fs.Bool("json", false, "print the report as JSON")
		pool     = fs.Int("pool", 0, "candidate pool size (0 = the mode's default)")
		lite     = fs.Bool("lite", c.Mode == "lite", "skip the embedding signal (set from --mode lite)")
		tune     = fs.String("tune", "", "weight tune to evaluate (see `kindred tune --list`; empty = the built-in default)")
		probe    = fs.Int("pool-depth", 0, "also measure candidate-generation recall at this depth (0 = off)")
		poolMode = fs.String("pool-mode", engine.DefaultPoolMode.String(), "candidate sources: tags, tags+collab, tags+embed, tags+collab+embed")

		lambda = fs.Float64("lambda", 0, "relevance vs diversity for the MMR pass (0 = engine default 0.75; 1 = no diversification). Changing it is how the diversity numbers in eval_runs earn their keep")
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
	pmode, err := engine.ParsePoolMode(*poolMode)
	if err != nil {
		return err
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

	// The document-frequency table is LOADED here for the same reason serve.go
	// loads it: the two IDF signals cannot score without it, and an evaluation
	// that quietly skips them measures a different recommender from the one
	// answering requests. This was a live defect: three runs came back
	// byte-identical because the evaluator never had the table.
	var docFreq *corpus.DocFreq
	if df, dfErr := corpus.LoadDocFreq(c.CorpusDB); dfErr != nil {
		if !errors.Is(dfErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "eval: docfreq: %v\n", dfErr)
		}
		fmt.Fprintf(os.Stderr, "eval: no document-frequency table at %s; idf_overlap and seed_agreement will skip — run \"kindred ingest\"\n",
			corpus.DocFreqPath(c.CorpusDB))
	} else {
		docFreq = df
	}

	eng := &engine.Engine{
		Store: s, Corpus: ao3, Graph: g, Collab: collabIdx,
		PoolSize: *pool, TopN: c.TopN, Lite: *lite, EmbedDim: c.EmbedDim,
		DocFreq: docFreq,
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

	if *tune != "" {
		fmt.Printf("evaluating tune %q\n", *tune)
	}
	if *lambda < 0 || *lambda > 1 {
		return fmt.Errorf("--lambda must be between 0 and 1, got %v", *lambda)
	}
	if *lambda > 0 {
		fmt.Printf("diversification lambda %.2f\n", *lambda)
	}
	rec := engineRecommender{eng: eng, tune: *tune, poolMode: pmode, lambda: *lambda}
	var runOpts []eval.Option
	if *probe > 0 {
		runOpts = append(runOpts, eval.WithPoolDepth(*probe))
	}
	rep, err := eval.Run(ctx, rec, split, *n, runOpts...)
	if err != nil {
		return err
	}
	// What never ran. Recorded before the numbers are stored so a run with a
	// blind signal cannot be compared with a complete one as if they were the
	// same recommender.
	rep.Degraded = rec.Degraded()

	// Store the run before printing it. A measurement that is not stored can
	// only be compared with someone's memory of last week, and the whole
	// reason this arm exists is comparing one run with the next. A failure to
	// record is a WARNING, not an error: the numbers are already computed and
	// a read-only state database must not make an evaluation impossible.
	runID := int64(0)
	if id, rerr := s.RecordEvalRun(ctx, store.EvalRun{
		K: rep.K, Readers: rep.Users, Bookmarks: rep.Bookmarks,
		Evaluated: rep.Evaluated, Failures: rep.Failures,
		HeldOut: rep.HeldOut, Hits: rep.Hits,
		Recall: rep.RecallAtK, NDCG: rep.NDCGAtK, Coverage: rep.Coverage,
		Mode: c.Mode, Degraded: degradedField(rep.Degraded),
		PoolDepth: rep.PoolDepth, PoolHits: rep.PoolHits, PoolRecall: rep.PoolRecall,
		TagOverlap: rep.TagOverlap, DistinctFandoms: rep.DistinctFandoms,
		DiversityRuns: rep.DiversityRuns,
	}); rerr != nil {
		fmt.Fprintf(os.Stderr, "eval: could not record this run (it was still measured): %v\n", rerr)
	} else {
		runID = id
		if !*asJSON {
			fmt.Printf("recorded as run #%d\n", id)
		}
	}

	// Where the run was weak. The aggregate recall is one number for the whole
	// mirror, and a fandom whose held-out works are never found is invisible in
	// it — which is the number that tells a reader whether this works for their
	// corner of the archive. Recorded after the run so a failure here cannot
	// lose the measurement.
	if runID > 0 && len(rep.Placements) > 0 {
		if serr := recordSlices(ctx, s, ao3, runID, rep.Placements); serr != nil {
			fmt.Fprintf(os.Stderr, "eval: could not record per-group slices (the run was still measured): %v\n", serr)
		} else if !*asJSON {
			fmt.Printf("recorded per-fandom and per-tag slices for run #%d\n", runID)
		}
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
	// What the ranking bought and what it cost. Recall alone cannot tell a
	// list of twelve variations on one fic from twelve different works, and the
	// MMR pass spends recall to avoid the first — so the two numbers print
	// together. "not measured" prints as its own sentence: zero overlap is a
	// good result and unmeasured is not.
	if rep.DiversityRuns > 0 {
		fmt.Printf("diversity: tag overlap %.3f (0 = no shared tags), %.1f fandoms per list, over %d readers\n",
			rep.TagOverlap, rep.DistinctFandoms, rep.DiversityRuns)
	} else {
		fmt.Printf("diversity: not measured -- %s\n", rep.DiversitySkipped)
	}
	if rep.Failures > 0 {
		fmt.Printf("%d recommendations failed; first: %s\n", rep.Failures, rep.Errors[0])
	}
	// Say what did not run, on every run rather than only when something looks
	// wrong. "recall 0.0067" means one thing with every signal live and
	// another with two of them blind, and the two printed identically until
	// this line existed.
	// The ceiling and the number inside it. When pool recall is near zero the
	// recommender is failing to CONSIDER the right works, and no weight change
	// can fix that; the two facts print together so they are read together.
	if rep.PoolDepth > *n {
		fmt.Printf("pool recall@%d %.4f -- the ceiling: a work the candidate set never held cannot be ranked\n",
			rep.PoolDepth, rep.PoolRecall)
	}
	if len(rep.Degraded) > 0 {
		fmt.Printf("signals that never ran: %s -- this run is not production's ranking\n",
			strings.Join(rep.Degraded, ", "))
	} else {
		fmt.Println("every registered signal contributed")
	}
	return nil
}

// degradedField is what goes in eval_runs.degraded.
//
// "all" is stored when every signal ran, so an EMPTY column keeps its one
// honest meaning: the run predates the column and nobody can say. Storing ""
// for both would make a legacy row render as "none blind", which is a claim
// about a run that never made one.
func degradedField(d []string) string {
	if len(d) == 0 {
		return "all"
	}
	return strings.Join(d, ",")
}

// sliceMinN is the smallest group worth reporting.
//
// A fandom with two held-out works has a hit rate of 0, 0.5 or 1. Printing it
// beside a group of 300 invites a comparison that is not one, so small groups
// are dropped rather than shown with a warning nobody reads.
const sliceMinN = 20

// sliceKeep bounds how many groups are stored per axis, by size: the point is
// to find the fandoms where the recommender is failing, and that is legible in
// the twenty-five biggest groups.
const sliceKeep = 25

// recordSlices groups a run's placements by fandom and by tag.
//
// The works' tags are read in CHUNKS because SQLite bounds how many parameters
// one statement may carry; asking for 36,000 ids at once is a failure that
// looks like "no tags" rather than a limit.
func recordSlices(ctx context.Context, s *store.Store, ao3 *corpus.AO3, runID int64, placements []eval.Placement) error {
	ids := make([]int64, 0, len(placements))
	seenWork := make(map[int64]bool, len(placements))
	for _, p := range placements {
		if !seenWork[p.Work] {
			seenWork[p.Work] = true
			ids = append(ids, p.Work)
		}
	}

	pairs := make(map[int64][]corpus.TagPair, len(ids))
	for start := 0; start < len(ids); start += 400 {
		end := start + 400
		if end > len(ids) {
			end = len(ids)
		}
		got, err := ao3.TagPairs(ctx, ids[start:end])
		if err != nil {
			return fmt.Errorf("tags for slices: %w", err)
		}
		for id, ps := range got {
			pairs[id] = ps
		}
	}

	// A work belongs to EVERY fandom it carries: a crossover is evidence about
	// both, not half a vote each.
	fandomOf := func(work int64) []string {
		var out []string
		seen := map[string]bool{}
		for _, p := range pairs[work] {
			if !fandom.LooksLikeFandom(p.Name) {
				continue
			}
			key := fandom.Canonical(p.Name)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, key)
		}
		return out
	}
	tagOf := func(work int64) []string {
		out := make([]string, 0, len(pairs[work]))
		for _, p := range pairs[work] {
			out = append(out, p.Name)
		}
		return out
	}

	var toStore []store.EvalSlice
	for _, axis := range []struct {
		name string
		key  func(int64) []string
	}{{"fandom", fandomOf}, {"tag", tagOf}} {
		slices := eval.ByGroup(placements, axis.name, sliceMinN, axis.key)
		if len(slices) > sliceKeep {
			slices = slices[:sliceKeep]
		}
		for _, sl := range slices {
			toStore = append(toStore, store.EvalSlice{
				Axis: sl.Axis, Name: sl.Name, N: sl.N, Hits: sl.Hits,
			})
		}
	}
	if len(toStore) == 0 {
		return nil
	}
	return s.RecordEvalSlices(ctx, runID, toStore)
}
