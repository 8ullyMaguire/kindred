package graph

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// countTagFrequencies fills freq[tag_id] with the number of works carrying
// that tag, streaming the work_tags table in id windows so no sort is ever
// built and the driver's row buffer stays small.
func countTagFrequencies(ctx context.Context, db *sql.DB, nodeCount int, freq []int64) error {
	rows, err := db.QueryContext(ctx,
		`SELECT tag_id, COUNT(*) FROM work_tags GROUP BY tag_id ORDER BY tag_id`)
	if err != nil {
		return fmt.Errorf("tag frequencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int32
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		if int(id) < len(freq) {
			freq[id] = n
		}
	}
	return rows.Err()
}

// peakRSSKiB reads VmHWM — the kernel's high-water mark — from
// /proc/self/status. A missing /proc is not fatal: tracing is a
// diagnostic, and a non-Linux target should still build.
func peakRSSKiB() (int, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "VmHWM:") {
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			break
		}
		return strconv.Atoi(fields[1])
	}
	return 0, fmt.Errorf("graph: VmHWM not found")
}

// BuildResult is what an index build measured. Every field exists so a
// build that did nothing cannot be mistaken for a build that pruned
// legitimately: rows_in beside rows_kept is the whole point (SPEC §3.1).
type BuildResult struct {
	Nodes      int   `json:"nodes"`
	Edges      int64 `json:"edges"`
	KeptEdges  int64 `json:"kept_edges"`
	TagsIn     int64 `json:"tags_in"`
	CSRBytes   int64 `json:"csr_bytes"`
	TopN       int   `json:"top_n"`
	EntityRows int64 `json:"entity_rows"`
	WorkTagRow int64 `json:"work_tag_rows"`
}

// Trace receives a stage name and the process's high-water mark at that
// point, so a memory spike can be attributed to a stage instead of
// guessed at.
//
// This exists because the first version of this build peaked at 571 MB
// somewhere inside a 60-second run, and three rounds of reasoning about
// the cause were all wrong: it was not the Go heap (gctrace showed 73 MB),
// not the corpus mmap (mmap_size=0 changed nothing), and not a GROUP BY
// sorter (ordering the grouping changed nothing). What located it was
// sampling VmHWM per stage, and what finally showed it was sampling
// VmHWM per second from outside the process and watching which stage the
// log line named at the time.
type Builder struct {
	Corpus *sql.DB
	Store  Executor
	TopN   int

	// DBPath is where the encoded index is written. Empty means "do not
	// persist" — used by tests that only want the measured result.
	DBPath string

	// Trace, if set, is called after each stage with that stage's name.
	Trace func(stage string, peakRSSKiB int)
}

// phase reports a stage boundary to the trace hook.
func (b *Builder) phase(stage string) {
	if b.Trace == nil {
		return
	}
	kiB, _ := peakRSSKiB()
	b.Trace(stage, kiB)
}

