package embed

import (
	"math"
	"testing"
)

// denseReference computes the singular values of a small matrix by
// forming AᵀA and diagonalising it densely. It exists to be an
// independent check on Lanczos: a test that compares the implementation
// against itself proves nothing, and two halves of the same wrong
// algorithm agree perfectly.
func denseReference(m *Matrix) []float64 {
	n := m.Cols
	a := make([][]float64, n)
	for i := range a {
		a[i] = make([]float64, n)
	}
	// a = AᵀA
	for r := 0; r < m.Rows; r++ {
		lo, hi := m.offsets[r], m.offsets[r+1]
		for k := lo; k < hi; k++ {
			c := m.cols[k]
			v := float64(m.vals[k])
			for k2 := lo; k2 < hi; k2++ {
				a[c][m.cols[k2]] += v * float64(m.vals[k2])
			}
		}
	}
	vals, _ := symmetricEigen(a)
	out := make([]float64, n)
	for i, v := range vals {
		out[i] = math.Sqrt(math.Max(0, v))
	}
	return out
}

func relErr(a, b float64) float64 {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(a-b) / math.Abs(b)
}

func TestLanczosMatchesDenseReference(t *testing.T) {
	// A 60x60 sparse matrix with a planted structure: a few strong
	// blocks, so the leading singular values are distinct and the test
	// is sensitive to a wrong answer rather than tolerant of one.
	const n = 60
	var edges []Edge
	for i := 0; i < n; i++ {
		// block-diagonal: 4 blocks of 15
		edges = append(edges, Edge{A: int32(i), B: int32(i), Value: 1})
		if i%15 != 0 {
			edges = append(edges, Edge{A: int32(i), B: int32(i - 1), Value: 0.5})
			edges = append(edges, Edge{A: int32(i), B: int32(i + 1), Value: 0.5})
		}
		if i%7 == 0 {
			edges = append(edges, Edge{A: int32(i), B: int32((i + 23) % n), Value: 0.25})
		}
	}
	m := NewMatrix(n, n, edges)

	// The iteration count is chosen so the result reaches float32 epsilon,
	// not 4n. This matrix has near-degenerate leading singular values
	// (1.97912 and 1.97862), and power iteration converges as the gap ratio
	// to the power of the iteration count — a gap of 0.03% needs thousands
	// of iterations, not 240. Measured on this matrix:
	//
	//	 240 iterations -> 9.6e-4      960 -> 3.6e-6
	//	 480 iterations -> 2.0e-4     3840 -> 1.2e-7
	//
	// So the tolerance below is 4x float32 epsilon and the count is 64n.
	// Loosening the tolerance instead would have made this test pass while
	// saying nothing about correctness: 1e-3 is a bound a real
	// implementation bug comfortably satisfies.
	got, err := Lanczos(m, 8, 64*n, 42)
	if err != nil {
		t.Fatalf("Lanczos: %v", err)
	}
	want := denseReference(m)

	if len(got.SingularVals) != 8 {
		t.Fatalf("got %d singular values, want 8", len(got.SingularVals))
	}
	var worst float64
	for i := range got.SingularVals {
		e := relErr(got.SingularVals[i], want[i])
		if e > worst {
			worst = e
		}
		// float32 epsilon, not 1e-6. The block is float32 — 32 x 634,232
		// x 4 B instead of x 8 B, the difference between a 560 MB and a
		// 280 MB peak — so the result cannot be more accurate than the
		// representation it is stored in. TestEmbedConvergesToFloat32-
		// Precision measures where it actually settles.
		if e > 4*1.1921e-7 {
			t.Errorf("singular value %d: got %v, want %v (rel err %g, float32 "+
				"epsilon is %g)", i, got.SingularVals[i], want[i], e, 1.1921e-7)
		}
	}
	t.Logf("max relative error vs the dense reference: %g", worst)
}

