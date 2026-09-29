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
	c := bindConfig(fs)
	var (
		doEmbed = fs.Bool("embed", false, "after building the index, print the command that builds the embeddings")
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
	// The embedding phase runs in a SEPARATE PROCESS.
	//
	// Measured on the real corpus: the index build peaks at 181 MiB and the
	// eigensolver adds 127 MiB of its own (the 32 x 634,232 block is 77 MiB
	// however it is stored, plus 44 MiB of matrix and 13 MiB of scratch).
	// In one process the peak is their SUM, 308 MiB against a 220 MiB cap,
	// and no amount of freeing inside the process helps: the 181 MiB is
	// SQLite's, holding a 216 MB state database, and it is resident whether
	// or not anything is reading it.
	//
	// In two processes each phase gets the whole cap. That is not a
	// workaround, it is the actual shape of the problem: the index build
	// is an I/O-bound job against a database, and the eigensolver is a
	// compute-bound job over a matrix. They share nothing except the file
	// on disk, which is the one thing processes are good at sharing.
	//
	// The first phase must therefore finish and close its database before
	// the second opens it, or SQLite's own locking makes this a race
	// rather than a sequence. The child is started after this function
	// returns, from main.
	if *doEmbed {
		if c.Mode == "lite" {
			fmt.Println("embed: skipping: --mode lite does not use the embedding signal, " +
				"and building the table would cost memory nothing reads")
		} else {
			// Everything the embed command needs is already on disk: the
			// corpus is the same file, the state db is the same file, and
			// the node count is in graph_meta. So the operator runs:
			//
			//   kindred embed --db KINDRED_DB --corpus MIRROR
			//
			// as a second command, and each process peaks on its own phase
			// rather than on the sum of both.
			fmt.Printf("embed: the index is built. Run the embedding phase as a "+
				"separate command so each fits the budget on its own:\n"+
				"  kindred embed --db %s --corpus <mirror>\n", c.DB)
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
