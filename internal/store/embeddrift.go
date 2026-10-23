package store

// Embedding drift (#76).
//
// `kindred embed` rewrites the whole `embeddings` table. Nothing compared the
// old space with the new one, so a rebuild that moved every vector -- a different
// dim, a different iteration count, a corpus whose tags changed -- was
// indistinguishable from a rebuild that reproduced the same space. Every stored
// reader weight was trained against the OLD positions, so that difference is not
// cosmetic: it silently invalidates what the site learned.
//
// # What is recorded, and why this and not a hash of the table
//
// A hash of 634k rows answers "did anything change" and nothing else — it cannot
// say whether the space moved or merely reshuffled, and it changes when one tag
// is added. So a run stores a small FINGERPRINT instead:
//
//   - rows and dim (a dim change is a different space by definition);
//   - the mean L2 norm (a solver settling at a different scale moves every
//     cosine, and cosine is scale-invariant only per-vector — a global scale
//     change shows up here first);
//   - the centroid, as the mean of every vector;
//   - a SAMPLE of individual vectors, so per-tag movement is measurable rather
//     than inferred from an average that can hide two opposite movements.
//
// The sample is the same tag ids every time (the smallest ids present), so two
// runs are comparable without storing 634k vectors.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// EmbedSample is one tag's vector from one build.
type EmbedSample struct {
	TagID int32     `json:"tag_id"`
	Vec   []float32 `json:"vec"`
}

// EmbedRun is one fingerprint of the stored embedding table.
type EmbedRun struct {
	ID       int64         `json:"id"`
	Rows     int           `json:"rows"`
	Dim      int           `json:"dim"`
	MeanNorm float64       `json:"mean_norm"`
	Centroid []float32     `json:"centroid"`
	Sample   []EmbedSample `json:"sample"`
	BuiltAt  string        `json:"built_at"`
}

