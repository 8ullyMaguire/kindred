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
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestRecordEmbedRunWithMoreRowsThanItsSample is the regression test for a
// deadlock that only the LIVE table could show.
//
// The pool holds one connection. A query whose Rows is not exhausted holds that
// connection, and the next query waits for it forever. With two fixture rows and
// a sample of eight, `rows.Next()` returns false and the Rows closes itself — so
// the small fixture passed while 634k rows against a sample of 64 hung the
// command for seven minutes at 0.5% CPU. The fixture therefore has MORE rows
// than the sample, which is the only shape that reaches the `break`.
func TestRecordEmbedRunWithMoreRowsThanItsSample(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 12; i++ {
		vec := []float32{1, 0}
		if err := func() error {
			_, err := s.DB.ExecContext(ctx,
				`INSERT INTO embeddings (kind, entity_id, dim, vec) VALUES ('tag', ?, 2, ?)`,
				strconv.Itoa(i+1), floatsToBlob(vec))
			return err
		}(); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	var run EmbedRun
	var rerr error
	go func() {
		run, rerr = s.RecordEmbedRun(ctx, 4) // 4 of 12: the break is reached
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("RecordEmbedRun did not finish with more rows than its sample: the sample query's " +
			"Rows is still holding the only connection, so the centroid query waits on it forever")
	}
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(run.Sample) != 4 {
		t.Fatalf("sample has %d vectors, want the 4 asked for", len(run.Sample))
	}
	if run.Rows != 12 {
		t.Fatalf("fingerprint says the table holds %d rows, want 12: the sample asked for 4 of 12, "+
			"and recording the sample's extent as the table's size makes the drift report's own "+
			"row column a lie", run.Rows)
	}
}

// TestTagsWithNoVectorDoNotCountAsDrift is the defect the live table found.
//
// This table records "no embedding for this tag" as an all-zero vector. Two of
// those have no angle, and scoring them 0.0 drags the mean down: the deployed
// table has 3 such tags among its first 64, which turned an honest 1.0000 into
// 0.9531 and made a build compared with ITSELF report "the space MOVED".
func TestTagsWithNoVectorDoNotCountAsDrift(t *testing.T) {
	zero := make([]float32, 4)
	before := EmbedRun{
		ID: 1, Dim: 4, MeanNorm: 1,
		Centroid: []float32{0.1, 0.2, 0.3, 0.4},
		Sample: []EmbedSample{
			{TagID: 1, Vec: []float32{1, 0, 0, 0}},
			{TagID: 2, Vec: zero},
			{TagID: 3, Vec: zero},
		},
	}
	after := before
	after.ID = 2
	d := DriftBetween(before, after)
	if d.SampleCosine < 0.999 {
		t.Fatalf("identical runs compared at cosine %.4f because zero vectors were scored 0.0", d.SampleCosine)
	}
	if d.SampleN != 1 {
		t.Fatalf("sample n = %d, want 1 compared tag (the two without vectors carry no position)", d.SampleN)
	}
	if d.EmptySample != 2 {
		t.Fatalf("empty sample = %d, want 2: dropped tags must be counted, not silently ignored", d.EmptySample)
	}
	if !strings.Contains(d.Verdict, "carry no vector") {
		t.Fatalf("verdict = %q, want the excluded tags named", d.Verdict)
	}
}
