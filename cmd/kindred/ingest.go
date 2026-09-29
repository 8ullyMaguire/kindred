package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/budget"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func runIngest(ctx context.Context, args []string) error {
	fs := newFlagSet("ingest")
	c, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if err := fs.Parse(args); err != nil {
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

func runStats(ctx context.Context, args []string) error {
	fs := newFlagSet("stats")
	c, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if err := fs.Parse(args); err != nil {
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
