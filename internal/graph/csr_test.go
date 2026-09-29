package graph

import (
	"math"
	"testing"
)

func build(t *testing.T, n int, edges []Edge, topN int) *CSR {
	t.Helper()
	g := Build(n, edges, nil, topN)
	if err := g.Verify(); err != nil {
		t.Fatalf("Verify after Build: %v", err)
	}
	return g
}

func TestDegreeSumEqualsTwiceEdgeCount(t *testing.T) {
	// The invariant that catches a silently truncated graph. A test that
	// only checks len(CSR) would pass on a graph that lost half its
	// edges, and every ranking built on it would still look plausible.
	edges := []Edge{{0, 1, 5}, {1, 2, 3}, {2, 0, 7}, {3, 3, 1}}
	g := build(t, 4, edges, 0)
	var sum int
	for i := 0; i < g.NodeCount; i++ {
		sum += g.Degree(int32(i))
	}
	if sum != 2*len(edges) {
		t.Fatalf("degrees sum to %d, want %d (2 x %d edges)", sum, 2*len(edges), len(edges))
	}
	if g.EdgeCount != 4 {
		t.Fatalf("EdgeCount = %d, want 4", g.EdgeCount)
	}
}

func TestRoundTripPreservesAdjacency(t *testing.T) {
	edges := []Edge{{0, 1, 5}, {1, 2, 3}, {2, 0, 7}}
	g := build(t, 3, edges, 0)

	for i := 0; i < g.NodeCount; i++ {
		id := int32(i)
		ids, ws := g.Neighbours(id)
		if len(ids) != len(ws) {
			t.Fatalf("node %d: %d neighbours but %d weights", i, len(ids), len(ws))
		}
		seen := map[int32]float32{}
		for k, n := range ids {
			seen[n] = ws[k]
		}
		for _, e := range edges {
			if e.A == id {
				if seen[e.B] != float32(e.Count) {
					t.Fatalf("node %d missing edge to %d (got %v want %d)", i, e.B, seen[e.B], e.Count)
				}
			}
			if e.B == id {
				if seen[e.A] != float32(e.Count) {
					t.Fatalf("node %d missing edge to %d", i, e.A)
				}
			}
		}
	}
}

func TestEmptyGraph(t *testing.T) {
	g := build(t, 0, nil, 0)
	if g.NodeCount != 0 || g.Degree(0) != 0 {
		t.Fatalf("empty graph reports nodes=%d degree=%d", g.NodeCount, g.Degree(0))
	}
	if err := g.Verify(); err != nil {
		t.Fatalf("Verify on empty: %v", err)
	}
	// Out-of-range access must be safe, not a panic: a request for a
	// tag id the graph does not have is a normal miss, not a crash.
	if g.Degree(-1) != 0 || g.Degree(9999) != 0 {
		t.Fatal("out-of-range degree did not return 0")
	}
	if ids, _ := g.Neighbours(9999); ids != nil {
		t.Fatal("out-of-range Neighbours returned data")
	}
}

func TestSingleNode(t *testing.T) {
	g := build(t, 1, nil, 0)
	if g.Degree(0) != 0 {
		t.Fatalf("single node degree = %d", g.Degree(0))
	}
}

func TestSelfLoop(t *testing.T) {
	// The corpus contains self-loops (a tag co-occurring with itself).
	// They must not corrupt the degree arithmetic.
	g := build(t, 2, []Edge{{0, 0, 4}, {0, 1, 2}}, 0)
	if g.Degree(0) != 3 {
		t.Fatalf("node 0 degree = %d, want 3 (two self-loop entries plus one edge)", g.Degree(0))
	}
}

func TestOutOfRangeEdgesAreSkipped(t *testing.T) {
	// A tag id past the node count must be dropped, not panic and not
	// silently corrupt the offsets.
	g := build(t, 2, []Edge{{0, 1, 3}, {0, 99, 7}, {-1, 0, 2}}, 0)
	if got := g.Degree(0); got != 1 {
		t.Fatalf("node 0 degree = %d, want 1 (only the in-range edge)", got)
	}
}

