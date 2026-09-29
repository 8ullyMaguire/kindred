package graph

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The index file format, pinned as literal bytes.
//
// A round-trip test cannot catch a writer and reader agreeing on the
// wrong layout: the first version of the streaming writer padded every
// flush with the full 16 KiB backing array, so the file grew by 101,032
// bytes of zeros past the end of its own header. Write-then-read passed
// — it agreed with itself — while every real load failed with
// "neighbour entry 0 is out of range: 2873866", which is the last offset
// value read out of the padding.
//
// So these tests assert the exact byte length and the exact bytes at each
// array boundary, against the format as documented. Golden bytes come
// from the layout, not from this implementation's own output.

func TestPersistCSRStreamWritesExactlyTheEncodedSize(t *testing.T) {
	g := smallGraph()
	path := filepath.Join(t.TempDir(), "k.db")
	b := &Builder{Corpus: nil, DBPath: path}

	if err := persistCSRStream(context.Background(), b, g, BuildResult{
		Nodes: g.NodeCount, Edges: int64(g.EdgeCount), TopN: g.TopN,
	}); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(csrPath(path))
	if err != nil {
		t.Fatal(err)
	}
	// header + offsets(8 each) + neighbours(4 each) + weights(4 each).
	want := int64(headerSize + 8*(g.NodeCount+1) + 4*len(g.neighbors) + 4*len(g.weights))
	if st.Size() != want {
		t.Fatalf("index file is %d bytes, want exactly %d.\n"+
			"A size that exceeds the sum of its arrays is padding: the writer "+
			"flushed a whole buffer instead of the bytes it held.", st.Size(), want)
	}
}

