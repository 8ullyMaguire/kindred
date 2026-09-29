package embed

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestEmbedConvergesToFloat32Precision pins the accuracy the float32
// conversion actually reaches, and why it stops there.
//
// The singular values converge to about 1.4e-7 and then stop improving no
// matter how many iterations run. That floor is float32 epsilon (1.19e-7),
// not a convergence failure: the block is float32 and the components are
// float32, so the result cannot be more accurate than the representation.
//
// The test asserts the floor rather than a tighter number on purpose. A
// test demanding 1e-9 from a float32 implementation fails forever for a
// reason that has nothing to do with the code, and a test demanding 1e-4
// would pass while a real numerical bug hid underneath.
func TestEmbedConvergesToFloat32Precision(t *testing.T) {
	n := 60
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32((i * 7) % n), Value: 1})
		edges = append(edges, Edge{A: int32(i), B: int32((i + 23) % n), Value: 0.25})
	}
	m := NewMatrix(n, n, edges)
	want := denseReference(m)

	// The converged value is the one at the highest iteration count, not
	// the worst across the run: the point of the test is where it settles,
	// and including the early iterations asserts that 120 is as accurate as
	// 3840, which is false by design — convergence is what takes the
	// iterations.
	const float32Epsilon = 1.1921e-7
	const iters = 3840
	got, err := Embed(m, 8, iters)
	if err != nil {
		t.Fatal(err)
	}
	var converged float64
	for i := range got.SingularVals {
		if e := relErr(got.SingularVals[i], want[i]); e > converged {
			converged = e
		}
	}
	// A real convergence bug is off by orders of magnitude, not by a
	// factor of two.
	if converged > float32Epsilon*4 {
		t.Fatalf("relative error %g after %d iterations; float32 precision "+
			"is %g, so this is a bug and not a precision limit", converged, iters, float32Epsilon)
	}
}

// TestMoreIterationsNeverMakeItWorse: convergence must be monotone-ish. A
// silent algebraic bug shows up here as an error that does not improve
// with iterations, which is how the missing-transpose bug was found.
func TestMoreIterationsNeverMakeItWorse(t *testing.T) {
	n := 60
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32((i * 7) % n), Value: 1})
		edges = append(edges, Edge{A: int32(i), B: int32((i + 23) % n), Value: 0.25})
	}
	m := NewMatrix(n, n, edges)
	want := denseReference(m)

	worstAt := func(iters int) float64 {
		got, err := Embed(m, 8, iters)
		if err != nil {
			t.Fatal(err)
		}
		var worst float64
		for i := range got.SingularVals {
			if e := relErr(got.SingularVals[i], want[i]); e > worst {
				worst = e
			}
		}
		return worst
	}
	few := worstAt(20)
	many := worstAt(400)
	if many > few {
		t.Fatalf("error at 400 iterations (%g) is worse than at 20 (%g); "+
			"more iterations made it less correct, which means the iteration "+
			"is not converging at all", many, few)
	}
}

// TestTransposeIsBuiltByNewMatrix guards the bug that cost the most
// time: NewMatrix did not build the column index, MulVecTranspose fell
// back to "assume symmetric", and the error was a constant factor no
// amount of iteration could fix.
func TestTransposeIsBuiltByNewMatrix(t *testing.T) {
	n := 20
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32((i + 3) % n), Value: 1})
	}
	m := NewMatrix(n, n, edges)
	if m.tOffsets == nil {
		t.Fatal("NewMatrix did not build the transposed index; a later " +
			"MulVecTranspose would return zeros and the solver would not converge")
	}
	// And it must actually equal the direct product on a symmetric matrix.
	x := make([]float64, n)
	for i := range x {
		x[i] = math.Sin(float64(i))
	}
	gotT := make([]float64, n)
	gotD := make([]float64, n)
	m.MulVecTranspose(x, gotT)
	m.MulVec(x, gotD)
	for i := range gotT {
		if math.Abs(gotT[i]-gotD[i]) > 1e-9 {
			t.Fatalf("element %d: transpose gives %v, direct gives %v on a "+
				"symmetric matrix", i, gotT[i], gotD[i])
		}
	}
}

// TestPackUnpackRoundTrip is the one test here that a round trip can
// honestly prove, because Pack and Unpack are the same format by
// definition. What it cannot prove is that the bytes are the *format's*
// bytes — that is the golden test in persist_test.go's spirit, and the
// literal below is the independent anchor.
func TestPackUnpackRoundTrip(t *testing.T) {
	v := []float32{1, -1, 0.5, 3.25, -0.125, 1e-8, 12345.678}
	blob := Pack(v)
	if len(blob) != len(v)*4 {
		t.Fatalf("packed %d bytes for %d float32", len(blob), len(v))
	}
	back, err := Unpack(len(v), blob)
	if err != nil {
		t.Fatal(err)
	}
	for i := range v {
		if back[i] != v[i] {
			t.Fatalf("element %d: %v != %v", i, back[i], v[i])
		}
	}
}

