package corpus

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
)

// Document frequencies: how many works carry each tag.
//
// The IDF-weighted overlap signal needs this for every tag it scores, and
// computing it per request is not affordable. Measured on the mirror (634,231
// tags, millions of work_tags rows):
//
//   - the pool's own tags, about 4,200 distinct per request, took 12.5 s via
//     `WHERE tag_id IN (...) GROUP BY tag_id`
//   - every tag in one pass took 29.7 s via a covering-index scan
//
// The first is a per-request cost that made every recommendation slower than
// the timeout a reader will wait, and it was shipped once without a live run.
// The second is a startup cost of the same order as the graph build.
//
// So the frequencies are computed ONCE, by `kindred ingest`, into a side file
// beside the mirror — exactly the shape internal/graph already uses for its
// CSR index, and for the same reason: the mirror is read-only to the server
// and a 634k-row table does not belong in a per-request query.
//
// When the file is absent the signal SKIPS for every candidate, which
// rank.Meta reports by name in `degraded`. That is the honest failure: the
// response says idf_overlap did not run, rather than ranking as if rarity
// meant nothing.

// DocFreqMagic identifies the file and its version.
var DocFreqMagic = [4]byte{'K', 'D', 'F', '1'}

// docFreqHeaderSize is magic + total works + tag count.
const docFreqHeaderSize = 4 + 8 + 8

// DocFreq is the loaded frequency table.
type DocFreq struct {
	// Total is the number of works in the corpus. A tag on every work
	// contributes zero evidence, which is only expressible if the total is
	// known.
	Total float64
	// ByTag maps a tag id to the number of works carrying it.
	ByTag map[int32]float64
}

// Count returns the number of works carrying a tag, and whether the tag was
// present.
//
// A tag absent from the table is NOT an error: the table is built from
// work_tags, and a tag that exists but is on no work has a genuine frequency
// of zero. The caller decides how to treat that; the IDF signal treats it as
// maximally rare.
func (d *DocFreq) Count(id int32) (float64, bool) {
	if d == nil {
		return 0, false
	}
	n, ok := d.ByTag[id]
	return n, ok
}

// DocFreqPath is where the table lives: beside the corpus, as a side file
// rather than a table inside it.
//
// The mirror is opened read-only by the server (`mode=ro`), so a table would
// be unwritable there; a side file needs no write access to the mirror and
// can be rebuilt without touching it.
func DocFreqPath(corpusPath string) string {
	return corpusPath + ".docfreq"
}

// BuildDocFreq computes every tag's document frequency and writes it.
//
// One covering-index scan over work_tags. It is deliberately a build-time
// step rather than a query: see the file comment for the measurements.
func BuildDocFreq(ctx context.Context, db *sql.DB, corpusPath string) (tags int, total float64, err error) {
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("work count: %w", err)
	}

	path := DocFreqPath(corpusPath)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, 0, fmt.Errorf("create docfreq: %w", err)
	}
	defer f.Close()

	rows, err := db.QueryContext(ctx, `SELECT tag_id, COUNT(*) FROM work_tags GROUP BY tag_id`)
	if err != nil {
		return 0, 0, fmt.Errorf("tag doc freq: %w", err)
	}
	defer rows.Close()

	// The count is not known until the scan ends, so the header is written
	// last, into a fixed-size prefix left blank here. Rewriting a prefix is
	// cheaper than buffering 634k pairs in memory to learn a number first.
	if _, err := f.Write(make([]byte, docFreqHeaderSize)); err != nil {
		return 0, 0, fmt.Errorf("write header: %w", err)
	}

	w := bufio.NewWriterSize(f, 64<<10)
	var pair [8]byte
	for rows.Next() {
		var id int64
		var df int64
		if err := rows.Scan(&id, &df); err != nil {
			return 0, 0, err
		}
		binary.LittleEndian.PutUint32(pair[0:4], uint32(id))
		binary.LittleEndian.PutUint32(pair[4:8], uint32(df))
		if _, err := w.Write(pair[:]); err != nil {
			return 0, 0, fmt.Errorf("write docfreq row: %w", err)
		}
		tags++
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if err := w.Flush(); err != nil {
		return 0, 0, fmt.Errorf("flush docfreq: %w", err)
	}

	// Now the header, over the blank prefix.
	var head [docFreqHeaderSize]byte
	copy(head[0:4], DocFreqMagic[:])
	binary.LittleEndian.PutUint64(head[4:12], uint64(total))
	binary.LittleEndian.PutUint64(head[12:20], uint64(tags))
	if _, err := f.WriteAt(head[:], 0); err != nil {
		return 0, 0, fmt.Errorf("write docfreq header: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, 0, fmt.Errorf("sync docfreq: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, 0, fmt.Errorf("close docfreq: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, 0, fmt.Errorf("install docfreq: %w", err)
	}
	return tags, total, nil
}

// LoadDocFreq reads the table written by BuildDocFreq.
//
// A missing file is reported as os.ErrNotExist rather than being swallowed:
// the caller distinguishes "this mirror has never been ingested" (skip the
// signal, say so) from "the file is corrupt" (a real problem worth a
// warning).
func LoadDocFreq(corpusPath string) (*DocFreq, error) {
	path := DocFreqPath(corpusPath)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < docFreqHeaderSize {
		return nil, fmt.Errorf("%s is %d bytes, shorter than its own header", path, st.Size())
	}

	var head [docFreqHeaderSize]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return nil, fmt.Errorf("read docfreq header: %w", err)
	}
	if [4]byte(head[0:4]) != DocFreqMagic {
		return nil, fmt.Errorf("%s is not a kindred docfreq file (bad magic %q)", path, string(head[0:4]))
	}
	total := float64(binary.LittleEndian.Uint64(head[4:12]))
	count := int64(binary.LittleEndian.Uint64(head[12:20]))

	want := docFreqHeaderSize + count*8
	if st.Size() != want {
		return nil, fmt.Errorf("%s is %d bytes, want %d for %d tags", path, st.Size(), want, count)
	}

	out := &DocFreq{Total: total, ByTag: make(map[int32]float64, count)}
	r := bufio.NewReaderSize(f, 64<<10)
	// The header has been consumed by ReadAt, which does not move the file
	// offset, so skip it explicitly.
	if _, err := r.Discard(docFreqHeaderSize); err != nil {
		return nil, err
	}
	var pair [8]byte
	for i := int64(0); i < count; i++ {
		if _, err := ioReadFull(r, pair[:]); err != nil {
			return nil, fmt.Errorf("read docfreq row %d: %w", i, err)
		}
		id := int32(binary.LittleEndian.Uint32(pair[0:4]))
		df := float64(binary.LittleEndian.Uint32(pair[4:8]))
		out.ByTag[id] = df
	}
	return out, nil
}

// ioReadFull is io.ReadFull, kept local so this file needs no io import for
// one call.
func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
