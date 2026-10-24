package main

// `kindred author-eval` -- #35's measurement, before the row is wired.
//
// The question is narrow: on held-out readers, does co-readership rank an author's
// neighbours better than the tag profile `/api/v1/authors/similar` ranks them today? The
// proxy arm is the SHIPPED function -- `engine.SimilarAuthors`, called exactly as the handler
// calls it -- so the number compares the endpoint against the alternative rather than one
// reimplementation against another.
//
// The report prints each arm's recall at K, how many truth pairs existed, and how many
// candidates the proxy could place at all. That last column is the one to read first: a proxy
// that cannot place half the candidates has a ceiling on its recall that has nothing to do
// with tags being wrong.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/authorcf"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func runAuthorEval(ctx context.Context, args []string) error {
	fs := newFlagSet("author-eval")
	c := bindConfig(fs)
	var (
		seeds      = fs.Int("seeds", authorcf.DefaultParams().Seeds, "how many seed authors to evaluate")
		holdout    = fs.Int("holdout", authorcf.DefaultParams().HoldoutPercent, "percent of each seed's readers held out as truth")
		k          = fs.Int("k", authorcf.DefaultParams().K, "recall cut-off")
		candidates = fs.Int("candidates", authorcf.DefaultParams().Candidates, "candidate authors per seed")
		minReaders = fs.Int("min-readers", authorcf.DefaultParams().MinReaders, "reader floor for a seed author")
		asJSON     = fs.Bool("json", false, "print the report as JSON")
		perSeed    = fs.Bool("seeds-table", false, "print one line per seed author")
	)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}
	if *k <= 0 || *candidates <= 0 || *seeds <= 0 {
		return fmt.Errorf("--seeds, --candidates and --k must be positive")
	}
	if *holdout <= 0 || *holdout >= 100 {
		return fmt.Errorf("--holdout must be between 1 and 99")
	}

	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	idx, err := authorcf.Build(ctx, s.Corpus)
	if err != nil {
		return err
	}

	// The same document-frequency table `serve` loads: without it the tag-profile cosine has
	// no idf, and the proxy would be measured in a weaker configuration than the one that
	// answers requests. An absent table is reported, not assumed away.
	var docFreq *corpus.DocFreq
	if df, dfErr := corpus.LoadDocFreq(c.CorpusDB); dfErr != nil {
		if !errors.Is(dfErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "author-eval: docfreq: %v\n", dfErr)
		}
		fmt.Fprintln(os.Stderr, "author-eval: no document-frequency table; the proxy arm runs WITHOUT idf and is weaker than the shipped endpoint -- run \"kindred ingest\"")
	} else {
		docFreq = df
	}

	eng := &engine.Engine{
		Store:   s,
		Corpus:  corpus.NewAO3(s.Corpus),
		DocFreq: docFreq,
	}

	proxy := func(author string, limit int) ([]string, error) {
		items, perr := eng.SimilarAuthors(ctx, author, limit)
		if perr != nil {
			return nil, perr
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, it.Name)
		}
		if os.Getenv("KINDRED_AUTHOR_EVAL_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "proxy(%q) -> %d items %v\n", author, len(out), out)
		}
		return out, nil
	}

	params := authorcf.Params{
		Seeds:          *seeds,
		HoldoutPercent: *holdout,
		Candidates:     *candidates,
		K:              *k,
		MinReaders:     *minReaders,
	}
	start := time.Now()
	rep := authorcf.Measure(idx, proxy, params)
	elapsed := time.Since(start)
	tot := rep.Totals()

	if *asJSON {
		out := map[string]any{
			"params":            params,
			"totals":            tot,
			"proxy_recall_at_k": tot.Recall(tot.ProxyHits),
			"cf_recall_at_k":    tot.Recall(tot.CFHits),
			"elapsed_seconds":   elapsed.Seconds(),
			"seeds":             rep.Seeds,
			"skipped":           rep.Skipped,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	fmt.Printf("author co-readership vs tag profile, %d seed authors, holdout %d%%, k=%d\n\n",
		tot.Seeds, *holdout, *k)
	fmt.Printf("  truth pairs        %d\n", tot.Truth)
	fmt.Printf("  candidates         %d (mean %.1f per seed)\n", tot.Candidates, mean(tot.Candidates, tot.Seeds))
	fmt.Printf("  proxy ranked       %d of %d (%.0f%%)\n", tot.ProxyRanked, tot.Candidates,
		100*share(tot.ProxyRanked, tot.Candidates))
	fmt.Println()
	fmt.Printf("  recall@%-2d  tag profile (shipped)  %.4f  (%d/%d)\n", *k, tot.Recall(tot.ProxyHits), tot.ProxyHits, tot.Truth)
	fmt.Printf("  recall@%-2d  co-readership          %.4f  (%d/%d)\n", *k, tot.Recall(tot.CFHits), tot.CFHits, tot.Truth)
	if d := tot.Recall(tot.CFHits) - tot.Recall(tot.ProxyHits); d > 0 {
		fmt.Printf("  difference  +%.4f in favour of co-readership\n", d)
	} else {
		fmt.Printf("  difference  %.4f in favour of the tag profile\n", -d)
	}
	fmt.Printf("  wall clock  %.1fs\n", elapsed.Seconds())

	if len(rep.Skipped) > 0 {
		keys := make([]string, 0, len(rep.Skipped))
		for k := range rep.Skipped {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println("\nskipped:")
		for _, k := range keys {
			fmt.Printf("  %-40s %d\n", k, rep.Skipped[k])
		}
	}
	if *perSeed {
		fmt.Println("\nper seed:")
		fmt.Printf("  %-40s %7s %7s %6s %6s %6s %6s %s\n",
			"author", "readers", "held", "cands", "truth", "proxy", "cf", "proxy-ranked")
		for _, r := range rep.Seeds {
			fmt.Printf("  %-40.40s %7d %7d %6d %6d %6d %6d %d\n",
				r.Author, r.Readers, r.HeldOut, r.Candidates, r.Truth, r.ProxyHits, r.CFHits, r.ProxyRanked)
		}
	}
	return nil
}

func mean(sum, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

func share(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}