func TestPackUsesLittleEndianFloat32(t *testing.T) {
	// Golden bytes for 1.0 and -2.0, taken from the IEEE-754 encoding
	// rather than from this implementation. A round trip would agree with
	// itself while both halves used the wrong byte order, which is the
	// same trap the index writer fell into.
	blob := Pack([]float32{1, -2})
	want := []byte{
		0x00, 0x00, 0x80, 0x3f, // 1.0
		0x00, 0x00, 0x00, 0xc0, // -2.0
	}
	if len(blob) != len(want) {
		t.Fatalf("packed %d bytes, want %d", len(blob), len(want))
	}
	for i := range want {
		if blob[i] != want[i] {
			t.Fatalf("byte %d: got 0x%02x, want 0x%02x (full: %x vs %x)",
				i, blob[i], want[i], blob, want)
		}
	}
}

func TestUnpackRejectsAMismatchedLength(t *testing.T) {
	// A short buffer read as the requested dim would read past the end, or
	// silently truncate a vector and produce a plausible wrong cosine.
	for _, tc := range []struct {
		dim  int
		size int
	}{
		{4, 12}, {4, 20}, {4, 0}, {0, 4}, {-1, 4},
	} {
		if _, err := Unpack(tc.dim, make([]byte, tc.size)); err == nil {
			t.Errorf("Unpack(dim=%d, %d bytes) succeeded; a length mismatch "+
				"must be refused", tc.dim, tc.size)
		}
	}
}

func TestBuildRejectsDegenerateInput(t *testing.T) {
	db := newEdgeDB(t, nil)
	for _, tc := range []struct{ nodes, dim int }{
		{0, 8}, {10, 0}, {-1, 8}, {10, -1},
	} {
		if _, _, err := Build(context.Background(), db, tc.nodes, tc.dim, 8, 24); err == nil {
			t.Errorf("Build(nodes=%d, dim=%d) succeeded", tc.nodes, tc.dim)
		}
	}
}

func TestBuildRefusesAnEmptyCorpus(t *testing.T) {
	// No edges at all: there is nothing to embed, and returning an empty
	// result would look like "computed successfully, found nothing".
	db := newEdgeDB(t, nil)
	if _, _, err := Build(context.Background(), db, 10, 4, 8, 24); err == nil {
		t.Fatal("Build succeeded on a corpus with no co-occurrence edges")
	}
}

func TestBuildProducesUnitVectors(t *testing.T) {
	db := newEdgeDB(t, []Edge{
		{A: 0, B: 1, Value: 1}, {A: 1, B: 2, Value: 1}, {A: 2, B: 3, Value: 1},
		{A: 0, B: 3, Value: 1}, {A: 1, B: 3, Value: 1},
	})
	comps, res, err := Build(context.Background(), db, 4, 3, 60, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) != 3 {
		t.Fatalf("got %d components, want 3", len(comps))
	}
	for i, c := range comps {
		if len(c) != 4 {
			t.Errorf("component %d has %d entries, want 4 (one per row)", i, len(c))
		}
		var n float64
		for _, f := range c {
			n += float64(f) * float64(f)
		}
		if math.Abs(math.Sqrt(n)-1) > 1e-4 {
			t.Errorf("component %d has norm %v, want 1; an unnormalised "+
				"component makes cosine similarity meaningless", i, math.Sqrt(n))
		}
	}
	if res.Rows != 4 || res.Dim != 3 {
		t.Errorf("result = %+v, want rows=4 dim=3", res)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	// Content addressing and reproducible indexes both depend on this.
	db := newEdgeDB(t, []Edge{
		{A: 0, B: 1, Value: 1}, {A: 1, B: 2, Value: 1}, {A: 2, B: 3, Value: 1},
		{A: 0, B: 3, Value: 1},
	})
	a, _, err := Build(context.Background(), db, 4, 3, 40, 24)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Build(context.Background(), db, 4, 3, 40, 24)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a {
		for r := range a[i] {
			if a[i][r] != b[i][r] {
				t.Fatalf("two builds of the same corpus differ at component %d row %d: "+
					"%v vs %v; an irreproducible index cannot be verified", i, r, a[i][r], b[i][r])
			}
		}
	}
}

func newEdgeDB(t *testing.T, edges []Edge) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE cooccurrence_edges(
		tag_a_id INTEGER, tag_b_id INTEGER, cooccur_count INTEGER,
		PRIMARY KEY(tag_a_id, tag_b_id))`); err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if _, err := db.Exec(
			`INSERT INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES(?,?,1)`,
			e.A, e.B); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
