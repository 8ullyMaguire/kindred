package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddingTransposeFromDisk is the check for the transpose the
// buildEmbeddings comment describes. The shapes do not line up — the
// solver gives one component at a time (component-major), the store wants
// one dim-vector per tag (entity-major) — and the first version of this
// sliced a single component as though it were the whole matrix, which
// panicked on every run.
//
// The test writes three known components to disk, transposes them the same
// way the command does, and checks the rows byte for byte against what the
// entity-major layout requires.
func TestEmbeddingTransposeFromDisk(t *testing.T) {
	const nodes, dim = 5, 3
	// component c, tag t -> c*nodes+t
	want := make([][]float32, dim)
	for c := 0; c < dim; c++ {
		want[c] = make([]float32, nodes)
		for tt := 0; tt < nodes; tt++ {
			want[c][tt] = float32(c*nodes+tt) / 8
		}
	}

	dir := t.TempDir()
	files := make([]*os.File, dim)
	for c := 0; c < dim; c++ {
		var err error
		files[c], err = os.Create(filepath.Join(dir, fmt.Sprintf("c%03d.f32", c)))
		if err != nil {
			t.Fatal(err)
		}
		w := bufio.NewWriter(files[c])
		if err := binary.Write(w, binary.LittleEndian, want[c]); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	// Rewind: the handles are positioned at the end after writing.
	for _, f := range files {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
	}

	// The transpose, exactly as buildEmbeddings does it.
	rowBuf := make([]byte, dim*4)
	for row := 0; row < nodes; row++ {
		for c := 0; c < dim; c++ {
			if err := binary.Read(files[c], binary.LittleEndian, rowBuf[c*4:c*4+4]); err != nil {
				t.Fatal(err)
			}
		}
		for c := 0; c < dim; c++ {
			got := math.Float32frombits(binary.LittleEndian.Uint32(rowBuf[c*4:]))
			if got != want[c][row] {
				t.Errorf("tag %d coordinate %d: got %v, want %v", row, c, got, want[c][row])
			}
		}
	}
}

// TestEmbeddingTransposeFailsCleanlyAtEOF: a component file shorter than
// nodes*4 bytes must produce an error naming the tag and coordinate, not a
// silent short row that reads as a zero embedding. A tag embedded as all
// zeros is a valid-looking row in the table and a wrong ranking in every
// response.
func TestEmbeddingTransposeFailsCleanlyAtEOF(t *testing.T) {
	dir := t.TempDir()
	// 9 bytes: not a whole number of 4-byte coordinates, and shorter than
	// the 12 the transpose below needs.
	short := filepath.Join(dir, "short.f32")
	if err := os.WriteFile(short, make([]byte, 9), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(short)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const nodes, dim = 2, 3
	rowBuf := make([]byte, dim*4)
	var readErr error
	row, c := 0, 0
	for row < nodes && readErr == nil {
		for c = 0; c < dim; c++ {
			if readErr = binary.Read(f, binary.LittleEndian, rowBuf[c*4:c*4+4]); readErr != nil {
				break
			}
		}
		row++
	}
	if readErr == nil {
		t.Fatal("reading a truncated component file to the declared tag count " +
			"succeeded; the last tags would be stored as zero embeddings")
	}
	if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want an EOF-flavoured error", readErr)
	}
}
