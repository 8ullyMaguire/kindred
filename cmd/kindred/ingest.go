package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/budget"
	"git.polarisocial.xyz/kindred/kindred/internal/embed"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func runIngest(ctx context.Context, args []string) error {
	fs := newFlagSet("ingest")
	c := bindConfig(fs)
	var (
		doEmbed    = fs.Bool("embed", false, "also compute the tag embeddings (slow; not needed in lite mode)")
		embedIters = fs.Int("embed-iters", 60, "power-iteration steps for the embeddings")
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

	start := time.Now()
	log := s.Log

	b := &graph.Builder{
		Corpus: s.Corpus, Store: execAdapter{s.DB}, TopN: c.TopN, DBPath: c.DB,
		Trace: func(stage string, peakKiB int) {
			fmt.Printf("  [%-18s] peak RSS %6.1f MiB\n", stage, float64(peakKiB)/1024)
		},
	}
	res, err := b.Build(ctx)
	if err != nil {
		return err
	}

	// The measured counts, printed as rows_in beside everything derived
	// from them. A rows_kept of zero is only diagnosable next to the count
	// that went in; without the input, a silent zero and a legitimate
	// prune look identical.
	for _, row := range []struct {
		label string
		n     int64
	}{
		{"works", res.EntityRows},
		{"tags", res.TagsIn},
		{"work_tags", res.WorkTagRow},
		{"cooccurrence_edges", res.Edges},
	} {
		fmt.Printf("corpus %-20s rows_in=%d\n", row.label, row.n)
		if err := log(ctx, "count "+row.label, fmt.Sprintf("rows_in=%d", row.n)); err != nil {
			return err
		}
	}

	// The graph node set is derived from the edges, so it can disagree
	// with the tags table. Print both: a large gap means the corpus has
	// tags nothing co-occurs with, and hiding it makes a pruned build
	// look like a complete one.
	fmt.Printf("index nodes=%d edges=%d kept_edges=%d top_n=%d csr_bytes=%d (%.1f MiB)\n",
		res.Nodes, res.Edges, res.KeptEdges, res.TopN, res.CSRBytes,
		float64(res.CSRBytes)/(1<<20))
	fmt.Printf("index tags_in=%d graph_nodes=%d pruned=%d (%.1f%% of tags are in no edge)\n",
		res.TagsIn, res.Nodes, res.TagsIn-int64(res.Nodes),
		100*float64(res.TagsIn-int64(res.Nodes))/float64(max(1, res.TagsIn)))
	if err := log(ctx, "index", fmt.Sprintf(
		"nodes=%d edges=%d kept=%d tags_in=%d top_n=%d csr_bytes=%d",
		res.Nodes, res.Edges, res.KeptEdges, res.TagsIn, res.TopN, res.CSRBytes)); err != nil {
		return err
	}

	// Embeddings. Off by default and on by flag, because the compute is
	// the slowest thing here and lite mode never reads them — the engine
	// drops the embedding signal and renormalises the remaining weights
	// instead. Building them unconditionally would make every Pi ingest
	// pay for a table nothing queries.
	if *doEmbed {
		if c.Mode == "lite" {
			fmt.Println("embed: skipping: --mode lite does not use the embedding signal, " +
				"and building the table would cost memory nothing reads")
		} else if err := buildEmbeddings(ctx, s, res.Nodes, c.EmbedDim, *embedIters, c.TopN); err != nil {
			return err
		}
	}

	if res.KeptEdges == 0 {
		return fmt.Errorf("built an index with no edges: the corpus has no co-occurrence rows " +
			"or they are all out of range — refusing to record this as a successful build")
	}

	if err := s.SetMeta(ctx, "node_count", fmt.Sprint(res.Nodes)); err != nil {
		return err
	}
	if err := s.SetMeta(ctx, "edge_count", fmt.Sprint(res.Edges)); err != nil {
		return err
	}
	if err := s.SetMeta(ctx, "work_count", fmt.Sprint(res.EntityRows)); err != nil {
		return err
	}
	if err := s.SetMeta(ctx, "top_n", fmt.Sprint(res.TopN)); err != nil {
		return err
	}
	if err := s.SetMeta(ctx, "index_built_at", store.Now()); err != nil {
		return err
	}
	if err := s.SetMeta(ctx, "corpus_built_at", corpusStamp(ctx, s.Corpus)); err != nil {
		return err
	}

	r := budget.Snapshot("after-ingest")
	fmt.Printf("ingest done in %s — peak RSS %.1f MiB\n",
		time.Since(start).Round(time.Millisecond), float64(r.PeakRSSKiB)/1024)
	return nil
}

// corpusStamp is the newest last_updated in the mirror: the age a client
// is told about through X-Kindred-Index-Age.
func corpusStamp(ctx context.Context, db *sql.DB) string {
	var newest sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT MAX(COALESCE(last_updated, first_seen)) FROM works`).Scan(&newest)
	if err != nil || !newest.Valid || newest.String == "" {
		return ""
	}
	return newest.String
}

// buildEmbeddings computes the tag embeddings and stores them.
func buildEmbeddings(ctx context.Context, s *store.Store, nodes, dim, iters, topN int) error {
	peak := func(stage string) {
		kiB, _ := budget.PeakRSSKiB()
		fmt.Printf("  [%-18s] peak RSS %6.1f MiB\n", stage, float64(kiB)/1024)
	}
	peak("embed:matrix")
	started := time.Now()
	vectors, res, err := embed.Build(ctx, s.Corpus, nodes, dim, iters, topN)
	if err != nil {
		return err
	}
	peak("embed:solved")

	if _, err := s.DB.ExecContext(ctx, `DELETE FROM embeddings`); err != nil {
		return err
	}
	// Written in batches: one row per tag, 634,231 of them, and a single
	// multi-row insert of that size is a statement SQLite has to parse
	// from a string of megabytes.
	const batch = 500
	var pending int
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO embeddings(entity_id, kind, dim, vec) VALUES(?,'tag',?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for i, v := range vectors {
		if _, err := stmt.ExecContext(ctx, int64(i), dim, embed.Pack(v)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store embedding %d: %w", i, err)
		}
		pending++
		if pending == batch {
			if err := tx.Commit(); err != nil {
				return err
			}
			if tx, err = s.DB.BeginTx(ctx, nil); err != nil {
				return err
			}
			if stmt, err = tx.PrepareContext(ctx,
				`INSERT INTO embeddings(entity_id, kind, dim, vec) VALUES(?,'tag',?,?)`); err != nil {
				tx.Rollback()
				return err
			}
			pending = 0
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	elapsed := time.Since(started)
	fmt.Printf("embed rows=%d dim=%d top_n=%d iters=%d entries=%d in %s\n",
		res.Rows, res.Dim, res.TopN, res.Iterations, res.Entries,
		elapsed.Round(time.Millisecond))
	peak("embed:stored")
	if err := s.SetMeta(ctx, "embed_dim", fmt.Sprint(res.Dim)); err != nil {
		return err
	}
	if err := s.Log(ctx, "embed", fmt.Sprintf("rows=%d dim=%d top_n=%d iters=%d entries=%d",
		res.Rows, res.Dim, res.TopN, res.Iterations, res.Entries)); err != nil {
		return err
	}
	return nil
}

func runStats(ctx context.Context, args []string) error {
	fs := newFlagSet("stats")
	c := bindConfig(fs)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return err
	}
	defer s.Close()

	for _, k := range []string{"node_count", "edge_count", "work_count", "top_n", "index_built_at", "corpus_built_at"} {
		v, err := s.Meta(ctx, k)
		if err != nil {
			return err
		}
		fmt.Printf("%-16s %s\n", k, orDash(v))
	}
	if graph.IndexExists(c.DB) {
		fmt.Printf("%-16s %d bytes\n", "index_file", graph.IndexSize(c.DB))
	} else {
		fmt.Printf("%-16s (no index — run ingest)\n", "index_file")
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