func TestPruneTopNKeepsStrongest(t *testing.T) {
	// Node 0 links to 1,2,3,4 with descending counts. Top 2 must keep 1
	// and 2 and drop 3 and 4.
	edges := []Edge{{0, 1, 90}, {0, 2, 80}, {0, 3, 70}, {0, 4, 60}}
	g := build(t, 5, edges, 2)
	ids, _ := g.Neighbours(0)
	if len(ids) != 2 {
		t.Fatalf("node 0 kept %d neighbours, want 2: %v", len(ids), ids)
	}
	got := map[int32]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[1] || !got[2] {
		t.Fatalf("node 0 kept %v, want the top two by count (1 and 2)", ids)
	}
	if got[3] || got[4] {
		t.Fatalf("node 0 kept the weaker neighbours %v", ids)
	}
}

func TestPruneTopNReclaimsMemory(t *testing.T) {
	// Pruning must actually shrink the arrays, not just stop reading
	// them: a lite-mode graph holding 3 GB of addressed-but-unused
	// neighbours defeats the entire point of the mode.
	//
	// Note the per-node semantics: topN bounds each node's degree, not
	// the edge count. Node 0 fans out to 50 neighbours and is cut to 5;
	// each of the 50 leaves keeps its single edge to node 0, so the
	// graph goes from 100 directed entries to 55, not to 10.
	edges := make([]Edge, 0, 50)
	for i := 1; i <= 50; i++ {
		edges = append(edges, Edge{0, int32(i), int64(100 - i)})
	}
	full := Build(1+50, edges, nil, 0)
	lit := build(t, 1+50, edges, 5)
	if len(lit.neighbors) >= len(full.neighbors) {
		t.Fatalf("pruned graph kept %d entries of %d; it did not reclaim",
			len(lit.neighbors), len(full.neighbors))
	}
	if got := len(lit.neighbors); got != 55 {
		t.Fatalf("pruned directed entries = %d, want 55 (5 from the hub plus 50 leaves)", got)
	}
	if d := lit.Degree(0); d != 5 {
		t.Fatalf("hub degree = %d, want 5", d)
	}
	// Every leaf still points at the hub: pruning must not orphan a node.
	leaves := 0
	for i := 1; i <= 50; i++ {
		if lit.Degree(int32(i)) != 1 {
			t.Fatalf("leaf %d has degree %d, want 1", i, lit.Degree(int32(i)))
		}
		leaves++
	}
	if leaves != 50 {
		t.Fatalf("checked %d leaves, want 50", leaves)
	}
}

func TestPruneTopNIsDeterministic(t *testing.T) {
	// Equal counts must break ties by id, or two builds of the same
	// corpus produce different rankings for reasons no one can explain.
	edges := []Edge{{0, 5, 10}, {0, 3, 10}, {0, 9, 10}, {0, 1, 10}}
	a := build(t, 10, edges, 2)
	b := build(t, 10, edges, 2)
	ai, _ := a.Neighbours(0)
	bi, _ := b.Neighbours(0)
	if len(ai) != len(bi) {
		t.Fatalf("lengths differ: %d vs %d", len(ai), len(bi))
	}
	for i := range ai {
		if ai[i] != bi[i] {
			t.Fatalf("build %d is not deterministic: %v vs %v", i, ai, bi)
		}
	}
}

func TestVerifyCatchesCorruption(t *testing.T) {
	// Verify is the guard, so it must actually fail on bad state. If it
	// cannot fail, it is decoration.
	g := Build(3, []Edge{{0, 1, 1}, {1, 2, 1}}, nil, 0)
	if err := g.Verify(); err != nil {
		t.Fatalf("healthy graph failed Verify: %v", err)
	}
	g.offsets = g.offsets[:2] // corrupt the offsets length
	if err := g.Verify(); err == nil {
		t.Fatal("Verify accepted a graph with truncated offsets")
	}
}

