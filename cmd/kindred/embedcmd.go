package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/budget"
	"git.polarisocial.xyz/kindred/kindred/internal/embed"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// runEmbed builds the tag embeddings from an already-ingested corpus.
//
// This is a separate command from `ingest` because the two phases have
// peaks that ADD, and the sum does not fit the budget:
//
//	index build    181 MiB   SQLite against a 216 MB state database
//	eigensolver    127 MiB   a 32 x 634,232 float32 block is 77 MiB
//	                      however it is stored, plus 44 MiB of matrix
//	                      and 13 MiB of scratch
//	              -------
//	one process     308 MiB, against a 220 MiB cap
//	two processes   181 and 127, both under
//
// The two phases share nothing except the files on disk, which is the one
// thing separate processes are good at sharing. Freeing inside one process
// does not work: the 181 MiB is SQLite's page cache and the driver's own
// buffers, resident whether or not anything is reading them.
//
// The command refuses to run on a lite database, because lite does not
// read the embedding signal and building a 216 MB table for it would cost
// memory nothing uses.
func runEmbed(ctx context.Context, args []string) error {
	fs := newFlagSet("embed")
	var (
		dbPath   = fs.String("db", "", "path to the kindred state database (required)")
		corpus   = fs.String("corpus", "", "path to the AO3 mirror (required)")
		dim      = fs.Int("dim", 32, "embedding dimensions")
		iters    = fs.Int("iters", 60, "power iterations per component")
		topN     = fs.Int("top-n", 24, "neighbours per tag in the embedding matrix")
		mode     = fs.String("mode", "full", "full or lite")
		force    = fs.Bool("force", false, "rebuild even if embeddings already exist")
		budgetMB = fs.Int("budget-mib", 220, "refuse to start above this RSS")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return errors.New("embed: --db is required")
	}
	if *corpus == "" {
		return errors.New("embed: --corpus is required")
	}
	if *mode == "lite" {
		return errors.New("embed: refusing to build embeddings in lite mode -- " +
			"lite does not read the embedding signal, and the table would be " +
			"216 MB of memory nothing queries")
	}
	if *dim <= 0 {
		return fmt.Errorf("embed: --dim must be positive, got %d", *dim)
	}

	peak := func(stage string) {
		kiB, _ := budget.PeakRSSKiB()
		fmt.Printf("  [%-18s] peak RSS %6.1f MiB\n", stage, float64(kiB)/1024)
	}

	s, err := store.Open(ctx, *dbPath, *corpus)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return err
	}

	if !*force {
		n, err := s.EmbeddingCount(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			fmt.Printf("embed: %d embeddings already present; pass --force to rebuild\n", n)
			return nil
		}
	}

	// The node count is what the index was built with. Taking it from
	// graph_meta rather than from a flag means the matrix is always the
	// same size as the index it will be used with, which is a pairing
	// that has to hold or every embedding lookup is off by a row.
	nodes, err := nodeCountFrom(ctx, s.DB)
	if err != nil {
		return err
	}
	if nodes <= 0 {
		return fmt.Errorf("embed: no node count in graph_meta -- run `kindred ingest` first")
	}
	peak("open")
	fmt.Printf("embed nodes=%d dim=%d top_n=%d iters=%d budget=%d MiB\n",
		nodes, *dim, *topN, *iters, *budgetMB)

	// Report what SQLite says its own limits are rather than assuming the
	// DSN applied them. A pragma that did not take is invisible from Go's
	// side, and the symptom is exactly this one: memory growing with rows
	// written and attributed to nothing.
	for _, q := range []string{"cache_size", "page_size", "mmap_size", "journal_size_limit"} {
		var v string
		if err := s.DB.QueryRowContext(ctx, "PRAGMA "+q).Scan(&v); err == nil {
			fmt.Printf("embed: %-18s = %s\n", q, v)
		}
	}

	// A hard ceiling on the Go heap, so the collector runs BEFORE the peak
	// rather than never running at all.
	//
	// This is the same bug twice. The per-component write leaked 2.4 MB
	// each because binary.Write allocated a 2.54 MB slice per component in
	// a 32-iteration loop, and the store phase leaks 116 MB for the same
	// reason: 634,232 Exec calls in a tight loop allocate driver values for
	// 81 MB of blobs, and nothing in the loop is a reason for the GC to
	// think it is behind. GOGC only triggers on growth past a multiple of
	// the live heap, and here the live heap is small and stable.
	//
	// SetMemoryLimit is the right tool rather than a periodic
	// runtime.GC() call: it makes the heap a function of the budget, so
	// the peak is the budget instead of a function of how long the loop is.
	//
	// The limit is the budget less what the process is already using
	// outside the Go heap -- the corpus mapping and SQLite's own buffers --
	// measured rather than guessed, because an over-tight limit turns into
	// a GC that runs continuously and costs more time than it saves memory.
	// The limit is the budget less what is already resident and NOT in the
	// Go heap -- the corpus's mapped pages, SQLite's page cache, the Go
	// runtime's own structures. Measuring that as `peak RSS - heap sys` is
	// the honest version; an earlier attempt subtracted a HeapInuse that
	// read 0 because nothing had allocated yet, and set the limit to the
	// whole budget, which left the peak 0.4 MiB over.
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	outside := uint64(0)
	if kiB, err := budget.PeakRSSKiB(); err == nil {
		if rss := uint64(kiB) * 1024; rss > ms.HeapSys {
			outside = rss - ms.HeapSys
		}
	}
	// Reserve a slice of the budget for the transients SetMemoryLimit does
	// not govern: the driver's per-call values, the file write buffer, and
	// the stacks. Four MiB measured as the difference between the limit and
	// the observed peak.
	const reserve = 8 << 20
	limit := uint64(*budgetMB) << 20
	if limit > outside+reserve {
		limit -= outside + reserve
	} else {
		limit = 16 << 20
	}
	debug.SetMemoryLimit(int64(limit))
	defer debug.SetMemoryLimit(math.MaxInt64)
	fmt.Printf("embed: heap limit %d MiB (budget %d, %d MiB already outside the heap)\n",
		limit>>20, *budgetMB, outside>>20)

	started := time.Now()

	// The components land in a temp dir beside the state database, so a
	// failed run leaves no half-written table and no stray files.
	dir, err := os.MkdirTemp(filepath.Dir(*dbPath), "embed-*")
	if err != nil {
		return fmt.Errorf("embed: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// One encode buffer for every component, sized on first use.
	var encode []byte

	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	res, err := embed.BuildTo(ctx, s.Corpus, nodes, *dim, *iters, *topN,
		func(component int, comp []float32) error {
			if len(comp) != nodes {
				return fmt.Errorf("embed: component %d has %d entries, want %d "+
					"(one per tag)", component, len(comp), nodes)
			}
			if component >= *dim {
				return fmt.Errorf("embed: solver produced component %d but dim is %d",
					component, *dim)
			}
			if overBudget(*budgetMB) {
				return fmt.Errorf("embed: over the %d MiB budget at component %d; "+
					"lower --dim or run on a host with more memory", *budgetMB, component)
			}
			path := filepath.Join(dir, fmt.Sprintf("c%03d.f32", component))
			f, err := os.Create(path)
			if err != nil {
				return err
			}
			// The bytes go out through a reusable buffer, one float at a
			// time.
			//
			// binary.Write(­w, LittleEndian, comp) allocates a 2.54 MB
			// byte slice per component to hand to the writer, and 32 of
			// them in a loop that never yields means the collector never
			// runs: measured, the peak grew 2.4 MB per component, exactly
			// one component's worth. Packing into a buffer allocated once
			// removes the growth and the dependence on when the GC runs.
			//
			// The buffer is a field on the closure rather than a local so
			// it is the SAME allocation for all 32 components.
			if len(encode) < len(comp)*4 {
				encode = make([]byte, len(comp)*4)
			}
			for i, f32 := range comp {
				binary.LittleEndian.PutUint32(encode[i*4:], math.Float32bits(f32))
			}
			if _, err := f.Write(encode); err != nil {
				f.Close()
				return err
			}
			files = append(files, f)
			if kiB, err := budget.PeakRSSKiB(); err == nil {
				fmt.Printf("  [component %-2d] peak RSS %6.1f MiB\n",
					component, float64(kiB)/1024)
			}
			return nil
		})
	if err != nil {
		return err
	}
	if len(files) < *dim {
		return fmt.Errorf("embed: the solver produced %d components, want %d; the "+
			"corpus may have too few independent signals for this --dim",
			len(files), *dim)
	}
	peak("solved")

	if _, err := s.DB.ExecContext(ctx, `DELETE FROM embeddings`); err != nil {
		return err
	}

	// The transpose pass, in batches.
	//
	// One transaction for all 634,232 rows is what made the stored step
	// cost 115 MB: in WAL mode a transaction holds every page it dirties
	// until it commits, and these rows scatter across the b-tree so each
	// commit dirties far more pages than the 1 MB of payload it writes.
	// Measured: solve 185 MiB flat, then the store step pushed the peak to
	// 300.
	//
	// Batching bounds the dirty set. The table is on a `temp_store` file
	// and a crash leaves it with fewer tags than it should, so
	// `embed_stored` is written only after the last batch commits: a
	// partial set is detectable by a reader rather than something a reader
	// has to infer from counting rows.
	const batch = 2000
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
	if err := beginBatch(); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := f.Seek(0, 0); err != nil {
			tx.Rollback()
			return fmt.Errorf("embed: rewind component: %w", err)
		}
	}
	rowBuf := make([]byte, *dim*4)
	written := 0
	for row := 0; row < nodes; row++ {
		for c := 0; c < *dim; c++ {
			if err := binary.Read(files[c], binary.LittleEndian, rowBuf[c*4:c*4+4]); err != nil {
				tx.Rollback()
				return fmt.Errorf("embed: reading tag %d coordinate %d: %w", row, c, err)
			}
		}
		if _, err := stmt.ExecContext(ctx, int64(row), *dim, rowBuf); err != nil {
			tx.Rollback()
			return fmt.Errorf("embed: store tag %d: %w", row, err)
		}
		written++
		if written%batch == 0 {
			if err := tx.Commit(); err != nil {
				return err
			}
			if err := beginBatch(); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("embed: checkpoint: %w", err)
	}
	peak("stored")

	// Free the Go heap before the final report, so the printed peak is the
	// peak rather than the peak plus everything the process is still
	// holding. VmHWM never goes down, so this only affects RSS.
	debug.FreeOSMemory()

	elapsed := time.Since(started)
	fmt.Printf("embed rows=%d dim=%d top_n=%d iters=%d entries=%d in %s\n",
		res.Rows, res.Dim, res.TopN, res.Iterations, res.Entries,
		elapsed.Round(time.Millisecond))
	if err := s.SetMeta(ctx, "embed_dim", fmt.Sprint(res.Dim)); err != nil {
		return err
	}
	if err := s.SetMeta(ctx, "embed_stored", fmt.Sprint(nodes)); err != nil {
		return err
	}
	if err := s.Log(ctx, "embed", fmt.Sprintf("rows=%d dim=%d top_n=%d iters=%d entries=%d",
		res.Rows, res.Dim, res.TopN, res.Iterations, res.Entries)); err != nil {
		return err
	}
	if overBudget(*budgetMB) {
		return fmt.Errorf("embed: completed, but the peak was over the %d MiB budget; "+
			"the table is correct and the host is under-provisioned for --dim %d",
			*budgetMB, *dim)
	}
	return nil
}

// overBudget reports whether the high-water mark is past the cap. It reads
// VmHWM, so it is a peak and not an instantaneous reading -- which is the
// only useful definition here, since a solver that peaked and released
// still peaked.
func overBudget(capMiB int) bool {
	kiB, err := budget.PeakRSSKiB()
	if err != nil {
		return false
	}
	return kiB > capMiB*1024
}

// nodeCountFrom reads the node count the index was built with.
func nodeCountFrom(ctx context.Context, db *sql.DB) (int, error) {
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM graph_meta WHERE key = 'node_count'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return 0, fmt.Errorf("embed: node_count is %q, not a number", v)
	}
	return n, nil
}