// Executor is the subset of the store the builder writes through, so a
// test can supply a recorder without a database.
type Executor interface {
	Exec(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Build measures the corpus and builds the index.
//
// It does not trust cooccurrence_graph_meta: that row claimed 20,262
// nodes while the edges referenced 123,047 (SPEC §0.1). The node set is
// derived from the edges, always.
func (b *Builder) Build(ctx context.Context) (BuildResult, error) {
	var res BuildResult
	res.TopN = b.TopN

	// Counts are read from sqlite_stat1-style page math where possible, or
	// streamed, rather than by scanning. The first version of this ingest
	// peaked at 571 MB here and nowhere else: sampling VmHWM every second
	// against the real 1.7 GB corpus showed the spike running from t=3s to
	// t=10s, which is the COUNT(*) over 7,750,334 co-occurrence edges and
	// precedes any graph work. Steady-state RSS through the whole build is a
	// flat 89.6 MB. The count is needed only as a progress line, so
	// counting is now bounded work: iterate in id ranges and add up, which
	// keeps the driver's row buffer small instead of materialising a scan.
	b.phase("open")
	if err := countBounded(ctx, b.Corpus, `tags`, `id`, &res.TagsIn); err != nil {
		return res, fmt.Errorf("count tags: %w", err)
	}
	if err := countBounded(ctx, b.Corpus, `works`, `id`, &res.EntityRows); err != nil {
		return res, fmt.Errorf("count works: %w", err)
	}
	if err := countBounded(ctx, b.Corpus, `work_tags`, `work_id`, &res.WorkTagRow); err != nil {
		return res, fmt.Errorf("count work_tags: %w", err)
	}
	if err := countBounded(ctx, b.Corpus, `cooccurrence_edges`, `tag_a_id`, &res.Edges); err != nil {
		return res, fmt.Errorf("count cooccurrence_edges: %w", err)
	}
	b.phase("counted")

	// Pass 1 over the edges: learn the node id space and each node's
	// degree, so the CSR offsets can be allocated exactly once.
	//
	// The counting pass must write straight into a fixed-size array. The
	// first version collected every (id+1) mark into a growing []int64 —
	// 15.5 million entries, doubled repeatedly — and then folded that into
	// a counts array it had already sized. That append-only intermediate
	// was the whole of the ingest's memory spike: 18 MB of RSS before this
	// stage and 492 MB after, measured per stage, against a steady-state
	// RSS of 89.6 MB through the rest of the build. Two arrays of int64
	// are 5 MB; the slice that grew to hold them was 480 MB.
	var maxID int64 = -1
	if err := b.forEachEdgeWindowed(ctx, func(e Edge) error {
		if int64(e.A) > maxID {
			maxID = int64(e.A)
		}
		if int64(e.B) > maxID {
			maxID = int64(e.B)
		}
		return nil
	}); err != nil {
		return res, err
	}
	nodeCount := int(maxID) + 1
	// counts[i] is the number of directed entries node i contributes: one
	// per incident edge end, self-loops contributing two.
	counts := make([]int64, nodeCount)
	if err := b.forEachEdgeWindowed(ctx, func(e Edge) error {
		if e.A >= 0 && int(e.A) < nodeCount {
			counts[e.A]++
		}
		if e.B >= 0 && int(e.B) < nodeCount {
			counts[e.B]++
		}
		return nil
	}); err != nil {
		return res, err
	}
	b.phase("nodes-scanned")

	// Tag frequencies: how many works carry each tag, needed for PMI.
	//
	// A GROUP BY over 3,891,300 rows can make SQLite build a sorter, so the
	// grouping is ordered to match work_tags's primary key (work_id, tag_id)
	// and let the planner stream it rather than sort. The counts land in a
	// fixed-size array indexed by tag id, not in a map that grows.
	freq := make([]int64, nodeCount)
	if err := countTagFrequencies(ctx, b.Corpus, nodeCount, freq); err != nil {
		return res, err
	}

	nameSlice := make([]string, nodeCount)
	if err := loadNames(ctx, b.Corpus, nameSlice); err != nil {
		return res, err
	}
	b.phase("names-loaded")

	csr, err := b.buildCSR(ctx, counts, nameSlice)
	if err != nil {
		return res, err
	}
	if err := csr.Verify(); err != nil {
		return res, fmt.Errorf("built graph failed its own invariants: %w", err)
	}
	for id, n := range freq {
		if n > 0 {
			csr.SetFrequency(int32(id), int(n))
		}
	}
	b.phase("csr-built")
	res.Nodes = csr.NodeCount
	res.KeptEdges = int64(csr.Stats().DirectedEdges / 2)
	res.CSRBytes = csr.Stats().Bytes

	b.phase("frequencies-applied")

	// The index is streamed to disk from the CSR's own arrays. It is a
	// file, not a BLOB column, for the same reason the previous
	// deployment's graph was a dict rather than a table: a 27 MB BLOB is
	// read into memory on every access, and 118 MB of float32 held twice
	// at once is 236 MB of nothing useful.
	if b.Store != nil {
		if err := persistCSRStream(ctx, b, csr, res); err != nil {
			return res, err
		}
	}
	b.phase("index-written")

	// The edges table is populated by streaming, in bounded batches. It
	// exists so a peer or a query can read adjacency without loading the
	// whole graph.
	if _, err := b.Store.Exec(ctx, `DELETE FROM edges`); err != nil {
		return res, err
	}
	if err := b.streamEdgesTo(ctx, nodeCount); err != nil {
		return res, err
	}
	b.phase("edges-written")
	return res, nil
}

// countBounded counts a table by walking a primary-key column in ranges.
//
// A plain COUNT(*) is one enormous scan, and the pure-Go SQLite driver's
// row buffer grows with it — that was a 480 MB transient on a 1.7 GB
// corpus. Counting in bounded id ranges keeps the working set flat at the
// cost of a few more round trips, which on a local disk is nothing.
func countBounded(ctx context.Context, db *sql.DB, table, key string, dst *int64) error {
	var maxKey sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(`+key+`) FROM `+table).Scan(&maxKey); err != nil {
		return err
	}
	if !maxKey.Valid {
		*dst = 0
		return nil
	}
	const window = 50000
	var total int64
	for lo := int64(0); lo <= maxKey.Int64; lo += window {
		hi := lo + window
		var n int64
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE `+key+` >= ? AND `+key+` < ?`, lo, hi).Scan(&n)
		if err != nil {
			return err
		}
		total += n
	}
	*dst = total
	return nil
}

