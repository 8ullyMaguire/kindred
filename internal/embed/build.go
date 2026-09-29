package embed

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
)

// BuildResult reports what an embedding run produced.
type BuildResult struct {
	Rows       int
	Dim        int
	Iterations int
	// Entries is the total number of matrix entries the eigensolver saw,
	// which is the honest measure of the work rather than the edge count:
	// a pruned graph feeds it far fewer entries than the corpus has.
	Entries int64
	// TopN is the cap the matrix was built under, recorded because a
	// capped matrix and an uncapped one give different embeddings from the
	// same corpus, and an embedding set with no record of which it is
	// cannot be reproduced.
	TopN int
}

// Build computes the tag embeddings and returns them as a row-indexed
// lookup.
//
// The matrix is the tag co-occurrence graph, not the work-tag incidence
// matrix, for a reason worth stating: the incidence matrix is 3,891,300
// rows by 634,231 columns, and the eigensolver holds a dense k-column
// projection of it — 634,231 x 32 float32 = 81 MB, twice over for the
// input and the output. The co-occurrence graph gives the same
// "tags that behave alike" signal from a matrix that is already capped at
// the graph's top-N adjacency.
//
// The edges are streamed into the matrix in windows rather than collected
// into a slice first. A slice of 7,750,334 edges at 16 bytes is 124 MB on
// its own, and this function's whole purpose is to stay inside a budget
// that the ingest peak is already close to.
func Build(ctx context.Context, db *sql.DB, nodeCount, dim, iters, topN int) ([][]float32, BuildResult, error) {
	var res BuildResult
	if nodeCount <= 0 || dim <= 0 {
		return nil, res, fmt.Errorf("embed: need a positive node count and dimension")
	}
	if iters <= 0 {
		iters = defaultIters
	}

	m, err := streamMatrix(ctx, db, nodeCount, topN)
	if err != nil {
		return nil, res, err
	}
	res.Entries = m.entries()

	out, err := Embed(m, dim, iters)
	if err != nil {
		return nil, res, err
	}
	res.Rows = nodeCount
	res.Dim = out.Dim
	res.Iterations = out.Iterations
	res.TopN = topN
	return out.Components, res, nil
}

// streamMatrix builds the co-occurrence matrix, capped at topN neighbours
// per row.
//
// The cap is the memory decision, and it is the same decision the graph
// made for the same reason. Uncapped, the matrix holds every one of the
// 7,750,334 corpus edges in both directions: 15,500,668 entries, 124 MB
// for the CSR and 124 MB for the transpose, and the eigensolver peaked at
// 440 MB against a 220 MB cap. Capped at top-24 the same signal is
// 2,884,447 entries — 23 MB each way — because most tags have a degree
// below the cap and only the hubs are trimmed.
//
// Neither pass holds an edge list. Pass one counts each row's final degree
// and pass two fills, so the largest thing allocated is the CSR itself.
// A slice of the admitted edges would be 23 MB and a slice of all of them
// 62 MB, and the whole exercise is to avoid exactly that.
//
// The fill pass runs in descending co-occurrence order, so the first topN
// edges admitted for a node are its strongest. Ordering by tag_a_id
// instead — cheaper, and what the counting pass reads — would admit the
// lowest tag ids, producing an embedding matrix that looks fine and ranks
// on noise.
func streamMatrix(ctx context.Context, db *sql.DB, nodeCount, topN int) (*Matrix, error) {
	if topN <= 0 {
		topN = 24
	}
	m := &Matrix{Rows: nodeCount, Cols: nodeCount}
	m.offsets = make([]int64, nodeCount+1)

	var lo, hi sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MIN(tag_a_id), MAX(tag_a_id) FROM cooccurrence_edges`).Scan(&lo, &hi); err != nil {
		return nil, fmt.Errorf("embed: edge range: %w", err)
	}
	if !lo.Valid || !hi.Valid || hi.Int64 < lo.Int64 {
		return nil, fmt.Errorf("embed: the corpus has no co-occurrence edges to embed")
	}

	// Pass one: degree per node, after capping, streamed in windows so the
	// driver's row buffer stays small.
	const window = 100000
	degree := make([]int32, nodeCount)
	for start := lo.Int64; start <= hi.Int64; start += window {
		end := start + window
		rows, err := db.QueryContext(ctx,
			`SELECT tag_a_id, tag_b_id FROM cooccurrence_edges
			 WHERE tag_a_id >= ? AND tag_a_id < ?`, start, end)
		if err != nil {
			return nil, fmt.Errorf("embed: read edges: %w", err)
		}
		for rows.Next() {
			var a, b int32
			if err := rows.Scan(&a, &b); err != nil {
				rows.Close()
				return nil, err
			}
			if a < 0 || b < 0 || int(a) >= nodeCount || int(b) >= nodeCount {
				continue
			}
			if int(degree[a]) < topN {
				degree[a]++
			}
			if a != b && int(degree[b]) < topN {
				degree[b]++
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	for i := 0; i < nodeCount; i++ {
		m.offsets[i+1] = m.offsets[i] + int64(degree[i])
	}
	n := m.offsets[nodeCount]
	if n == 0 {
		return nil, fmt.Errorf("embed: every co-occurrence row fell outside the %d-node space", nodeCount)
	}
	m.cols = make([]int32, n)
	m.vals = make([]float32, n)
	cursor := make([]int64, nodeCount)
	copy(cursor, m.offsets[:nodeCount])
	taken := make([]int32, nodeCount)

	// Pass two: fill, strongest first, admitting only while the node has
	// room. SQLite's sort is what buys the global strength order; it runs
	// here rather than in the graph build because this matrix is
	// recomputed on every embedding run.
	rows, err := db.QueryContext(ctx,
		`SELECT tag_a_id, tag_b_id FROM cooccurrence_edges
		 ORDER BY cooccur_count DESC`)
	if err != nil {
		return nil, fmt.Errorf("embed: order edges by strength: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b int32
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		if a < 0 || b < 0 || int(a) >= nodeCount || int(b) >= nodeCount {
			continue
		}
		if int(taken[a]) < topN {
			taken[a]++
			m.cols[cursor[a]] = b
			m.vals[cursor[a]] = 1
			cursor[a]++
		}
		if a != b && int(taken[b]) < topN {
			taken[b]++
			m.cols[cursor[b]] = a
			m.vals[cursor[b]] = 1
			cursor[b]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The transpose is built here rather than lazily inside the multiply:
	// it costs one pass and 8 bytes per entry, and building it under the
	// eigensolver's peak would put 23 MB on top of a tight budget.
	m.buildTransposed()
	return m, nil
}

// entries is the number of stored matrix entries.
func (m *Matrix) entries() int64 {
	if m == nil || len(m.offsets) == 0 {
		return 0
	}
	return m.offsets[m.Rows]
}

// Pack encodes a vector as little-endian float32 bytes for the store.
//
// The byte order is pinned here rather than assumed at read time. A
// mirrored pair of codec halves agrees perfectly with itself while reading
// the wrong bytes, so a round-trip test cannot catch a byte-order mistake
// — only golden bytes taken from the format can.
func Pack(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// Unpack decodes little-endian float32 bytes, refusing a short or empty
// buffer rather than reading past the end.
func Unpack(dim int, b []byte) ([]float32, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("embed: dim %d is not positive", dim)
	}
	if len(b) != dim*4 {
		return nil, fmt.Errorf("embed: got %d bytes for a %d-dim vector, want %d", len(b), dim, dim*4)
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}
