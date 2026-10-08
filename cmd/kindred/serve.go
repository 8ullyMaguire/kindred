package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/api"
	"git.polarisocial.xyz/kindred/kindred/internal/budget"
	"git.polarisocial.xyz/kindred/kindred/internal/collab"
	"git.polarisocial.xyz/kindred/kindred/internal/config"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// runServe starts the read-only HTTP API.
//
// The index is memory-mapped, not loaded: mapping 26.8 MB of CSR and
// touching it per request keeps the resident set near the pages actually
// used, which is what lets the same binary serve a 16 GB host and a
// 512 MB Pi. A missing index is not fatal — the work and tag endpoints
// work without it, and the ones that need it answer 503 with a reason
// rather than pretending the corpus is empty.
func runServe(ctx context.Context, args []string) error {
	fs := newFlagSet("serve")
	noCrawl := fs.Bool("no-crawl", false,
		"disable the background auto-crawler (it fetches works readers request but the mirror lacks, "+
			"and completes stored works missing metadata, at robots.txt's crawl delay)")
	c := bindConfig(fs)
	c2, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}
	c = c2

	lite := c.Mode == "lite"
	capKiB := budget.CapKiB(c.Mode)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	ao3 := corpus.NewAO3(s.Corpus)

	// Load the graph if it is there.
	var g *graph.CSR
	// The names and the frequencies come from the CORPUS, not from the
	// index file. Passing nil callbacks here is the bug this replaces: the
	// graph loaded with 634,232 nodes and 2,884,447 edges and answered
	// every tag-similarity request with an empty list.
	//
	// The reason it hid: PMI is
	//
	//	log(P(co|occur) / (P(a) P(b)))
	//
	// and with every Frequency() returning 0 both marginals are 0, so
	// every PMI is 0, and the handler drops every pair whose PMI is not
	// positive. An empty list is the *correct* output of a graph with no
	// frequencies. The endpoints returned 200 with `returned: 0` and the
	// budget gate -- which walks routes and checks status codes -- called
	// it a pass.
	//
	// So: two callbacks that read what the graph needs from the only place
	// it exists. Tag names are 634,231 strings, which is why they are read
	// here rather than stored in the index file: the corpus already holds
	// them and a second copy in the index is a second thing to keep
	// consistent.
	g, err = graph.LoadCSR(ctx, c.DB, s.Corpus,
		func() (map[int32]int64, error) { return graph.TagFrequencies(ctx, s.Corpus) },
		func() ([]string, error) { return graph.TagNames(ctx, s.Corpus, lenFromMeta(ctx, s, "node_count")) },
	)
	if err != nil {
		// A missing or unreadable index degrades the service, it does not
		// stop it: the AO3 read surface needs no graph at all.
		logger.Warn("no co-occurrence index; tag-similarity endpoints will answer 503",
			"db", c.DB, "err", err)
		g = nil
	} else if g != nil {
		st := g.Stats()
		logger.Info("index loaded",
			"nodes", st.NodeCount, "edges", st.EdgeCount, "top_n", st.TopN)
	}

	// Embeddings: present in full mode, optional in lite.
	embedDim := c.EmbedDim
	if embedDim == 0 {
		embedDim = 32
	}
	embCount := 0
	if n, err := s.EmbeddingCount(ctx); err == nil {
		embCount = n
	}
	if embCount == 0 {
		logger.Warn("no embeddings stored; the embedding signal will skip",
			"hint", "run \"kindred ingest --embed\"")
	}

	// Collaborative filtering: the co-bookmark index over the mirror's
	// user_work_interactions.
	//
	// Built at STARTUP rather than loaded from the index file, because it is
	// small and derived: measured on the live mirror it is 12,812 pairs over
	// 9,365 works and 23 MB, against the tag graph's 7.75M edges. Persisting
	// it would add a second file that has to be invalidated whenever the
	// mirror's bookmark rows change, which is a new way for the index to be
	// stale. Rebuilding costs ~21 s once at boot.
	//
	// A failure degrades rather than stops the service, the same shape as the
	// graph above: the collab signal then skips for every candidate and names
	// itself in meta.degraded[], which is the honest report.
	var collabIdx *collab.Index
	cb := &collab.Builder{Corpus: s.Corpus}
	if cidx, cst, err := cb.Build(ctx); err != nil {
		logger.Warn("no collaborative-filtering index; the collab signal will skip",
			"err", err)
	} else {
		collabIdx = cidx
		logger.Info("collab index built",
			"gate", cb.Gate, "pairs", cst.Pairs, "pairs_before_gate", cst.PairsBeforeGate,
			"works", cst.Works, "users", cst.Users)
	}

	poolSize := engine.DefaultPoolSize
	if !lite {
		poolSize = engine.FullPoolSize
	}
	eng := &engine.Engine{
		Store:    s,
		Corpus:   ao3,
		Graph:    g,
		Collab:   collabIdx,
		PoolSize: poolSize,
		TopN:     c.TopN,
		Lite:     lite,
		EmbedDim: embedDim,
	}

	// The arena's ratings, loaded once at startup so the peer_rating signal
	// is a map lookup per candidate rather than a query. A ratings table
	// that changes only when a batch runs is exactly the kind of thing a
	// request-path query is wrong for, and a batch is rare enough that
	// reloading on a timer would be simpler than invalidating per request.
	//
	// A failure here is logged and the signal reports itself degraded, which
	// is the whole point of SPEC §7.1: an arena signal that is inert must
	// say so rather than disappear.
	if ratings, median, err := s.EffectiveRatings(ctx); err != nil {
		logger.Warn("arena ratings unavailable; peer_rating will report degraded",
			"err", err)
		eng.SetArenaRatings(map[int64]float64{}, 0)
	} else {
		eng.SetArenaRatings(ratings, median)
		logger.Info("arena ratings loaded", "works", len(ratings), "median", median)
	}

	// The deployment's standing defaults: a site-wide block list resolved to
	// ids ONCE here (startup, not per request), and the standing filters as
	// parsed by config. Both apply under whatever a request says.
	var defaultBlocked map[int64]bool
	if len(c.DefaultBlockedTags) > 0 {
		defaultBlocked = make(map[int64]bool, len(c.DefaultBlockedTags))
		for _, name := range c.DefaultBlockedTags {
			id, err := ao3.TagIDByName(ctx, name)
			if err != nil {
				// A misspelled tag in the env must not silently shrink the
				// block list: name it and refuse to start.
				logger.Error("default blocked tag not found in corpus", "tag", name)
				return fmt.Errorf("default blocked tag %q: %w", name, err)
			}
			defaultBlocked[id] = true
		}
		logger.Info("site-wide blocked tags loaded", "count", len(defaultBlocked))
	}
	if c.Standing != nil {
		logger.Info("standing filters loaded", "min_words", c.Standing.MinWords,
			"not_updated_within_days", c.Standing.NotUpdatedWithinDays,
			"complete", c.Standing.Complete, "languages", c.Standing.Languages)
	}

	srv := &api.Server{
		Engine:               eng,
		Store:                s,
		Log:                  logger,
		Version:              c.Version,
		StartedAt:            time.Now(),
		Lite:                 lite,
		Standing:             standingOrDefault(c),
		DefaultBlockedTagIDs: defaultBlocked,
	}

	// The profile store is attached here, from the STATE db rather than from
	// the corpus, and the failure is deliberately not fatal.
	//
	// A missing or unwritable state db must not stop the server: everything
	// except /profiles and /profile works without it, and a recommender that
	// refuses to boot because a taste profile could not be opened would be a
	// worse failure than one profile page. So it is logged and left nil, and
	// those two pages then answer 503 with the reason rather than 500.
	if profs, _, closeProfs, err := openProfileStore(c); err != nil {
		logger.Warn("taste profiles disabled for this server",
			"reason", err, "hint", "pass --db PATH to enable /profiles and /profile")
	} else {
		defer closeProfs()
		srv.Profiles = profs
		if err := profs.EnsureSchema(context.Background()); err != nil {
			logger.Warn("profile schema unavailable", "err", err)
			srv.Profiles = nil
		} else {
			logger.Info("taste profiles enabled", "state_db", c.DB)
		}
	}

	// Timeouts. ReadHeaderTimeout is the one that matters: without it a
	// slow client holds a connection open indefinitely, and on a Pi with
	// a small fd budget that is a denial of service by accident.
	httpSrv := &http.Server{
		Addr:              c.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	logger.Info("listening",
		"addr", c.Listen, "mode", c.Mode, "lite", lite,
		"corpus", c.CorpusDB, "index", g != nil,
		"embeddings", embCount, "rss_cap_kib", capKiB,
		"auto_crawl", !*noCrawl)

	// The auto-crawler: the mirror grows while it is used. Its whole
	// existence is off this path -- one goroutine, a queue in state.db,
	// one fetch per robots.txt's crawl delay -- and `--no-crawl` restores
	// the literal "serve never touches the network" guarantee.
	if *noCrawl {
		logger.Info("auto-crawl disabled by --no-crawl")
	} else {
		go runAutoCrawl(ctx, s, c, logger)
	}

	// The budget gate is a periodic check, not a one-off: the memory this
	// project exists to control is not a constant, and a service that
	// only checks at startup will happily drift.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				snap := budget.Snapshot("serve")
				if snap.PeakRSSKiB > capKiB {
					logger.Error("over the memory budget",
						"peak_rss_kib", snap.PeakRSSKiB, "cap_kib", capKiB,
						"phase", snap.Phase)
				}
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}