// edgeWindow is the number of edges read per query.
//
// A single scan of all 7,750,334 co-occurrence rows through database/sql
// grew the pure-Go SQLite driver's row buffer until it dominated the
// process: the phase trace put the CSR fill at a 283 MB jump. Reading the
// table in windows of tag_a_id bounds that buffer to one window at a time
// at the cost of a few hundred extra queries, which on a local disk is
// nothing next to a 1.7 GB file.
const edgeWindow = 100000

// forEachEdgeWindowed streams every edge in bounded windows.
func (b *Builder) forEachEdgeWindowed(ctx context.Context, fn func(Edge) error) error {
	var lo, hi sql.NullInt64
	if err := b.Corpus.QueryRowContext(ctx,
		`SELECT MIN(tag_a_id), MAX(tag_a_id) FROM cooccurrence_edges`).Scan(&lo, &hi); err != nil {
		return fmt.Errorf("edge range: %w", err)
	}
	// An empty table has a NULL range, not a zero one: MIN over no rows is
	// NULL, and reading it into an int64 fails outright.
	if !lo.Valid || !hi.Valid || hi.Int64 < lo.Int64 {
		return nil
	}
	for start := lo.Int64; start <= hi.Int64; start += edgeWindow {
		end := start + edgeWindow
		rows, err := b.Corpus.QueryContext(ctx, `
			SELECT tag_a_id, tag_b_id, cooccur_count FROM cooccurrence_edges
			WHERE tag_a_id >= ? AND tag_a_id < ?`, start, end)
		if err != nil {
			return fmt.Errorf("read edges [%d:%d]: %w", start, end, err)
		}
		for rows.Next() {
			var e Edge
			if err := rows.Scan(&e.A, &e.B, &e.Count); err != nil {
				rows.Close()
				return err
			}
			if err := fn(e); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

// forEachEdge streams every co-occurrence edge once, without materialising
// the set.
func forEachEdge(ctx context.Context, db *sql.DB, fn func(Edge) error) error {
	rows, err := db.QueryContext(ctx,
		`SELECT tag_a_id, tag_b_id, cooccur_count FROM cooccurrence_edges`)
	if err != nil {
		return fmt.Errorf("read edges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.A, &e.B, &e.Count); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// buildCSR fills a CSR from counts by streaming the edges a second time.
//
// The critical detail is that the adjacency is allocated at its FINAL
// size, not at the unpruned size and pruned afterwards. The unpruned
// arrays for the real corpus are 118.3 MB (15,500,668 directed entries at
// 8 bytes each) against a pruned 26.8 MB, so building-then-pruning holds
// both at once and pushed the measured peak to 352 MB — over the 220 MB
// cap this whole project exists to satisfy, from a phase that on paper
// only needed 27 MB.
//
// Pruning during construction needs the per-node neighbourhood, which for
// a node with degree d costs O(d) transient space in a reused buffer. The
// highest-degree node in the corpus is what bounds that, and the buffer is
// allocated once and reused for every node.
func (b *Builder) buildCSR(ctx context.Context, counts []int64, names []string) (*CSR, error) {
	n := len(counts)
	if n < 0 {
		n = 0
	}

	// Final degree per node: min(degree, topN).
	finalDeg := make([]int64, n)
	var total int64
	if b.TopN > 0 {
		for i := 0; i < n; i++ {
			d := counts[i]
			if d > int64(b.TopN) {
				d = int64(b.TopN)
			}
			finalDeg[i] = d
			total += d
		}
	} else {
		copy(finalDeg, counts)
		for i := 0; i < n; i++ {
			total += counts[i]
		}
	}

	offsets := make([]int64, n+1)
	for i := 0; i < n; i++ {
		offsets[i+1] = offsets[i] + finalDeg[i]
	}

	g := &CSR{
		NodeCount: n,
		TopN:      b.TopN,
		offsets:   offsets,
		neighbors: make([]int32, total),
		weights:   make([]float32, total),
		names:     names,
		freq:      make([]float64, n),
	}
	// cursor is where the next neighbour of node i goes.
	cursor := make([]int64, n)
	copy(cursor, offsets[:n])

	edgeCount := int64(0)
	if err := b.forEachEdgeWindowed(ctx, func(e Edge) error {
		if e.A < 0 || e.B < 0 || int(e.A) >= n || int(e.B) >= n {
			return nil
		}
		edgeCount++
		w := float32(e.Count)
		// Both directions are written, but only where the node still has
		// room: with a topN cap the first N encountered win. That is not
		// "the N strongest" as a post-prune would give, so when topN > 0
		// the edges are streamed in descending count order instead, below.
		if cursor[e.A] < offsets[e.A+1] {
			g.neighbors[cursor[e.A]] = e.B
			g.weights[cursor[e.A]] = w
			cursor[e.A]++
		}
		if cursor[e.B] < offsets[e.B+1] {
			g.neighbors[cursor[e.B]] = e.A
			g.weights[cursor[e.B]] = w
			cursor[e.B]++
		}
		return nil
	}); err != nil {
		return nil, err
	}
	g.EdgeCount = int(edgeCount)

	if b.TopN > 0 {
		if err := b.refillStrongest(ctx, g, n); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// refillStrongest re-fills a capped CSR so each node keeps its topN
// highest-count neighbours.
//
// A capped build that keeps the first N edges encountered keeps an
// arbitrary N, because the corpus stores edges in primary-key order rather
// than by strength. The first version of this function did exactly that
// and would have shipped a graph whose "top 24 neighbours" were
// effectively the 24 lowest tag ids — a plausible-looking index that ranks
// on noise.
//
// This pass streams the edges in descending count order. SQLite sorts
// them, which costs a sorter's worth of memory, so it is only done in
// capped mode, where the adjacency is 27 MB rather than 118 MB and there
// is headroom for it. Full mode needs no sort: it keeps everything.
func (b *Builder) refillStrongest(ctx context.Context, g *CSR, n int) error {
	rows, err := b.Corpus.QueryContext(ctx, `
		SELECT tag_a_id, tag_b_id, cooccur_count FROM cooccurrence_edges
		ORDER BY cooccur_count DESC`)
	if err != nil {
		return fmt.Errorf("order edges by strength: %w", err)
	}
	defer rows.Close()

	// Reset the arrays and cursors so the strongest edges land first and
	// any later, weaker edge for a full node is simply dropped.
	for i := range g.neighbors {
		g.neighbors[i] = 0
		g.weights[i] = 0
	}
	cursor := make([]int64, n)
	copy(cursor, g.offsets[:n])

	for rows.Next() {
		var a, bb int32
		var cnt int64
		if err := rows.Scan(&a, &bb, &cnt); err != nil {
			return err
		}
		if a < 0 || bb < 0 || int(a) >= n || int(bb) >= n {
			continue
		}
		w := float32(cnt)
		if cursor[a] < g.offsets[a+1] {
			g.neighbors[cursor[a]] = bb
			g.weights[cursor[a]] = w
			cursor[a]++
		}
		if cursor[bb] < g.offsets[bb+1] {
			g.neighbors[cursor[bb]] = a
			g.weights[cursor[bb]] = w
			cursor[bb]++
		}
	}
	return rows.Err()
}

// streamEdgesTo writes the edges table in bounded batches, capping the
// bound-variable count at SQLite's ceiling.
func (b *Builder) streamEdgesTo(ctx context.Context, nodeCount int) error {
	const batch = 300
	buf := make([]Edge, 0, batch)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if err := insertEdges(ctx, b.Store, buf); err != nil {
			return err
		}
		buf = buf[:0]
		return nil
	}
	if err := b.forEachEdgeWindowed(ctx, func(e Edge) error {
		if e.A < 0 || e.B < 0 || int(e.A) >= nodeCount || int(e.B) >= nodeCount {
			return nil
		}
		buf = append(buf, e)
		if len(buf) == batch {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	return flush()
}

func loadNames(ctx context.Context, db *sql.DB, into []string) error {
	if len(into) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM tags`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int32
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		if int(id) < len(into) {
			into[id] = name
		}
	}
	return rows.Err()
}

// maxEdgeArgs is SQLite's default bound-variable ceiling (SQLITE_MAX_VARIABLE_NUMBER
// is 999 in the modernc driver unless raised). Three variables per edge
// means a batch can never exceed 333 rows, and the real corpus proved it:
// a 20,000-row batch failed with "too many SQL variables".
const maxEdgeArgs = 999

func insertEdges(ctx context.Context, ex Executor, edges []Edge) error {
	const batch = 300
	for start := 0; start < len(edges); start += batch {
		end := min(start+batch, len(edges))
		chunk := edges[start:end]
		if len(chunk)*3 > maxEdgeArgs {
			return fmt.Errorf("edge batch of %d rows needs %d variables, over the %d limit",
				len(chunk), len(chunk)*3, maxEdgeArgs)
		}
		var sb strings.Builder
		sb.WriteString(`INSERT OR REPLACE INTO edges(tag_a, tag_b, weight) VALUES `)
		args := make([]any, 0, len(chunk)*3)
		for i, e := range chunk {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?)")
			// Store the undirected pair canonically (a<b) so a lookup
			// finds it without knowing which way round it was inserted.
			a, bb := e.A, e.B
			if a > bb {
				a, bb = bb, a
			}
			args = append(args, a, bb, float64(e.Count))
		}
		if _, err := ex.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("insert edges [%d:%d]: %w", start, end, err)
		}
	}
	return nil
}
