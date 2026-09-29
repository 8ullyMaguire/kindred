// Package graph holds the tag co-occurrence index.
//
// The whole memory budget rests on one representation choice (SPEC §0.1):
// the previous Python deployment held this graph as a dict-of-dicts at
// 3.0 GB resident for 7,750,334 edges, because a dict entry costs roughly
// 100 bytes of overhead against 8 bytes of payload. The same graph as
// three arrays is 124 MB. That is a 24x cut from representation alone,
// with no algorithm changed.
package graph

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// CSR is a compressed-sparse-row adjacency. Node i's neighbours live at
// indices[offsets[i]:offsets[i+1]], each with a parallel weight.
//
// Undirected input is symmetrised on build: every edge (a,b) also produces
// (b,a), so degrees sum to 2*EdgeCount.
type CSR struct {
	NodeCount int
	EdgeCount int // undirected edges
	TopN      int

	offsets   []int64
	neighbors []int32
	weights   []float32
	names     []string // node id -> tag name, for reasons and dumps
	nameOf    map[int32]string
	freq      []float64 // node id -> work count, for PMI
}

// Edge is one undirected co-occurrence.
type Edge struct {
	A, B  int32
	Count int64
}

// Build constructs a CSR from undirected edges, keeping only the TopN
// highest-count neighbours per node.
//
// topN <= 0 keeps every neighbour (full mode); a positive topN is the
// lite mode, where the adjacency is bounded by construction rather than
// by hope.
func Build(nodeCount int, edges []Edge, names []string, topN int) *CSR {
	if nodeCount < 0 {
		nodeCount = 0
	}
	degree := make([]int64, nodeCount+1)
	// Two passes: count degrees, then fill. No maps, no sort of a slice
	// of structs, no per-node allocation.
	for _, e := range edges {
		if !valid(nodeCount, e.A, e.B) {
			continue
		}
		degree[e.A+1]++
		degree[e.B+1]++
	}
	for i := 1; i <= nodeCount; i++ {
		degree[i] += degree[i-1]
	}
	total := degree[nodeCount]

	g := &CSR{
		NodeCount: nodeCount,
		EdgeCount: len(edges),
		TopN:      topN,
		offsets:   degree,
		neighbors: make([]int32, total),
		weights:   make([]float32, total),
		names:     names,
		freq:      make([]float64, nodeCount),
	}
	if names != nil {
		g.nameOf = make(map[int32]string, len(names))
		for i, n := range names {
			g.nameOf[int32(i)] = n
		}
	}
	cursor := make([]int64, nodeCount)
	copy(cursor, degree[:nodeCount])
	for _, e := range edges {
		if !valid(nodeCount, e.A, e.B) {
			continue
		}
		w := float32(e.Count)
		g.neighbors[cursor[e.A]] = e.B
		g.weights[cursor[e.A]] = w
		cursor[e.A]++
		g.neighbors[cursor[e.B]] = e.A
		g.weights[cursor[e.B]] = w
		cursor[e.B]++
	}
	if topN > 0 {
		g.pruneTopN(topN)
	}
	return g
}

func valid(n int, a, b int32) bool { return a >= 0 && b >= 0 && int(a) < n && int(b) < n }

// pruneTopN keeps only the topN strongest neighbours per node, in place,
// rebuilding the arrays to reclaim the memory the dropped edges held. A
// lite-mode graph that kept 3.0 GB of addressed-but-unused neighbours
// would defeat the entire point of the mode.
func (g *CSR) pruneTopN(topN int) {
	type nb struct {
		id int32
		w  float32
	}
	// Reused across nodes: allocating per node is 123k allocations.
	buf := make([]nb, 0, 256)

	newOffsets := make([]int64, g.NodeCount+1)
	var total int64
	for i := 0; i < g.NodeCount; i++ {
		lo, hi := g.offsets[i], g.offsets[i+1]
		d := int(hi - lo)
		if d > topN {
			buf = buf[:0]
			for k := lo; k < hi; k++ {
				buf = append(buf, nb{id: g.neighbors[k], w: g.weights[k]})
			}
			sort.Slice(buf, func(x, y int) bool {
				if buf[x].w != buf[y].w {
					return buf[x].w > buf[y].w
				}
				return buf[x].id < buf[y].id // stable, so the graph is reproducible
			})
			buf = buf[:topN]
			lo, hi = 0, int64(len(buf))
		}
		// Compact into the tail of the old arrays; the new offsets are
		// computed in a second pass, so use a running cursor.
		start := total
		for k := lo; k < hi; k++ {
			g.neighbors[total] = g.neighbors[k]
			g.weights[total] = g.weights[k]
			total++
		}
		newOffsets[i+1] = total
		_ = start
	}
	g.offsets = newOffsets
	g.neighbors = g.neighbors[:total]
	g.weights = g.weights[:total]
}