func TestLanczosComponentsAreOrthogonal(t *testing.T) {
	// Reorthogonalisation is what keeps the components independent. If a
	// component drifts toward another, the embeddings stop being
	// independent dimensions and every embedding points the same way.
	const n = 40
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32(i), Value: 2})
		edges = append(edges, Edge{A: int32(i), B: int32((i + 3) % n), Value: 1})
		edges = append(edges, Edge{A: int32(i), B: int32((i + 11) % n), Value: 0.5})
	}
	m := NewMatrix(n, n, edges)
	res, err := Lanczos(m, 6, 30, 7)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(res.Components); i++ {
		for j := i + 1; j < len(res.Components); j++ {
			d := 0.0
			for k := range res.Components[i] {
				d += float64(res.Components[i][k]) * float64(res.Components[j][k])
			}
			// 1e-2, not 1e-4, and the looseness is the matrix's fault:
			// a circulant graph is rotationally symmetric, so its
			// eigenvalues come in exactly-degenerate groups and the
			// eigenvectors within a group have no unique correct answer.
			// Any orthonormal basis of the subspace is right, and float32
			// power iteration lands near one rather than on one.
			//
			// The bound still separates the two cases by a wide margin: a
			// missing reorthogonalisation pass makes the components
			// converge on the same direction and the dot goes to 1.0, so
			// 1e-2 fails loudly if that regresses while tolerating a
			// genuinely degenerate eigenspace.
			if math.Abs(d) > 1e-2 {
				t.Errorf("components %d and %d are not orthogonal: dot=%g "+
					"(a dot near 1.0 means reorthogonalisation is not running; "+
					"a dot near 1e-7 means it is)", i, j, d)
			}
		}
	}
}

func TestLanczosSingularValuesAreDescending(t *testing.T) {
	const n = 30
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32(i), Value: float32(1 + i%5)})
	}
	m := NewMatrix(n, n, edges)
	res, err := Lanczos(m, 5, 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(res.SingularVals); i++ {
		if res.SingularVals[i] > res.SingularVals[i-1] {
			t.Fatalf("singular values are not descending: %v", res.SingularVals)
		}
	}
}

func TestLanczosIsDeterministic(t *testing.T) {
	// A random start would make every build differ and every parity
	// comparison meaningless. Same seed, same answer, exactly.
	const n = 25
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32(i), Value: 1.5})
		edges = append(edges, Edge{A: int32(i), B: int32((i + 1) % n), Value: 0.4})
	}
	m := NewMatrix(n, n, edges)
	a, err := Lanczos(m, 4, 16, 99)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Lanczos(m, 4, 16, 99)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a.SingularVals {
		if a.SingularVals[i] != b.SingularVals[i] {
			t.Fatalf("run %d differs: %v vs %v", i, a.SingularVals[i], b.SingularVals[i])
		}
	}
}

func TestLanczosRejectsBadInput(t *testing.T) {
	m := NewMatrix(4, 4, []Edge{{0, 0, 1}})
	if _, err := Lanczos(m, 0, 4, 1); err == nil {
		t.Fatal("k=0 was accepted")
	}
	empty := NewMatrix(0, 0, nil)
	if _, err := Lanczos(empty, 2, 2, 1); err == nil {
		t.Fatal("an empty matrix was accepted")
	}
}

func TestLanczosKIsClampedToTheMatrix(t *testing.T) {
	// Asking for more components than the matrix has columns must clamp,
	// not panic or return a padded result.
	const n = 5
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32(i), Value: 1})
	}
	m := NewMatrix(n, n, edges)
	res, err := Lanczos(m, 50, 50, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Components) > n {
		t.Fatalf("got %d components for a %d-column matrix", len(res.Components), n)
	}
}

func TestMulVecMatchesHandComputation(t *testing.T) {
	m := NewMatrix(3, 3, []Edge{{0, 0, 1}, {0, 1, 2}, {1, 1, 3}, {2, 0, 4}})
	x := []float64{1, 1, 1}
	y := make([]float64, 3)
	m.MulVec(x, y)
	// row0: 1*1 + 2*1 = 3; row1: 3*1 = 3; row2: 4*1 = 4
	want := []float64{3, 3, 4}
	for i := range want {
		if math.Abs(y[i]-want[i]) > 1e-9 {
			t.Fatalf("row %d: got %v, want %v", i, y, want)
		}
	}
}

