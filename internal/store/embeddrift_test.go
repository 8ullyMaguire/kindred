package store

// Embedding drift (#76).
//
// A drift monitor is only worth having if it can be WRONG in a way an operator
// would notice, so the tests pin the three verdicts rather than the arithmetic:
// a rebuild that reproduces the space says so, a rebuild that moved it says so
// loudly, and a change of width is its own sentence because nothing trained
// against the old vectors survives it.

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestIdenticalRebuildsReportReproduced(t *testing.T) {
	before := EmbedRun{
		ID: 1, Rows: 3, Dim: 4, MeanNorm: 1.0,
		Centroid: []float32{0.1, 0.2, 0.3, 0.4},
		Sample: []EmbedSample{
			{TagID: 7, Vec: []float32{1, 0, 0, 0}},
			{TagID: 9, Vec: []float32{0, 1, 0, 0}},
		},
	}
	after := before
	after.ID = 2
	d := DriftBetween(before, after)
	if d.SampleCosine < 0.999 {
		t.Fatalf("sample cosine %.4f for an identical rebuild", d.SampleCosine)
	}
	if !strings.Contains(d.Verdict, "reproduced") {
		t.Fatalf("verdict = %q, want it to say the space was reproduced", d.Verdict)
	}
}

func TestAMovedSpaceSaysSoAndNamesTheConsequence(t *testing.T) {
	before := EmbedRun{
		ID: 1, Dim: 4, MeanNorm: 1.0,
		Centroid: []float32{0.1, 0.2, 0.3, 0.4},
		Sample: []EmbedSample{
			{TagID: 7, Vec: []float32{1, 0, 0, 0}},
			{TagID: 9, Vec: []float32{0, 1, 0, 0}},
		},
	}
	after := EmbedRun{
		ID: 2, Dim: 4, MeanNorm: 1.0,
		Centroid: []float32{-0.1, -0.2, -0.3, -0.4},
		Sample: []EmbedSample{
			// Both tags rotated: the centroid moved AND the individual
			// positions moved, which is the case the sample exists for.
			{TagID: 7, Vec: []float32{0, 0, 1, 0}},
			{TagID: 9, Vec: []float32{0, 0, 0, 1}},
		},
	}
	d := DriftBetween(before, after)
	if d.SampleCosine > 0.01 {
		t.Fatalf("sample cosine %.3f for orthogonal vectors", d.SampleCosine)
	}
	if !strings.Contains(d.Verdict, "MOVED") {
		t.Fatalf("verdict = %q, want it to say the space moved", d.Verdict)
	}
	// The consequence is the part an operator needs: the weights are stale.
	if !strings.Contains(d.Verdict, "trained before this rebuild") {
		t.Fatalf("the verdict does not say what it invalidates: %q", d.Verdict)
	}
}

// TestAnAverageCanHideOppositeMovements is why there is a sample at all: two
// tags moving in opposite directions leave the centroid where it was.
func TestAnAverageCanHideOppositeMovements(t *testing.T) {
	before := EmbedRun{
		ID: 1, Dim: 2, MeanNorm: 1.0,
		// Non-zero, because a zero vector has no direction and cosine32
		// reports 0 for it — the fixture must not test that degenerate case by
		// accident.
		Centroid: []float32{0.5, 0},
		Sample: []EmbedSample{
			{TagID: 1, Vec: []float32{1, 0}},
			{TagID: 2, Vec: []float32{-1, 0}},
		},
	}
	after := EmbedRun{
		ID: 2, Dim: 2, MeanNorm: 1.0,
		Centroid: []float32{0.5, 0}, // unchanged
		Sample: []EmbedSample{
			{TagID: 1, Vec: []float32{0, 1}},  // moved 90 degrees
			{TagID: 2, Vec: []float32{0, -1}}, // moved the other way
		},
	}
	d := DriftBetween(before, after)
	if d.CentroidCosine < 0.999 {
		t.Fatalf("the fixture's centroids differ (cosine %.3f); the test is about a centroid that does NOT move",
			d.CentroidCosine)
	}
	if d.SampleCosine > 0.01 {
		t.Fatalf("sample cosine %.3f, want ~0: the per-tag positions are orthogonal to the old ones",
			d.SampleCosine)
	}
	if !strings.Contains(d.Verdict, "MOVED") {
		t.Fatalf("a centroid that held while every sampled tag rotated was reported as %q", d.Verdict)
	}
}

func TestADifferentWidthIsItsOwnVerdict(t *testing.T) {
	before := EmbedRun{ID: 1, Dim: 32, MeanNorm: 1, Centroid: make([]float32, 32), Sample: []EmbedSample{{TagID: 1, Vec: []float32{1}}}}
	after := EmbedRun{ID: 2, Dim: 64, MeanNorm: 1, Centroid: make([]float32, 64), Sample: []EmbedSample{{TagID: 1, Vec: []float32{1}}}}
	d := DriftBetween(before, after)
	if !strings.Contains(d.Verdict, "different width") {
		t.Fatalf("verdict = %q, want the width change named", d.Verdict)
	}
}

// TestRecordEmbedRunFingerprintsWhatIsStored: the fingerprint must describe the
// table, not a caller's copy of it, so this writes vectors and reads them back.
func TestRecordEmbedRunFingerprintsWhatIsStored(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Two vectors of width 2: (1,0) and (0,1).
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO embeddings (kind, entity_id, dim, vec) VALUES ('tag', '1', 2, ?)`,
		floatsToBlob([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO embeddings (kind, entity_id, dim, vec) VALUES ('tag', '2', 2, ?)`,
		floatsToBlob([]float32{0, 1})); err != nil {
		t.Fatal(err)
	}
	run, err := s.RecordEmbedRun(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if run.Rows != 2 || run.Dim != 2 {
		t.Fatalf("fingerprint says rows=%d dim=%d, want 2 and 2", run.Rows, run.Dim)
	}
	if run.MeanNorm < 0.99 || run.MeanNorm > 1.01 {
		t.Fatalf("mean norm %.3f, want 1.0 for two unit vectors", run.MeanNorm)
	}
	// The centroid of (1,0) and (0,1) is (0.5,0.5).
	if len(run.Centroid) != 2 || run.Centroid[0] < 0.49 || run.Centroid[0] > 0.51 {
		t.Fatalf("centroid = %v, want ~(0.5,0.5)", run.Centroid)
	}
	runs, err := s.EmbedRuns(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("EmbedRuns returned %+v", runs)
	}
	if len(runs[0].Sample) != 2 {
		t.Fatalf("sample has %d vectors, want both", len(runs[0].Sample))
	}
}

// floatsToBlob is the encoding the embeddings table stores: little-endian float32.
func floatsToBlob(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		bits := math.Float32bits(f)
		out[i*4] = byte(bits)
		out[i*4+1] = byte(bits >> 8)
		out[i*4+2] = byte(bits >> 16)
		out[i*4+3] = byte(bits >> 24)
	}
	return out
}