// runRecommend is the CLI form of the same recommendation the API serves,
// so the two cannot drift: both go through the engine.
func runRecommend(ctx context.Context, args []string) error {
	fs := newFlagSet("recommend")
	c := bindConfig(fs)
	var (
		seedArg     = fs.String("seed", "", "seed as kind:id (repeatable via comma)")
		kind        = fs.String("kind", "ao3_work", "the kind to recommend")
		n           = fs.Int("n", 10, "how many results")
		groupBy     = fs.String("group_by", "", "cap per group: fandom, tag, author, or empty")
		maxPerGroup = fs.Int("max_per_group", 0, "cap per group (0 disables)")
		exclude     = fs.Bool("exclude_seeds", true, "drop the seeds from the results")
		pool        = fs.Int("pool", 0, "candidate pool size (0 = the mode's default)")
		asJSON      = fs.Bool("json", false, "print the raw JSON response")
	)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}

	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return err
	}
	defer s.Close()

	ao3 := corpus.NewAO3(s.Corpus)
	var g *graph.CSR
	// The names and the frequencies come from the CORPUS, not from the
	// index file. Passing nil callbacks here is the bug this replaces: the
	// graph loaded with 634,232 nodes and 2,884,447 edges and answered
	// every tag-similarity request with an empty list.
	//
	// The reason it hid: PMI is
	//
	//	log(P(co|occur) / (P(a) P(b)))
	//
	// and with every Frequency() returning 0 both marginals are 0, so
	// every PMI is 0, and the handler drops every pair whose PMI is not
	// positive. An empty list is the *correct* output of a graph with no
	// frequencies. The endpoints returned 200 with `returned: 0` and the
	// budget gate -- which walks routes and checks status codes -- called
	// it a pass.
	//
	// So: two callbacks that read what the graph needs from the only place
	// it exists. Tag names are 634,231 strings, which is why they are read
	// here rather than stored in the index file: the corpus already holds
	// them and a second copy in the index is a second thing to keep
	// consistent.
	g, err = graph.LoadCSR(ctx, c.DB, s.Corpus,
		func() (map[int32]int64, error) { return graph.TagFrequencies(ctx, s.Corpus) },
		func() ([]string, error) { return graph.TagNames(ctx, s.Corpus, lenFromMeta(ctx, s, "node_count")) },
	)
	if err != nil {
		g = nil
	}

	eng := &engine.Engine{
		Store: s, Corpus: ao3, Graph: g,
		PoolSize: *pool, TopN: c.TopN, Lite: c.Mode == "lite", EmbedDim: c.EmbedDim,
	}

	req := &engine.Request{
		Kind: *kind, N: *n, GroupBy: *groupBy,
		MaxPerGroup: *maxPerGroup, Exclude: *exclude, PoolSize: *pool,
	}
	for _, part := range splitList(*seedArg) {
		seed, err := engine.ParseSeed(part)
		if err != nil {
			return err
		}
		req.Seeds = append(req.Seeds, seed)
	}
	if len(req.Seeds) == 0 {
		return errors.New("--seed is required, e.g. --seed ao3_work:1234")
	}

	res, err := eng.Recommend(ctx, *req)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(res)
	}

	fmt.Printf("%s recommendations from %v\n\n", res.Kind, res.Seeds)
	for i, item := range res.Items {
		fmt.Printf("%2d. %s  (score %.4f)\n", i+1, truncate(item.Title, 60), item.Score)
		if item.URL != "" {
			fmt.Printf("    %s\n", item.URL)
		}
		if len(item.Evidence) > 0 {
			for _, ev := range item.Evidence[:min(2, len(item.Evidence))] {
				fmt.Printf("    %-14s %.4f  %s\n", ev.Signal, ev.Value, truncate(ev.Reason, 70))
			}
		}
		fmt.Println()
	}
	// The shortfall line is not decoration. A short list with no
	// explanation is indistinguishable from a broken one.
	if res.Meta.Shortfall != nil {
		sh := res.Meta.Shortfall
		fmt.Printf("SHORT: %d of %d returned — %s\n", sh.Returned, sh.Requested, sh.Reason)
	}
	fmt.Printf("seeds=%d pool=%d tune=%s\n", res.Meta.Seeds, res.Meta.PoolSize, res.Meta.Tune)
	return nil
}

func splitList(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r == ' ' || r == '\t' {
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// lenFromMeta reads a numeric graph_meta value, returning 0 when absent.
func lenFromMeta(ctx context.Context, s *store.Store, key string) int {
	v, err := s.MetaInt(ctx, key)
	if err != nil {
		return 0
	}
	return v
}

// standingOrDefault unwraps the optional standing-filter pointer. A nil
// pointer is the common case: no KINDRED_STANDING_FILTERS, no defaults.
func standingOrDefault(c *config.Config) engine.Filter {
	if c == nil || c.Standing == nil {
		return engine.Filter{}
	}
	return *c.Standing
}
