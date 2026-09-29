package graph

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

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

// Builder reads a corpus and produces a CSR plus the tag frequencies and
// entity/tag mapping the store needs.
type Builder struct {
	Corpus *sql.DB
	Store  Executor
	TopN   int

	// DBPath is where the encoded index is written. Empty means "do not
	// persist" — used by tests that only want the measured result.
	DBPath string
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

	if err := count(ctx, b.Corpus, `SELECT COUNT(*) FROM tags`, &res.TagsIn); err != nil {
		return res, fmt.Errorf("count tags: %w", err)
	}
	if err := count(ctx, b.Corpus, `SELECT COUNT(*) FROM works`, &res.EntityRows); err != nil {
		return res, fmt.Errorf("count works: %w", err)
	}
	if err := count(ctx, b.Corpus, `SELECT COUNT(*) FROM work_tags`, &res.WorkTagRow); err != nil {
		return res, fmt.Errorf("count work_tags: %w", err)
	}

	// Tag frequencies: how many works carry each tag. Needed for PMI, and
	// cheap as a grouped count.
	freq := make(map[int32]int64, 1<<17)
	rows, err := b.Corpus.QueryContext(ctx, `SELECT tag_id, COUNT(*) FROM work_tags GROUP BY tag_id`)
	if err != nil {
		return res, fmt.Errorf("tag frequencies: %w", err)
	}
	for rows.Next() {
		var id int32
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return res, err
		}
		freq[id] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()

	// Edges. Read in one pass, keeping only endpoints that co-occur, and
	// remembering the names so reasons can name tags.
	names := map[int32]string{}
	var edges []Edge
	rows, err = b.Corpus.QueryContext(ctx,
		`SELECT tag_a_id, tag_b_id, cooccur_count FROM cooccurrence_edges`)
	if err != nil {
		return res, fmt.Errorf("read edges: %w", err)
	}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.A, &e.B, &e.Count); err != nil {
			rows.Close()
			return res, err
		}
		edges = append(edges, e)
		names[e.A] = ""
		names[e.B] = ""
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()
	res.Edges = int64(len(edges))

	nodeCount := 0
	for id := range names {
		if int(id) >= nodeCount {
			nodeCount = int(id) + 1
		}
	}
	nameSlice := make([]string, nodeCount)
	for id := range names {
		if int(id) < nodeCount {
			nameSlice[id] = "_"
		}
	}
	if err := loadNames(ctx, b.Corpus, nameSlice); err != nil {
		return res, err
	}

	csr := Build(nodeCount, edges, nameSlice, b.TopN)
	if err := csr.Verify(); err != nil {
		return res, fmt.Errorf("built graph failed its own invariants: %w", err)
	}
	for id, n := range freq {
		csr.SetFrequency(id, int(n))
	}
	res.Nodes = csr.NodeCount
	res.KeptEdges = int64(csr.Stats().DirectedEdges / 2)
	res.CSRBytes = csr.Stats().Bytes

	blob := csr.Encode()
	if _, err := b.Store.Exec(ctx, `DELETE FROM edges`); err != nil {
		return res, err
	}
	if err := insertEdges(ctx, b.Store, edges); err != nil {
		return res, err
	}
	// The blob lives in a file, not a table: 118 MB of float32 in a BLOB
	// column is read into memory on every access, which is the same
	// mistake as holding the graph in a Python dict.
	if b.Store != nil {
		if err := persistCSR(ctx, b, blob, res); err != nil {
			return res, err
		}
	}
	return res, nil
}

func count(ctx context.Context, db *sql.DB, q string, dst *int64) error {
	return db.QueryRowContext(ctx, q).Scan(dst)
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

func insertEdges(ctx context.Context, ex Executor, edges []Edge) error {
	// Batched so a 7.7M-row table does not become 7.7M transactions.
	const batch = 20000
	for start := 0; start < len(edges); start += batch {
		end := min(start+batch, len(edges))
		chunk := edges[start:end]
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
