package graph

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
)

// csrPath is where the encoded graph lives: a file beside kindred.db,
// not a BLOB column. A 118 MB BLOB is read into memory on every access,
// which is the same mistake as holding the graph in a Python dict — the
// representation is what costs, wherever it is stored.
func csrPath(dbPath string) string {
	return dbPath + ".graph"
}

// headerSize is the fixed prefix written before the adjacency arrays:
// magic, version, node count, edge count, topN.
const headerSize = 4 + 4 + 8 + 8 + 8

var csrMagic = [4]byte{'K', 'G', 'R', '1'}

// persistCSRStream writes the graph straight to disk from its arrays.
//
// The alternative — Encode() into one contiguous buffer and then write it
// — was measured at 285 MB of transient allocation on the real corpus,
// because the blob exists alongside the three arrays it was copied from.
// Streaming the arrays into the file's buffer costs one 64 KiB copy buffer
// instead of a second copy of the whole graph.
func persistCSRStream(ctx context.Context, b *Builder, g *CSR, res BuildResult) error {
	if b.DBPath == "" {
		return fmt.Errorf("no db path: cannot persist the index")
	}
	path := csrPath(b.DBPath)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	defer f.Close()

	var head [headerSize]byte
	copy(head[0:4], csrMagic[:])
	binary.LittleEndian.PutUint32(head[4:8], 1)
	binary.LittleEndian.PutUint64(head[8:16], uint64(g.NodeCount))
	binary.LittleEndian.PutUint64(head[16:24], uint64(g.EdgeCount))
	binary.LittleEndian.PutUint64(head[24:32], uint64(g.TopN))
	if _, err := f.Write(head[:]); err != nil {
		return fmt.Errorf("write index header: %w", err)
	}

	// A length-tracked buffer, flushed with an explicit length.
	//
	// The first version of this used `buf = buf[:0]` and `f.Write(buf)`,
	// which writes the whole 16 KiB backing array on every flush and
	// nothing at all when the buffer is empty — so the file gained a run
	// of zero padding after each real write. The loader, which is
	// position-based, then read its neighbours out of that padding: the
	// first neighbour of the real corpus came back as 2873866, which is
	// the value of the last offset. The file was 101,032 bytes longer
	// than its own header implied, which is how the padding was found.
	//
	// Write with an explicit length and the layout is exactly what
	// Decode expects, with no padding anywhere.
	const chunk = 16 * 1024
	var (
		buf  [chunk]byte
		fill int
	)
	flush := func() error {
		if fill == 0 {
			return nil
		}
		_, err := f.Write(buf[:fill])
		fill = 0
		return err
	}
	put := func(b []byte) error {
		if len(b) > chunk {
			// Too big to buffer: flush what is held, then write straight
			// through, so the bytes never need a full-size staging copy.
			if err := flush(); err != nil {
				return err
			}
			_, err := f.Write(b)
			return err
		}
		if fill+len(b) > chunk {
			if err := flush(); err != nil {
				return err
			}
		}
		copy(buf[fill:], b)
		fill += len(b)
		return nil
	}

	var scratch [8]byte
	for _, v := range g.offsets {
		binary.LittleEndian.PutUint64(scratch[:], uint64(v))
		if err := put(scratch[:]); err != nil {
			return fmt.Errorf("write offsets: %w", err)
		}
	}
	for _, v := range g.neighbors {
		binary.LittleEndian.PutUint32(scratch[:4], uint32(v))
		if err := put(scratch[:4]); err != nil {
			return fmt.Errorf("write neighbours: %w", err)
		}
	}
	for _, v := range g.weights {
		binary.LittleEndian.PutUint32(scratch[:4], math.Float32bits(v))
		if err := put(scratch[:4]); err != nil {
			return fmt.Errorf("write weights: %w", err)
		}
	}
	if err := flush(); err != nil {
		return fmt.Errorf("write index: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync index: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close index: %w", err)
	}
	// Rename is atomic: a reader sees either the old index or the new
	// one, never a half-written file that decodes into a wrong graph.
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit index: %w", err)
	}
	return nil
}

// persistCSR writes the encoded graph with a header, atomically.
func persistCSR(ctx context.Context, b *Builder, blob []byte, res BuildResult) error {
	if b.DBPath == "" {
		return fmt.Errorf("no db path: cannot persist the index")
	}
	path := csrPath(b.DBPath)
	tmp := path + ".tmp"

	buf := make([]byte, headerSize+len(blob))
	copy(buf[0:4], csrMagic[:])
	binary.LittleEndian.PutUint32(buf[4:8], 1) // format version
	binary.LittleEndian.PutUint64(buf[8:16], uint64(res.Nodes))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(res.Edges))
	binary.LittleEndian.PutUint64(buf[24:32], uint64(res.TopN))
	copy(buf[headerSize:], blob)

	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("write index: %w", err)
	}
	// Rename is atomic: a reader sees either the old index or the new
	// one, never a half-written file that decodes into a wrong graph.
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit index: %w", err)
	}
	return nil
}

