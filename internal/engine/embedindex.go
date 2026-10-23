package engine

import (
	"container/heap"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// tagIndex is the tag-embedding matrix, resident.
//
// The state database holds 634,232 tag vectors at 32 dimensions — 81 MiB as
// float32. It is loaded ONCE per process rather than read per request, and
// that is a deliberate trade rather than an oversight: a per-request read
// would stream 81 MiB of blobs out of SQLite on every call, which is
// strictly worse than holding them, and the memory cap that made resident
// tables unacceptable was removed on 2026-10-22 (SPEC §6).
//
// The slab is row-major over the TAG ID SPACE, not over a dense array of
// present ids: ids run from 1 and are dense in this mirror, so a slice
// indexed by tag id turns a lookup into arithmetic instead of a map probe in
// the hottest loop this package has (634k cosine comparisons per finder
// call). A sparse id space would waste at most a few rows.
//
// Absent rows are all-zero and their norm is zero, so every consumer must
// check ok rather than trusting a zero vector: a zero vector is a tag whose
// embedding was never built, and treating it as a real point at the origin
// would make it maximally similar to nothing and tie with everything.
type tagIndex struct {
	slab  []float32 // rows*dim, row-major
	norms []float32 // one per row, L2
	dim   int
	rows  int32 // max tag id + 1
}

// tagIndexOnce guards the lazy load. Engine has no constructor that could
// do it eagerly: the CLI builds engines for recommend, eval, tune and the
// e2e server, and only two of those paths read embeddings at all. A lite
// deployment and every test that never touches embeddings pay nothing.
type tagIndexState struct {
	once sync.Once
	idx  *tagIndex
	err  error
}

// loadTagIndex reads every tag embedding into one slab.
//
// An empty table is NOT an error: it means this mirror has never had
// `kindred embed` run, and the callers must distinguish "no embeddings" from
// "could not read embeddings" — the first is a supported deployment, the
// second is a fault that should be logged.
func loadTagIndex(ctx context.Context, db *sql.DB) (*tagIndex, error) {
	if db == nil {
		return nil, nil
	}
	var maxID sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(entity_id) FROM embeddings WHERE kind = 'tag'`).Scan(&maxID); err != nil {
		// A state database that predates the embeddings table answers with
		// "no such table". That is "no embeddings", not a fault.
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read embedding ids: %w", err)
	}
	if !maxID.Valid || maxID.Int64 < 0 {
		return nil, nil
	}
	rows := int32(maxID.Int64 + 1)
	if rows > 1<<24 {
		// 16.7M ids x 32 dims x 4 B = 2 GiB. A tag id space this large means
		// the state database is not this mirror's, and allocating for it
		// would take the process down rather than report the mismatch.
		return nil, fmt.Errorf("embedding id space is %d rows; refusing to allocate", rows)
	}

	var dim int
	if err := db.QueryRowContext(ctx,
		`SELECT dim FROM embeddings WHERE kind = 'tag' LIMIT 1`).Scan(&dim); err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read embedding width: %w", err)
	}
	if dim <= 0 || dim > 4096 {
		return nil, fmt.Errorf("embedding width %d is not usable", dim)
	}

	started := time.Now()
	idx := &tagIndex{
		slab:  make([]float32, int(rows)*dim),
		norms: make([]float32, rows),
		dim:   dim,
		rows:  rows,
	}
	q, err := db.QueryContext(ctx,
		`SELECT entity_id, dim, vec FROM embeddings WHERE kind = 'tag'`)
	if err != nil {
		return nil, fmt.Errorf("read embeddings: %w", err)
	}
	defer q.Close()
	var read, skipped int
	for q.Next() {
		var id int64
		var d int
		var blob []byte
		if err := q.Scan(&id, &d, &blob); err != nil {
			return nil, fmt.Errorf("scan embedding: %w", err)
		}
		if id < 0 || id >= int64(rows) || d != dim || len(blob) != d*4 {
			// A row written by a different build (another --dim) cannot be
			// mixed into this slab: the coordinates would land in the wrong
			// dimensions. Skip it and count it, and let the caller report
			// the shortfall rather than silently ranking half a space.
			skipped++
			continue
		}
		off := int(id) * dim
		var sum float64
		for i := 0; i < dim; i++ {
			v := float32FromLe(blob[i*4:])
			idx.slab[off+i] = v
			sum += float64(v) * float64(v)
		}
		idx.norms[id] = float32(math.Sqrt(sum))
		read++
	}
	if err := q.Err(); err != nil {
		return nil, fmt.Errorf("iterate embeddings: %w", err)
	}
	if read == 0 {
		return nil, nil
	}
	if skipped > 0 {
		return idx, fmt.Errorf("embedding index loaded %d rows but skipped %d "+
			"that did not match width %d; rebuild with `kindred embed --force`",
			read, skipped, dim)
	}
	_ = read
	slog.Info("tag embeddings loaded",
		"rows", read, "dim", dim, "took", time.Since(started).Round(time.Millisecond))
	return idx, nil
}

// isMissingTable reports whether err is SQLite's "no such table".
//
// Matched on the message because the driver exposes no typed error for it,
// and the alternative — treating every read error as "no embeddings" — would
// turn a corrupt state database into a silent loss of the embedding
// dimension, which is the failure mode this whole unit exists to remove.
func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column")
}

// tagVectors returns the process's tag-embedding matrix, loading it on first
// use.
//
// The load happens exactly once, including when it FAILS: a 634k-row scan that
// errored is not retried on every request, and the cached nil index makes the
// embedding signal skip (and name itself degraded) rather than turning every
// ranking into a database read.
func (e *Engine) tagVectors(ctx context.Context) (*tagIndex, error) {
	if e == nil {
		return nil, nil
	}
	e.tagIdx.once.Do(func() {
		var db *sql.DB
		if e.Store != nil {
			db = e.Store.DB
		}
		e.tagIdx.idx, e.tagIdx.err = loadTagIndex(ctx, db)
	})
	return e.tagIdx.idx, e.tagIdx.err
}

// seedCentroid returns the seeds' position in tag space, or nil when nothing
// can be placed.
//
// Each seed contributes ONE point, and that point is the mean of its tags, so
// a forty-tag bookmarked work does not outvote a five-tag one. Averaging all
// tags of all seeds at once would weight a seed by how much its author tagged
// it, which is a property of the tagging habit rather than of taste.
func seedCentroid(idx *tagIndex, seeds []rank.Candidate) []float32 {
	if idx == nil || len(seeds) == 0 {
		return nil
	}
	dim := idx.dim
	sum := make([]float32, dim)
	var placed int
	for _, s := range seeds {
		c := idx.centroid(s.TagIDs)
		if c == nil {
			continue
		}
		for i := range sum {
			sum[i] += c[i]
		}
		placed++
	}
	if placed == 0 {
		return nil
	}
	inv := 1 / float32(placed)
	for i := range sum {
		sum[i] *= inv
	}
	return sum
}

func (t *tagIndex) row(id int32) ([]float32, bool) {
	if t == nil || id < 0 || id >= t.rows || t.norms[id] == 0 {
		return nil, false
	}
	off := int(id) * t.dim
	return t.slab[off : off+t.dim], true
}

func (t *tagIndex) norm(id int32) float32 {
	if t == nil || id < 0 || id >= t.rows {
		return 0
	}
	return t.norms[id]
}

// centroid returns the mean of the vectors for ids, skipping absent ones.
//
// The mean rather than the sum: a work with forty tags and one with four
// must land in the same place if they name the same three concepts, and a
// sum would make the larger work's neighbourhood dominate the seed.
func (t *tagIndex) centroid(ids []int32) []float32 {
	if t == nil {
		return nil
	}
	out := make([]float32, t.dim)
	var n int
	for _, id := range ids {
		v, ok := t.row(id)
		if !ok {
			continue
		}
		for i := range v {
			out[i] += v[i]
		}
		n++
	}
	if n == 0 {
		return nil
	}
	inv := 1 / float32(n)
	for i := range out {
		out[i] *= inv
	}
	return out
}

// cosineOf returns the cosine similarity between a unit-normalisable vector
// and a stored tag's vector.
func (t *tagIndex) cosineOf(v []float32, vNorm float32, id int32) float64 {
	row, ok := t.row(id)
	if !ok || vNorm == 0 {
		return 0
	}
	var dot float64
	for i := range row {
		dot += float64(row[i]) * float64(v[i])
	}
	return dot / (float64(vNorm) * float64(t.norms[id]))
}

// normalise returns v's L2 norm, so a caller comparing one vector against
// many stored ones computes it once.
func normOf(v []float32) float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return float32(math.Sqrt(sum))
}

// tagSim is one tag and its similarity to a centroid.
type tagSim struct {
	ID  int32
	Sim float64
}

// nearestTags returns the k tags most similar to v, excluding ids in skip and
// anything below minSim.
//
// A bounded heap rather than a full sort: the slab holds 634,232 rows and k
// is 32, so sorting everything would spend two orders of magnitude more work
// than the answer needs. Ties break on tag id ASCENDING so the same centroid
// always yields the same tag list — a pool that reshuffles between identical
// requests is a pool that changes rankings.
func (t *tagIndex) nearestTags(v []float32, k int, minSim float64, skip map[int32]bool) []tagSim {
	if t == nil || len(v) != t.dim || k <= 0 {
		return nil
	}
	vNorm := normOf(v)
	if vNorm == 0 {
		return nil
	}
	h := &tagSimHeap{}
	heap.Init(h)
	for id := int32(0); id < t.rows; id++ {
		if t.norms[id] == 0 || skip[id] {
			continue
		}
		sim := t.cosineOf(v, vNorm, id)
		if sim < minSim {
			continue
		}
		if h.Len() < k {
			heap.Push(h, tagSim{ID: id, Sim: sim})
			continue
		}
		if betterThan(tagSim{ID: id, Sim: sim}, (*h)[0]) {
			(*h)[0] = tagSim{ID: id, Sim: sim}
			heap.Fix(h, 0)
		}
	}
	out := make([]tagSim, h.Len())
	for i := range out {
		out[i] = heap.Pop(h).(tagSim)
	}
	// Pop returns worst-first (the heap is a min-heap on quality), so the
	// final order is best-first.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sim != out[j].Sim {
			return out[i].Sim > out[j].Sim
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// betterThan orders by similarity descending, then id ascending: the same
// rule the final sort uses, so the heap's notion of "worst" agrees with the
// output's notion of "last".
func betterThan(a, b tagSim) bool {
	if a.Sim != b.Sim {
		return a.Sim > b.Sim
	}
	return a.ID < b.ID
}

type tagSimHeap []tagSim

func (h tagSimHeap) Len() int { return len(h) }
func (h tagSimHeap) Less(i, j int) bool {
	// min-heap on quality: the root is the WORST entry, so replacing it is
	// the cheap way to keep the top k.
	return betterThan(h[j], h[i])
}
func (h tagSimHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *tagSimHeap) Push(x any)   { *h = append(*h, x.(tagSim)) }
func (h *tagSimHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// NearTag is one tag and how close it sits to another in embedding space.
type NearTag struct {
	ID  int32   `json:"id"`
	Sim float64 `json:"similarity"`
}

// TagsNear returns the tags nearest id in the tag-embedding space.
//
// This is the reader-facing half of the embedding index. The pool source uses
// a seed CENTROID, which is the right question for ranking ("where is this
// reader standing?") and the wrong one for a tag page ("what else lives
// here?"). Asked about a tag, the answer is a neighbourhood, and its whole
// value is that it is NOT the co-occurrence neighbourhood: 634k tags have a
// vector, while the graph holds an edge only where two tags appeared on the
// same work. A tag with three works has no graph neighbours worth the name
// and still has a position.
//
// ok is false when this deployment holds no embeddings at all — a supported
// state (a mirror that never ran `kindred embed`), not an error, and the
// caller must say so rather than answering an empty list.
func (e *Engine) TagsNear(ctx context.Context, id int32, k int, minSim float64) (near []NearTag, ok bool, err error) {
	idx, ierr := e.tagVectors(ctx)
	if idx == nil {
		if ierr != nil {
			return nil, false, ierr
		}
		return nil, false, nil
	}
	v, found := idx.row(id)
	if !found {
		// The tag exists in the mirror but has no vector: `kindred embed` ran
		// before this tag was crawled, or skipped its family. That is a fact
		// about the INDEX, and the caller reports it by name — an empty list
		// would read as "nothing is near it".
		return nil, true, fmt.Errorf("tag %d has no embedding", id)
	}
	sims := idx.nearestTags(v, k, minSim, map[int32]bool{id: true})
	out := make([]NearTag, 0, len(sims))
	for _, s := range sims {
		out = append(out, NearTag{ID: s.ID, Sim: s.Sim})
	}
	// A partial index still answers, and the error travels with the answer so
	// the caller can report both: the rows it read and the rows it skipped
	// are different findings.
	return out, true, ierr
}