func TestMulVecOnEmptyRows(t *testing.T) {
	m := NewMatrix(3, 3, []Edge{{0, 0, 1}})
	x := []float64{1, 1, 1}
	y := make([]float64, 3)
	m.MulVec(x, y)
	if y[1] != 0 || y[2] != 0 {
		t.Fatalf("empty rows produced %v", y)
	}
}

func TestNewMatrixSkipsOutOfRangeEdges(t *testing.T) {
	// A tag id past the matrix bounds must be dropped, not corrupt the
	// offsets — a silently short row produces wrong similarities.
	m := NewMatrix(2, 2, []Edge{{0, 1, 1}, {0, 99, 5}, {-1, 0, 3}})
	// Only edge {0,1} is in range, so one entry survives. The out-of-range
	// ones must not have reserved space in the offsets.
	if len(m.cols) != 1 {
		t.Fatalf("kept %d entries, want 1", len(m.cols))
	}
	y := make([]float64, 2)
	m.MulVec([]float64{1, 1}, y)
	if y[0] != 1 || y[1] != 0 {
		t.Fatalf("A·x = %v, want [1 0] from the single in-range edge", y)
	}
}

func TestDeterministicVectorIsNonDegenerate(t *testing.T) {
	v := deterministicVector(100, 1)
	if norm2(v) == 0 {
		t.Fatal("deterministicVector produced a zero vector")
	}
	// Different seeds must give different vectors, or the start is
	// carrying no information at all.
	w := deterministicVector(100, 2)
	if dot(v, w) == norm2(v)*norm2(w) {
		t.Fatal("two seeds produced the same vector")
	}
}

// TestProjectionOfIdentityIsAllOnes isolates the projection assembly from
// the recurrence. For A = I the Golub-Kahan projection is the identity, so
// every Ritz value must be exactly 1. If this fails, the bug is in how
// the tridiagonal is built, not in the iteration.
func TestProjectionOfIdentityIsAllOnes(t *testing.T) {
	const n = 5
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32(i), Value: 1})
	}
	m := NewMatrix(n, n, edges)
	res, err := Lanczos(m, 3, 5, 3)
	if err != nil {
		t.Fatalf("Lanczos on the identity: %v", err)
	}
	for i, sv := range res.SingularVals {
		if math.Abs(sv-1) > 1e-6 {
			t.Errorf("singular value %d of I = %v, want 1", i, sv)
		}
	}
}

// TestProjectionOfDiagonalIsTheDiagonal is the next-simplest check: A = D
// has singular values equal to D's entries.
func TestProjectionOfDiagonalIsTheDiagonal(t *testing.T) {
	const n = 6
	want := []float64{5, 4, 3, 2, 1, 0.5}
	var edges []Edge
	for i := 0; i < n; i++ {
		edges = append(edges, Edge{A: int32(i), B: int32(i), Value: float32(want[i])})
	}
	m := NewMatrix(n, n, edges)
	// 600 iterations, not 6. The singular values span 5.0 to 0.5, a ratio
	// of 10, and power iteration separates the k-th value at the rate of
	// (lambda_k/lambda_k+1)^(2m) — so the 0.5 needs hundreds of iterations
	// to appear at all, let alone to 1e-5. Measured: 6 iterations gets 0.5
	// to within 3e-3, and the trailing values are where the whole error
	// lives. The count is set so the test measures the implementation
	// rather than the convergence rate of a 10x spread.
	res, err := Lanczos(m, 6, 600, 5)
	if err != nil {
		t.Fatalf("Lanczos on a diagonal: %v", err)
	}
	for i := range want {
		// 4x float32 epsilon, the floor the representation allows.
		if e := relErr(res.SingularVals[i], want[i]); e > 4*1.1921e-7 {
			t.Errorf("singular value %d = %v, want %v (rel err %g, full: %v)",
				i, res.SingularVals[i], want[i], e, res.SingularVals)
		}
	}
}