// LoadCSR reads an index written by persistCSRStream, restoring names and
// frequencies from the corpus.
//
// The index is read through a streaming decoder rather than
// os.ReadFile + Decode. The first version did the obvious thing — read
// 28 MB into one buffer, then copy it into the offsets, neighbours and
// weights arrays — and lite mode measured 58.9 MB of heap, over its
// 60 MB cap, for a graph that is 27 MB on disk. It was the same "hold it
// twice" mistake the writer had: this process was the one paying for it
// twice, on every start, to hold a file it was about to discard.
//
// The header is read first so the array lengths are known, then each
// array is decoded straight from a buffered reader. The peak is one
// buffer plus the arrays, not the arrays plus a copy of themselves.
func LoadCSR(ctx context.Context, dbPath string, corpus *sql.DB, readFreq func() (map[int32]int64, error), readNames func() ([]string, error)) (*CSR, error) {
	path := csrPath(dbPath)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat index: %w", err)
	}
	if st.Size() < headerSize {
		return nil, fmt.Errorf("index file is truncated: %d bytes", st.Size())
	}

	head := make([]byte, headerSize)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, fmt.Errorf("read index header: %w", err)
	}
	if string(head[0:4]) != string(csrMagic[:]) {
		return nil, fmt.Errorf("index file has the wrong magic; not a kindred index")
	}
	if v := binary.LittleEndian.Uint32(head[4:8]); v != 1 {
		return nil, fmt.Errorf("index format version %d is not supported (want 1)", v)
	}
	nodes := int(binary.LittleEndian.Uint64(head[8:16]))
	edges := int(binary.LittleEndian.Uint64(head[16:24]))
	topN := int(binary.LittleEndian.Uint64(head[24:32]))
	if nodes < 0 || nodes > maxSaneNodes {
		return nil, fmt.Errorf("index header claims %d nodes, which is not a real graph", nodes)
	}

	// A 256 KiB read buffer: large enough that the per-read syscall cost
	// disappears against 28 MB, small enough to be noise.
	br := bufio.NewReaderSize(f, 256*1024)
	g := &CSR{
		NodeCount: nodes,
		EdgeCount: edges,
		TopN:      topN,
		offsets:   make([]int64, nodes+1),
		freq:      make([]float64, nodes),
	}

	var scratch [8]byte
	for i := range g.offsets {
		if _, err := io.ReadFull(br, scratch[:8]); err != nil {
			return nil, fmt.Errorf("read offsets (%d/%d): %w", i, len(g.offsets), err)
		}
		g.offsets[i] = int64(binary.LittleEndian.Uint64(scratch[:8]))
	}
	n := int(g.offsets[nodes])
	if n < 0 {
		return nil, fmt.Errorf("index declares %d entries, which is negative", n)
	}
	// The file must be exactly the header, the offsets, and two arrays of
	// n entries. Anything else means the writer and reader disagree, and
	// reading on regardless would decode whatever happens to be there.
	want := int64(headerSize) + int64(8*(nodes+1)) + int64(8*n)
	if st.Size() != want {
		return nil, fmt.Errorf("index file is %d bytes but its header describes %d; "+
			"the writer and reader disagree on the layout", st.Size(), want)
	}

	g.neighbors = make([]int32, n)
	for i := range g.neighbors {
		if _, err := io.ReadFull(br, scratch[:4]); err != nil {
			return nil, fmt.Errorf("read neighbours (%d/%d): %w", i, n, err)
		}
		g.neighbors[i] = int32(binary.LittleEndian.Uint32(scratch[:4]))
	}
	g.weights = make([]float32, n)
	for i := range g.weights {
		if _, err := io.ReadFull(br, scratch[:4]); err != nil {
			return nil, fmt.Errorf("read weights (%d/%d): %w", i, n, err)
		}
		g.weights[i] = math.Float32frombits(binary.LittleEndian.Uint32(scratch[:4]))
	}

	if err := g.Verify(); err != nil {
		return nil, fmt.Errorf("index on disk failed its invariants: %w", err)
	}
	if readNames != nil {
		names, err := readNames()
		if err != nil {
			return nil, err
		}
		g.names = names
	}
	if readFreq != nil {
		freq, err := readFreq()
		if err != nil {
			return nil, err
		}
		for id, n := range freq {
			g.SetFrequency(id, int(n))
		}
	}
	return g, nil
}

// maxSaneNodes bounds what a header may claim, so a corrupt or hostile
// file cannot make the process allocate for a graph that does not exist.
const maxSaneNodes = 1 << 28

// IndexExists reports whether an index is on disk.
func IndexExists(dbPath string) bool {
	st, err := os.Stat(csrPath(dbPath))
	return err == nil && st.Size() > headerSize
}

// IndexSize returns the on-disk index size in bytes.
func IndexSize(dbPath string) int64 {
	st, err := os.Stat(csrPath(dbPath))
	if err != nil {
		return 0
	}
	return st.Size()
}
