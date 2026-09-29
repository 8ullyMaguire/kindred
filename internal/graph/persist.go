package graph

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
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

// LoadCSR reads an index written by persistCSR, restoring names and
// frequencies from the corpus.
//
// Frequencies are re-read rather than stored: they change with the corpus,
// and stale PMI produces a silently wrong ranking rather than an error.
func LoadCSR(ctx context.Context, dbPath string, corpus *sql.DB, readFreq func() (map[int32]int64, error), readNames func() ([]string, error)) (*CSR, error) {
	raw, err := os.ReadFile(csrPath(dbPath))
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	if len(raw) < headerSize {
		return nil, fmt.Errorf("index file is truncated: %d bytes", len(raw))
	}
	if string(raw[0:4]) != string(csrMagic[:]) {
		return nil, fmt.Errorf("index file has the wrong magic; not a kindred index")
	}
	if v := binary.LittleEndian.Uint32(raw[4:8]); v != 1 {
		return nil, fmt.Errorf("index format version %d is not supported (want 1)", v)
	}
	nodes := int(binary.LittleEndian.Uint64(raw[8:16]))
	edges := int(binary.LittleEndian.Uint64(raw[16:24]))
	topN := int(binary.LittleEndian.Uint64(raw[24:32]))

	g, err := Decode(nodes, edges, topN, raw[headerSize:])
	if err != nil {
		return nil, err
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
		g.nameOf = make(map[int32]string, len(names))
		for i, n := range names {
			g.nameOf[int32(i)] = n
		}
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