func TestVerifyCatchesBadNeighbourId(t *testing.T) {
	g := Build(2, []Edge{{0, 1, 1}}, nil, 0)
	g.neighbors[0] = 42 // out of range for a 2-node graph
	if err := g.Verify(); err == nil {
		t.Fatal("Verify accepted an out-of-range neighbour id")
	}
}

func TestPMIRanksInformativePairsHigher(t *testing.T) {
	// A pair seen in a small slice of a large corpus is more informative
	// than one seen in a third of it. That is the whole reason PMI is
	// used rather than the raw count.
	total := 10000.0
	//  a,b each on 100 works, co-occurrence 50 -> tight
	//  c,d each on 5000 works, co-occurrence 2500 -> broad
	tight := PMI(100, 100, 50, total)
	broad := PMI(5000, 5000, 2500, total)
	if tight <= broad {
		t.Fatalf("tight PMI %f <= broad PMI %f; PMI is not discounting the common case", tight, broad)
	}
}

func TestPMIHandlesDegenerateInput(t *testing.T) {
	for _, tc := range []struct {
		name            string
		a, b, co, total float64
	}{
		{"zero co", 10, 10, 0, 100},
		{"zero freq", 0, 10, 5, 100},
		{"zero total", 10, 10, 5, 0},
		{"impossible", 10, 10, 1000, 100},
	} {
		got := PMI(tc.a, tc.b, tc.co, tc.total)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("%s: PMI returned %v", tc.name, got)
		}
	}
}

func TestNeighbourScoreRanksAndBounds(t *testing.T) {
	g := Build(4, []Edge{{0, 1, 10}, {0, 2, 8}, {0, 3, 6}, {1, 2, 99}}, nil, 0)
	g.SetFrequency(0, 100)
	g.SetFrequency(1, 20)
	g.SetFrequency(2, 500)
	g.SetFrequency(3, 4000)

	got := g.NeighbourScore(0, 0, 10000)
	// Node 3 is expected to be filtered out: with freq 4000 of 10000 and
	// a co-occurrence of 6, its PMI is below 1 and NeighbourScore drops
	// non-positive PMI rather than reporting a useless dimension. Two
	// surviving neighbours is the correct answer, not a bug.
	if len(got) != 2 {
		t.Fatalf("got %d scored neighbours, want 2: %+v", len(got), got)
	}
	for _, n := range got {
		if n.PMI <= 0 {
			t.Fatalf("non-positive PMI survived: %+v", n)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].PMI < got[i].PMI {
			t.Fatalf("results are not strongest-first: %+v", got)
		}
	}
	// A limit must actually bound, and the unbounded call must not be
	// what a caller gets by accident.
	limited := g.NeighbourScore(0, 1, 10000)
	if len(limited) != 1 {
		t.Fatalf("limit=1 returned %d", len(limited))
	}
	if limited[0].TagID != got[0].TagID {
		t.Fatalf("limit=1 returned %d, want the top one %d", limited[0].TagID, got[0].TagID)
	}
}