// TestPersistCSRStreamMatchesEncodeByteForByte is the format check that
// actually pins the layout: the streaming writer and the reference
// encoder must produce identical bytes for the same graph. A mirrored
// implementation agreeing with itself is not enough, so the expected
// bytes are built here from the documented layout, independently of both.
func TestPersistCSRStreamMatchesTheDocumentedLayout(t *testing.T) {
	g := smallGraph()
	path := filepath.Join(t.TempDir(), "k.db")
	b := &Builder{DBPath: path}
	if err := persistCSRStream(context.Background(), b, g, BuildResult{
		Nodes: g.NodeCount, Edges: int64(g.EdgeCount), TopN: g.TopN,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(csrPath(path))
	if err != nil {
		t.Fatal(err)
	}

	// Build the expected file from the layout by hand.
	var want []byte
	head := make([]byte, headerSize)
	copy(head[0:4], csrMagic[:])
	binary.LittleEndian.PutUint32(head[4:8], 1)
	binary.LittleEndian.PutUint64(head[8:16], uint64(g.NodeCount))
	binary.LittleEndian.PutUint64(head[16:24], uint64(g.EdgeCount))
	binary.LittleEndian.PutUint64(head[24:32], uint64(g.TopN))
	want = append(want, head...)
	for _, v := range g.offsets {
		var s [8]byte
		binary.LittleEndian.PutUint64(s[:], uint64(v))
		want = append(want, s[:]...)
	}
	for _, v := range g.neighbors {
		var s [4]byte
		binary.LittleEndian.PutUint32(s[:], uint32(v))
		want = append(want, s[:]...)
	}
	for _, v := range g.weights {
		var s [4]byte
		binary.LittleEndian.PutUint32(s[:], math.Float32bits(v))
		want = append(want, s[:]...)
	}

	if len(raw) != len(want) {
		t.Fatalf("streamed %d bytes, layout requires %d", len(raw), len(want))
	}
	if string(raw) != string(want) {
		for i := range want {
			if raw[i] != want[i] {
				lo := i - 8
				if lo < 0 {
					lo = 0
				}
				hi := i + 8
				if hi > len(want) {
					hi = len(want)
				}
				t.Fatalf("first difference at byte %d (in the %s array):\n got %x\nwant %x",
					i, arrayNameAt(i, g), raw[lo:hi], want[lo:hi])
			}
		}
	}
}

// arrayNameAt names which array a byte offset falls in, for a readable
// failure message.
func arrayNameAt(i int, g *CSR) string {
	endOffsets := headerSize + 8*(g.NodeCount+1)
	if i < endOffsets {
		return "offsets"
	}
	if i < endOffsets+4*len(g.neighbors) {
		return "neighbours"
	}
	return "weights"
}

// TestLoadCSRReadsBackAStreamedIndex is the end-to-end check: build, write,
// load, and compare against the in-memory graph. It is here as well as a
// round-trip because the golden bytes above are what make it meaningful.
func TestLoadCSRReadsBackAStreamedIndex(t *testing.T) {
	g := smallGraph()
	path := filepath.Join(t.TempDir(), "k.db")
	b := &Builder{DBPath: path}
	if err := persistCSRStream(context.Background(), b, g, BuildResult{
		Nodes: g.NodeCount, Edges: int64(g.EdgeCount), TopN: g.TopN,
	}); err != nil {
		t.Fatal(err)
	}

	// An empty corpus handle: the loader only needs names and frequencies,
	// and passing nil readers means it restores neither.
	loaded, err := LoadCSR(context.Background(), path, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NodeCount != g.NodeCount {
		t.Fatalf("node count %d, want %d", loaded.NodeCount, g.NodeCount)
	}
	if len(loaded.neighbors) != len(g.neighbors) {
		t.Fatalf("loaded %d neighbours, want %d", len(loaded.neighbors), len(g.neighbors))
	}
	for i := range g.neighbors {
		if loaded.neighbors[i] != g.neighbors[i] {
			t.Fatalf("neighbour %d = %d, want %d", i, loaded.neighbors[i], g.neighbors[i])
		}
	}
	for i := range g.weights {
		if loaded.weights[i] != g.weights[i] {
			t.Fatalf("weight %d = %v, want %v", i, loaded.weights[i], g.weights[i])
		}
	}
	if err := loaded.Verify(); err != nil {
		t.Fatalf("a freshly written index failed Verify: %v", err)
	}
}

// TestLoadCSRRejectsAPaddedIndex is the regression test for the padding
// bug, in the strongest form available.
//
// The first version of the loader read the arrays and ignored whatever
// followed, so a padded file loaded "successfully" and served a graph
// with the padding still in memory. The loader now checks that the file
// is exactly the size its header describes, so a padded index is refused
// outright rather than silently accepted.
func TestLoadCSRRejectsAPaddedIndex(t *testing.T) {
	g := smallGraph()
	path := filepath.Join(t.TempDir(), "k.db")
	if err := persistCSRStream(context.Background(), &Builder{DBPath: path}, g,
		BuildResult{Nodes: g.NodeCount, Edges: int64(g.EdgeCount), TopN: g.TopN}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(csrPath(path))
	if err != nil {
		t.Fatal(err)
	}
	// Append the padding the broken writer produced.
	padded := append(append([]byte{}, raw...), make([]byte, 512)...)
	if err := os.WriteFile(csrPath(path), padded, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadCSR(context.Background(), path, nil, nil, nil)
	if err == nil {
		t.Fatal("a padded index loaded; the loader must refuse a file whose " +
			"size disagrees with its header, which is the padding signature")
	}
	if !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("err = %v, want it to name the layout disagreement", err)
	}
}

// TestLoadCSRRejectsATruncatedIndex is the other half: a short file must
// fail too, rather than decoding whatever bytes remain.
func TestLoadCSRRejectsATruncatedIndex(t *testing.T) {
	g := smallGraph()
	path := filepath.Join(t.TempDir(), "k.db")
	if err := persistCSRStream(context.Background(), &Builder{DBPath: path}, g,
		BuildResult{Nodes: g.NodeCount, Edges: int64(g.EdgeCount), TopN: g.TopN}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(csrPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(csrPath(path), raw[:len(raw)-8], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCSR(context.Background(), path, nil, nil, nil); err == nil {
		t.Fatal("a truncated index loaded")
	}
}

// TestPersistCSRStreamWritesNoTrailingZeroRun is the direct check: the file
// must not end in a run of zeros, which is the padding signature.
func TestPersistCSRStreamWritesNoTrailingZeroRun(t *testing.T) {
	g := smallGraph()
	path := filepath.Join(t.TempDir(), "k.db")
	if err := persistCSRStream(context.Background(), &Builder{DBPath: path}, g,
		BuildResult{Nodes: g.NodeCount, Edges: int64(g.EdgeCount), TopN: g.TopN}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(csrPath(path))
	if err != nil {
		t.Fatal(err)
	}
	// Count trailing zero bytes. A legitimate float32 weight of 0 or a
	// neighbour id of 0 could produce a zero, but not 512 of them.
	run := 0
	for i := len(raw) - 1; i >= 0 && raw[i] == 0; i-- {
		run++
	}
	if run >= 512 {
		t.Fatalf("%d trailing zero bytes: the writer is flushing its whole buffer, not its contents", run)
	}
}

// TestIndexExistsRejectsAHeaderOnlyFile guards the "index present" signal
// that serve uses to decide whether to warn.
func TestIndexExistsRejectsAHeaderOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.db")
	if err := os.WriteFile(csrPath(path), make([]byte, headerSize), 0o644); err != nil {
		t.Fatal(err)
	}
	if IndexExists(path) {
		t.Fatal("a file holding only a header is not an index; serve would try to load it and fail")
	}
}

// smallGraph is a hand-built graph with non-uniform degrees, so the
// offsets are not a trivial arithmetic sequence and a layout mistake
// shows up in the bytes.
func smallGraph() *CSR {
	// degrees: 2, 0, 1, 3  -> offsets 0,2,2,3,6
	offsets := []int64{0, 2, 2, 3, 6}
	neighbors := []int32{3, 2, 3, 0, 1, 0}
	weights := []float32{1.5, 2.5, 3.5, 4.5, 5.5, 6.5}
	return &CSR{
		NodeCount: 4,
		EdgeCount: 3,
		TopN:      8,
		offsets:   offsets,
		neighbors: neighbors,
		weights:   weights,
		names:     []string{"a", "b", "c", "d"},
		freq:      make([]float64, 4),
	}
}

var _ = fmt.Sprint
var _ = sql.ErrNoRows
var _ = strings.TrimSpace