// RecordEmbedRun stores a fingerprint of the table as it stands now.
//
// `sampleN` is how many tag vectors to keep for per-tag comparison. Zero or
// negative means the package default; the caller does not get to store an
// unbounded sample.
func (s *Store) RecordEmbedRun(ctx context.Context, sampleN int) (EmbedRun, error) {
	var run EmbedRun
	if sampleN <= 0 || sampleN > 512 {
		sampleN = 64
	}

	rows, err := s.DB.QueryContext(ctx, `SELECT entity_id, dim, vec FROM embeddings ORDER BY CAST(entity_id AS INTEGER) LIMIT ?`, sampleN+1)
	if err != nil {
		return run, err
	}
	defer rows.Close()
	var sample []EmbedSample
	for rows.Next() {
		if len(sample) >= sampleN {
			break
		}
		var id int64
		var dim int
		var blob []byte
		if err := rows.Scan(&id, &dim, &blob); err != nil {
			return run, err
		}
		vec, err := decodeVec(blob, dim)
		if err != nil {
			return run, err
		}
		sample = append(sample, EmbedSample{TagID: int32(id), Vec: vec})
		run.Dim = dim
	}
	// Close BEFORE the next query. The pool holds exactly one connection
	// (store.go: MaxOpenConns(1)), and an unexhausted Rows holds that
	// connection until it is closed -- so with more rows than the sample asks
	// for, the centroid query below waits on itself forever. Found on the live
	// database, where 634k rows hit the `break` and the unit test's two rows
	// never did: an exhausted Rows closes itself, which is why the small
	// fixture passed.
	if err := rows.Close(); err != nil {
		return run, err
	}
	if err := rows.Err(); err != nil {
		return run, err
	}
	if len(sample) == 0 {
		return run, fmt.Errorf("no embeddings are stored, so there is no fingerprint to take")
	}

	// The centroid and the mean norm need the WHOLE table: a fingerprint built
	// from the sample alone would describe the sample, and the sample is chosen
	// by id, which correlates with nothing but is also not a random draw.
	crows, err := s.DB.QueryContext(ctx, `SELECT dim, vec FROM embeddings`)
	if err != nil {
		return run, err
	}
	defer crows.Close()
	centroid := make([]float64, run.Dim)
	var n int
	var normSum float64
	for crows.Next() {
		var dim int
		var blob []byte
		if err := crows.Scan(&dim, &blob); err != nil {
			return run, err
		}
		vec, err := decodeVec(blob, dim)
		if err != nil {
			return run, err
		}
		var norm float64
		for i, v := range vec {
			centroid[i] += float64(v)
			norm += float64(v) * float64(v)
		}
		normSum += math.Sqrt(norm)
		n++
	}
	// Same reason as above, and here the waiting query is an INSERT: until the
	// scan's connection is returned, the fingerprint cannot be written at all.
	if err := crows.Close(); err != nil {
		return run, err
	}
	if err := crows.Err(); err != nil {
		return run, err
	}
	if n == 0 {
		return run, fmt.Errorf("no embeddings are stored")
	}
	// run.Rows is the TABLE's row count, from the centroid pass, not the
	// sample's extent: a fingerprint of a 634k-row table that recorded "rows 64"
	// would make the drift table's own size column a lie.
	run.Rows = n
	run.MeanNorm = normSum / float64(n)
	cent := make([]float32, len(centroid))
	for i, v := range centroid {
		cent[i] = float32(v / float64(n))
	}
	run.Centroid = cent
	run.Sample = sample
	run.BuiltAt = time.Now().UTC().Format(time.RFC3339)

	centJSON, err := json.Marshal(cent)
	if err != nil {
		return run, err
	}
	sampleJSON, err := json.Marshal(sample)
	if err != nil {
		return run, err
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO embed_runs (rows, dim, mean_norm, centroid, sample, built_at)
		 VALUES (?,?,?,?,?,?)`,
		run.Rows, run.Dim, run.MeanNorm, string(centJSON), string(sampleJSON), run.BuiltAt)
	if err != nil {
		return run, err
	}
	run.ID, _ = res.LastInsertId()
	return run, nil
}

// EmbedRuns returns the most recent fingerprints, newest first.
func (s *Store) EmbedRuns(ctx context.Context, limit int) ([]EmbedRun, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, rows, dim, mean_norm, centroid, sample, built_at
		 FROM embed_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []EmbedRun
	for rows.Next() {
		var run EmbedRun
		var centJSON, sampleJSON string
		if err := rows.Scan(&run.ID, &run.Rows, &run.Dim, &run.MeanNorm, &centJSON, &sampleJSON, &run.BuiltAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(centJSON), &run.Centroid)
		_ = json.Unmarshal([]byte(sampleJSON), &run.Sample)
		out = append(out, run)
	}
	return out, rows.Err()
}

// EmbedDrift compares two fingerprints.
type EmbedDrift struct {
	FromID     int64  `json:"from_run"`
	ToID       int64  `json:"to_run"`
	FromAt     string `json:"from_at"`
	ToAt       string `json:"to_at"`
	RowsBefore int    `json:"rows_before"`
	RowsAfter  int    `json:"rows_after"`
	DimBefore  int    `json:"dim_before"`
	DimAfter   int    `json:"dim_after"`
	// CentroidCosine is the similarity of the two centroids: 1.0 means the
	// average position did not move at all.
	CentroidCosine float64 `json:"centroid_cosine"`
	// MeanNormRatio is after/before; 1.0 means the solver settled at the same
	// scale.
	MeanNormRatio float64 `json:"mean_norm_ratio"`
	// SampleCosine is the mean per-tag cosine over the tags present in both
	// samples. This is the number that says whether individual tags moved, which
	// an average can hide when two halves move in opposite directions.
	//
	// Tags whose vector is all zeros are NOT in this mean: a tag with no vector
	// has no position to move, and counting it as cosine 0 reports a drift that
	// did not happen. They are counted in EmptySample instead -- measured on the
	// live table, where 3 of the 64 sampled tags carry zeros and dragged an
	// honest 1.0000 down to 0.9531, i.e. a rebuild compared with ITSELF was
	// reported as having moved the space.
	SampleCosine float64 `json:"sample_cosine"`
	SampleN      int     `json:"sample_n"`
	// EmptySample is how many sampled tags carried a zero vector in either run.
	EmptySample int `json:"empty_sample"`
	// Verdict is a sentence, not a number: an operator reading 0.87 should not
	// have to remember what 0.87 means.
	Verdict string `json:"verdict"`
}

// DriftFloor is the per-tag cosine below which a rebuild counts as having moved
// the space.
//
// 0.98 rather than 0.9: this is a deterministic solver over the same corpus, so a
// rebuild that produces cosine 0.95 across its sample has genuinely changed
// positions, not merely re-rolled a seed. A floor of 0.9 would call that "fine"
// and the weights trained against the old space would go quietly stale.
const DriftFloor = 0.98

// DriftBetween computes the drift from one fingerprint to another.
func DriftBetween(before, after EmbedRun) EmbedDrift {
	d := EmbedDrift{
		FromID: before.ID, ToID: after.ID, FromAt: before.BuiltAt, ToAt: after.BuiltAt,
		RowsBefore: before.Rows, RowsAfter: after.Rows,
		DimBefore: before.Dim, DimAfter: after.Dim,
		CentroidCosine: cosine32(before.Centroid, after.Centroid),
	}
	if before.MeanNorm > 0 {
		d.MeanNormRatio = after.MeanNorm / before.MeanNorm
	}
	// Per-tag comparison over the tags both runs sampled.
	byID := make(map[int32][]float32, len(before.Sample))
	for _, s := range before.Sample {
		byID[s.TagID] = s.Vec
	}
	var sum float64
	var n, empty int
	for _, s := range after.Sample {
		prev, ok := byID[s.TagID]
		if !ok {
			continue
		}
		if !hasDirection(prev) || !hasDirection(s.Vec) {
			empty++
			continue
		}
		sum += cosine32(prev, s.Vec)
		n++
	}
	d.EmptySample = empty
	d.SampleN = n
	if n > 0 {
		d.SampleCosine = sum / float64(n)
	}
	d.Verdict = driftVerdict(d)
	return d
}

func driftVerdict(d EmbedDrift) string {
	if d.DimBefore != d.DimAfter {
		return fmt.Sprintf("the space was rebuilt at a different width (%d -> %d); nothing trained against "+
			"the old vectors is comparable, and every stored tag weight should be treated as untrained",
			d.DimBefore, d.DimAfter)
	}
	if d.SampleN == 0 {
		if d.EmptySample > 0 {
			return fmt.Sprintf("none of the sampled tags carry a vector in both runs (%d carry none at all), "+
				"so per-tag movement cannot be measured", d.EmptySample)
		}
		return "the two runs share no sampled tags, so per-tag movement cannot be measured"
	}
	if d.SampleCosine < DriftFloor {
		return fmt.Sprintf("the space MOVED: sampled tags average cosine %.3f against the previous build "+
			"(floor %.2f). Reader weights trained before this rebuild were trained against other positions%s",
			d.SampleCosine, DriftFloor, d.emptyNote())
	}
	if d.MeanNormRatio < 0.9 || d.MeanNormRatio > 1.1 {
		return fmt.Sprintf("tag positions held (cosine %.3f) but the scale changed by %.0f%%; "+
			"cosine-based readers are unaffected, additive scores are not",
			d.SampleCosine, (d.MeanNormRatio-1)*100)
	}
	return fmt.Sprintf("reproduced: sampled tags average cosine %.3f, centroid %.3f, scale %.3f%s",
		d.SampleCosine, d.CentroidCosine, d.MeanNormRatio, d.emptyNote())
}

// emptyNote keeps the excluded tags visible. Silently dropping them from the
// mean would make a thin sample look like a complete one.
func (d EmbedDrift) emptyNote() string {
	if d.EmptySample == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d sampled tag(s) carry no vector and are excluded from the mean)", d.EmptySample)
}

// hasDirection reports whether a vector has a position at all: an all-zero
// vector is how this table records "no embedding for this tag", and two of them
// have no angle to measure.
func hasDirection(v []float32) bool {
	for _, f := range v {
		if f != 0 {
			return true
		}
	}
	return false
}

func decodeVec(blob []byte, dim int) ([]float32, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("dim %d", dim)
	}
	if len(blob) != dim*4 {
		return nil, fmt.Errorf("expected %d bytes for dim %d, got %d", dim*4, dim, len(blob))
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		bits := uint32(blob[i*4]) | uint32(blob[i*4+1])<<8 | uint32(blob[i*4+2])<<16 | uint32(blob[i*4+3])<<24
		out[i] = math.Float32frombits(bits)
	}
	return out, nil
}

func cosine32(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