// Degree returns the number of retained neighbours of a node.
func (g *CSR) Degree(id int32) int {
	if id < 0 || int(id) >= g.NodeCount {
		return 0
	}
	return int(g.offsets[id+1] - g.offsets[id])
}

// Neighbours returns a node's retained neighbours. The returned slice
// aliases the CSR's storage: read it, do not keep it.
func (g *CSR) Neighbours(id int32) (ids []int32, weights []float32) {
	if id < 0 || int(id) >= g.NodeCount {
		return nil, nil
	}
	lo, hi := g.offsets[id], g.offsets[id+1]
	return g.neighbors[lo:hi], g.weights[lo:hi]
}

// Name returns a node's tag name, or "" if unknown.
func (g *CSR) Name(id int32) string {
	if g.nameOf == nil {
		return ""
	}
	return g.nameOf[id]
}

// SetFrequency records how many works carry a tag, for PMI.
func (g *CSR) SetFrequency(id int32, n int) {
	if id >= 0 && int(id) < g.NodeCount {
		g.freq[id] = float64(n)
	}
}

// Frequency returns a tag's work count.
func (g *CSR) Frequency(id int32) float64 {
	if id < 0 || int(id) >= g.NodeCount {
		return 0
	}
	return g.freq[id]
}

// PMI is the pointwise mutual information of a co-occurrence, the
// measure the neighbourhood signal ranks by.
//
//	p(a,b) = log( P(a,b) / (P(a) * P(b)) )
//
// freqA and freqB are work counts, co is the co-occurrence count and
// totalWorks the corpus size. A tag pair seen in a small slice of a large
// corpus is more informative than one seen in a third of it, which is why
// the raw count is not the right ranking on its own.
func PMI(freqA, freqB, co, totalWorks float64) float64 {
	if co <= 0 || freqA <= 0 || freqB <= 0 || totalWorks <= 0 {
		return 0
	}
	pab := co / totalWorks
	pa := freqA / totalWorks
	pb := freqB / totalWorks
	v := math.Log(pab / (pa * pb))
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// NeighbourScore ranks a node's neighbours by PMI against a query tag and
// returns the top limit, strongest first.
//
// limit <= 0 returns everything, which is only appropriate for small
// graphs; a caller that forgot to bound it gets the whole neighbourhood
// and the memory that goes with it.
func (g *CSR) NeighbourScore(query int32, limit int, totalWorks float64) []ScoredNeighbour {
	ids, ws := g.Neighbours(query)
	if len(ids) == 0 {
		return nil
	}
	fq := g.Frequency(query)
	out := make([]ScoredNeighbour, 0, len(ids))
	for i, id := range ids {
		p := PMI(fq, g.Frequency(id), float64(ws[i]), totalWorks)
		if p <= 0 {
			continue
		}
		out = append(out, ScoredNeighbour{TagID: id, PMI: p, Count: float64(ws[i])})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PMI != out[j].PMI {
			return out[i].PMI > out[j].PMI
		}
		return out[i].TagID < out[j].TagID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ScoredNeighbour is one neighbour with its PMI.
type ScoredNeighbour struct {
	TagID int32
	PMI   float64
	Count float64
}

// Stats describes a built graph, for logs and the stats endpoint.
type Stats struct {
	NodeCount     int   `json:"node_count"`
	EdgeCount     int   `json:"edge_count"`
	DirectedEdges int   `json:"directed_edges"`
	TopN          int   `json:"top_n"`
	Bytes         int64 `json:"bytes"`
}

func (g *CSR) Stats() Stats {
	return Stats{
		NodeCount:     g.NodeCount,
		EdgeCount:     g.EdgeCount,
		DirectedEdges: len(g.neighbors),
		TopN:          g.TopN,
		Bytes:         int64(len(g.offsets))*8 + int64(len(g.neighbors))*4 + int64(len(g.weights))*4,
	}
}

// Verify checks the CSR's internal invariants: every degree is non
// negative and within bounds, offsets are monotonic, and the total
// directed length is consistent.
//
// This exists because a silently truncated graph looks perfectly valid to
// any test that only checks len(). A test that cannot fail tests nothing.
func (g *CSR) Verify() error {
	if len(g.offsets) != g.NodeCount+1 {
		return errfOf("offsets has %d entries, want node_count+1 = %d", len(g.offsets), g.NodeCount+1)
	}
	var sum int64
	for i := 0; i < g.NodeCount; i++ {
		d := g.offsets[i+1] - g.offsets[i]
		if d < 0 {
			return errfOf("node %d has negative degree %d", i, d)
		}
		sum += d
	}
	if sum != int64(len(g.neighbors)) {
		return errfOf("degrees sum to %d but there are %d neighbour entries", sum, len(g.neighbors))
	}
	if len(g.neighbors) != len(g.weights) {
		return errfOf("%d neighbours but %d weights", len(g.neighbors), len(g.weights))
	}
	for i, id := range g.neighbors {
		if id < 0 || int(id) >= g.NodeCount {
			return errfOf("neighbour entry %d is out of range: %d", i, id)
		}
	}
	return nil
}

type errf string

func (e errf) Error() string { return "graph: " + string(e) }

func errfOf(format string, args ...any) error {
	return errf(fmt.Sprintf(format, args...))
}

// Encode serialises the graph so an index build is not repeated per
// process start. Little-endian float32/int32, which is what the numbers
// were already in memory as.
func (g *CSR) Encode() []byte {
	size := 8*len(g.offsets) + 4*len(g.neighbors) + 4*len(g.weights)
	buf := make([]byte, size)
	off := 0
	for _, v := range g.offsets {
		binary.LittleEndian.PutUint64(buf[off:], uint64(v))
		off += 8
	}
	for _, v := range g.neighbors {
		binary.LittleEndian.PutUint32(buf[off:], uint32(v))
		off += 4
	}
	for _, v := range g.weights {
		binary.LittleEndian.PutUint32(buf[off:], math.Float32bits(v))
		off += 4
	}
	return buf
}

// Decode restores a graph encoded by Encode. Frequencies are not in the
// blob; they are re-read from the store, because they change with the
// corpus and stale PMI is a silently wrong answer.
func Decode(nodeCount, edgeCount, topN int, blob []byte) (*CSR, error) {
	nOff := (nodeCount + 1) * 8
	need := nOff + (nodeCount*2)*4 // worst case: 2 directed entries per node
	_ = need
	if len(blob) < nOff {
		return nil, errfOf("blob too small: %d bytes, need at least %d", len(blob), nOff)
	}
	g := &CSR{
		NodeCount: nodeCount,
		EdgeCount: edgeCount,
		TopN:      topN,
		offsets:   make([]int64, nodeCount+1),
		freq:      make([]float64, nodeCount),
	}
	off := 0
	for i := range g.offsets {
		g.offsets[i] = int64(binary.LittleEndian.Uint64(blob[off:]))
		off += 8
	}
	n := int(g.offsets[nodeCount])
	if n < 0 || off+n*8 > len(blob) {
		return nil, errfOf("blob truncated: want %d entries (%d bytes), have %d bytes",
			n, n*8, len(blob)-off)
	}
	g.neighbors = make([]int32, n)
	for i := range g.neighbors {
		g.neighbors[i] = int32(binary.LittleEndian.Uint32(blob[off:]))
		off += 4
	}
	g.weights = make([]float32, n)
	for i := range g.weights {
		g.weights[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[off:]))
		off += 4
	}
	return g, nil
}
