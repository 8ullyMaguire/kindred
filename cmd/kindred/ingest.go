package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
//
// The shapes do not line up, and that is the whole problem:
//
//   - the solver produces components: k=32 vectors, each of `nodes`
//     float32 values, one per tag. Component-major, 81 MB total.
//   - the store wants one row per (kind, entity) with a dim-vector blob:
//     entity-major, so tag t's vector is (c0[t], c1[t], ... c31[t]).
//
// Holding both to transpose in memory is 162 MB on the real corpus, on top
// of the 137 MB the solver already needs — which is how the first version
// reached 379 MB against a 220 MB cap. A per-component sink does not help:
// one component carries only 1 of the 32 coordinates, so it cannot write a
// complete row without the other 31.
//
// So the components go to disk as they are produced — 2.5 MB each, written
// through a length-tracked buffer — and a second pass reads all 32 streams
// in lockstep and writes the transposed rows. The transpose needs one
// coordinate per component at a time, so its working set is 32 buffered
// readers, not 32 vectors. Peak stays at the solver's, ~140 MB.
func buildEmbeddings(ctx context.Context, s *store.Store, nodes, dim, iters, topN int) error {
	peak := func(stage string) {
		kiB, _ := budget.PeakRSSKiB()
		fmt.Printf("  [%-18s] peak RSS %6.1f MiB\n", stage, float64(kiB)/1024)
	}
	peak("embed:matrix")
	started := time.Now()

	// Components land in a temp dir beside the state DB, so the build can be
	// retried without recomputing and so a failed run leaves no half-written
	// table behind.
	dir, err := os.MkdirTemp(filepath.Dir(s.Path), "embed-*")
	if err != nil {
		return fmt.Errorf("embed: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	files := make([]*os.File, 0, dim)
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	var res embed.BuildResult
	res, err = embed.BuildTo(ctx, s.Corpus, nodes, dim, iters, topN,
		func(component int, comp []float32) error {
			if len(comp) != nodes {
				return fmt.Errorf("embed: component %d has %d entries, want %d "+
					"(one per tag)", component, len(comp), nodes)
			}
			if component >= dim {
				return fmt.Errorf("embed: solver produced component %d but dim is %d",
					component, dim)
			}
			path := filepath.Join(dir, fmt.Sprintf("c%03d.f32", component))
			f, err := os.Create(path)
			if err != nil {
				return err
			}
			// A bufio.Writer is a fixed 4 KB buffer, and its Flush is what
			// makes the length exact. Writing the component directly as a
			// 2.5 MB byte slice is the alternative and costs that much
			// transiently; the file is written once and read once, so the
			// smaller peak is worth the extra pass through a buffer.
			w := bufio.NewWriterSize(f, 1<<16)
			if err := binary.Write(w, binary.LittleEndian, comp); err != nil {
				f.Close()
				return err
			}
			if err := w.Flush(); err != nil {
				f.Close()
				return err
			}
			files = append(files, f)
			return nil
		})
	if err != nil {
		return err
	}
	if len(files) < dim {
		return fmt.Errorf("embed: solver produced %d components, want %d; the "+
			"corpus may have too few independent signals", len(files), dim)
	}
	peak("embed:solved")

	if _, err := s.DB.ExecContext(ctx, `DELETE FROM embeddings`); err != nil {
		return err
	}
	// Batched transactions, not one for the whole table.
	//
	// The database is in WAL mode, and a single transaction inserting
	// 634,231 rows holds every page it dirties in the write-ahead log
	// until commit. Measured: the store step took the peak from 192 MB to
	// 325 MB, and the extra 133 MB was the log, not the data. Batching
	// every 5,000 rows caps the log at ~1 MB.
	//
	// Committing per row instead would be 634,231 fsyncs and minutes of
	// wall clock, which is why the batch is 5,000 rather than 1.
	//
	// A crash midway leaves a table with fewer tags than it should have,
	// so `embed_stored` is written only in the final batch: a partial set
	// is detectable rather than something a reader has to infer from
	// counting rows.
	const batch = 5000
	var tx *sql.Tx
	var stmt *sql.Stmt
	beginBatch := func() error {
		var err error
		if tx, err = s.DB.BeginTx(ctx, nil); err != nil {
			return err
		}
		if stmt, err = tx.PrepareContext(ctx,
			`INSERT INTO embeddings(kind, entity_id, dim, vec) VALUES('tag',?,?,?)`); err != nil {
			tx.Rollback()
			return err
		}
		return nil
	}
	commitBatch := func() error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}
	if err := beginBatch(); err != nil {
		return err
	}
	// The component files were written through these handles, so each is
	// positioned at its end. The transpose pass reads them from the
	// beginning: seek, or the first read is an EOF.
	for _, f := range files {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("embed: rewind component: %w", err)
		}
	}

	rowBuf := make([]byte, dim*4)
	written := 0
	for row := 0; row < nodes; row++ {
		for c := 0; c < dim; c++ {
			if err := binary.Read(files[c], binary.LittleEndian, rowBuf[c*4:c*4+4]); err != nil {
				tx.Rollback()
				return fmt.Errorf("embed: reading tag %d coordinate %d: %w", row, c, err)
			}
		}
		if _, err := stmt.ExecContext(ctx, int64(row), dim, rowBuf); err != nil {
			tx.Rollback()
			return fmt.Errorf("embed: store tag %d: %w", row, err)
		}
		written++
		if written%batch == 0 {
			if err := commitBatch(); err != nil {
				return err
			}
			if err := beginBatch(); err != nil {
				return err
			}
		}
	}
	if err := commitBatch(); err != nil {
		return err
	}
	// Checkpoint the log into the database and let SQLite shrink the WAL.
	// Without this the -wal file stays at its high-water size and the pages
	// it holds are counted in the process's RSS until the file is truncated.
	if _, err := s.DB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("embed: checkpoint: %w", err)
	}
	peak("embed:stored")

	elapsed := time.Since(started)
	fmt.Printf("embed rows=%d dim=%d top_n=%d iters=%d entries=%d in %s\n",
		res.Rows, res.Dim, res.TopN, res.Iterations, res.Entries,
		elapsed.Round(time.Millisecond))
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