func TestNeighbourScoreOnIsolatedNode(t *testing.T) {
	g := Build(3, []Edge{{0, 1, 1}}, nil, 0)
	if got := g.NeighbourScore(2, 10, 100); got != nil {
		t.Fatalf("isolated node returned %v", got)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	// A round trip proves the two halves agree with each other. That is
	// necessary and not sufficient: it says nothing about whether the
	// bytes are the format's bytes. The golden-bytes check below is the
	// one that proves correctness.
	edges := []Edge{{0, 1, 5}, {1, 2, 3}, {2, 0, 7}}
	g := build(t, 3, edges, 0)
	blob := g.Encode()

	back, err := Decode(g.NodeCount, g.EdgeCount, g.TopN, blob)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if err := back.Verify(); err != nil {
		t.Fatalf("decoded graph failed Verify: %v", err)
	}
	for i := 0; i < g.NodeCount; i++ {
		gi, gw := g.Neighbours(int32(i))
		bi, bw := back.Neighbours(int32(i))
		if len(gi) != len(bi) {
			t.Fatalf("node %d: %d vs %d neighbours", i, len(gi), len(bi))
		}
		for k := range gi {
			if gi[k] != bi[k] || gw[k] != bw[k] {
				t.Fatalf("node %d entry %d: %d/%v vs %d/%v", i, k, gi[k], gw[k], bi[k], bw[k])
			}
		}
	}
}

// TestEncodeMatchesGoldenBytes pins the wire format against bytes derived
// from the format's definition, not from our own encoder. A round trip
// test would pass just as happily if both halves were byte-swapped.
func TestEncodeMatchesGoldenBytes(t *testing.T) {
	g := Build(3, []Edge{{0, 1, 5}}, nil, 0)
	// Node 0 has 1 neighbour, node 1 has 1, node 2 has 0.
	// offsets = [0,1,2,2] as little-endian uint64.
	want := []byte{
		0, 0, 0, 0, 0, 0, 0, 0, // offsets[0] = 0
		1, 0, 0, 0, 0, 0, 0, 0, // offsets[1] = 1
		2, 0, 0, 0, 0, 0, 0, 0, // offsets[2] = 2
		2, 0, 0, 0, 0, 0, 0, 0, // offsets[3] = 2
		1, 0, 0, 0, // neighbours[0] = node 1
		0, 0, 0, 0, // neighbours[1] = node 0
		0, 0, 160, 64, // weights[0] = float32(5)  = 0x40A00000
		0, 0, 160, 64, // weights[1] = float32(5)
	}
	got := g.Encode()
	if len(got) != len(want) {
		t.Fatalf("encoded %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d = %d, want %d\n got: %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}

func TestDecodeRejectsTruncatedBlob(t *testing.T) {
	g := Build(3, []Edge{{0, 1, 5}, {1, 2, 3}}, nil, 0)
	blob := g.Encode()
	// Cut the blob in half: Decode must refuse rather than read past the
	// end and hand back a graph that looks valid.
	if _, err := Decode(g.NodeCount, g.EdgeCount, g.TopN, blob[:len(blob)/2]); err == nil {
		t.Fatal("Decode accepted a truncated blob")
	}
	if _, err := Decode(g.NodeCount, g.EdgeCount, g.TopN, nil); err == nil {
		t.Fatal("Decode accepted an empty blob")
	}
}

func TestStatsBytesIsNonZeroForARealGraph(t *testing.T) {
	edges := make([]Edge, 0, 1000)
	for i := range 1000 {
		edges = append(edges, Edge{A: int32(i % 999), B: int32((i + 1) % 1000), Count: 1})
	}
	g := Build(1000, edges, nil, 0)
	if st := g.Stats(); st.Bytes <= 0 || st.NodeCount != 1000 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestFrequencyAccessors(t *testing.T) {
	g := Build(3, nil, nil, 0)
	g.SetFrequency(1, 500)
	if got := g.Frequency(1); got != 500 {
		t.Fatalf("Frequency(1) = %v, want 500", got)
	}
	if got := g.Frequency(99); got != 0 {
		t.Fatalf("Frequency out of range = %v, want 0", got)
	}
	g.SetFrequency(99, 10) // must not panic
}

func TestNames(t *testing.T) {
	names := []string{"alpha", "beta", "gamma"}
	g := Build(3, []Edge{{0, 1, 1}}, names, 0)
	if got := g.Name(0); got != "alpha" {
		t.Fatalf("Name(0) = %q", got)
	}
	unnamed := Build(2, nil, nil, 0)
	if got := unnamed.Name(0); got != "" {
		t.Fatalf("Name on an unnamed graph = %q, want empty", got)
	}
}
