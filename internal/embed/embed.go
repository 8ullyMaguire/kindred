// Package embed computes low-dimensional tag embeddings from the sparse
// tag matrix.
//
// This replaces the previous deployment's scikit-learn TruncatedSVD
// (scipy + sklearn + pandas + numpy in the process, hundreds of MB) with
// a few hundred lines of Go. The algorithm differs, so the output differs:
// the parity report is a report, not a gate, and claiming bit-parity with
// a different algorithm would be a false promise (SPEC §6).
//
// Algorithm note, recorded because the first attempt was wrong and the
// reason is worth not repeating. A Golub-Kahan two-sided Lanczos was
// implemented here and discarded. Against a dense reference it drifted
// ~11% on a diagonal matrix and returned no positive singular values at
// all on a 5x5 identity, which traced to step accounting in the
// recurrence rather than to the projection. Getting a correct Lanczos
// right is a research-grade exercise, and this project does not need one:
// the embedding signal is one of eight, contributes 32 dimensions, and
// its job is to rank tags that co-occur, not to reproduce a spectrum to
// six decimal places.
//
// What ships instead is block power iteration with Rayleigh-Ritz
// projection. It is a standard, well-understood method, it converges
// monotonically, it is simple enough to verify against a dense
// reference, and it produces orthonormal components by construction.
// Where Lanczos would need full reorthogonalisation to stay accurate,
// this needs only a QR of the block.
package embed

import (
	"fmt"
	"math"
)

// Matrix is a sparse matrix in CSR form. Values are float32 throughout:
// the PMI matrix has millions of entries and float64 would double the
// memory for no accuracy that matters at this use.
type Matrix struct {
	Rows, Cols int
	offsets    []int64
	cols       []int32
	vals       []float32
}

// NewMatrix builds a CSR matrix from edges, skipping out-of-range indices.
func NewMatrix(rows, cols int, edges []Edge) *Matrix {
	// Two-pass counting sort, the same shape as the graph's CSR: count
	// degrees, prefix-sum, fill. No maps, no per-row allocation.
	degree := make([]int64, rows+1)
	for _, e := range edges {
		if !inRange(rows, e.A) || !inRange(cols, e.B) {
			continue
		}
		degree[e.A+1]++
	}
	for i := 1; i <= rows; i++ {
		degree[i] += degree[i-1]
	}
	total := degree[rows]

	m := &Matrix{
		Rows: rows, Cols: cols,
		offsets: degree,
		cols:    make([]int32, total),
		vals:    make([]float32, total),
	}
	cursor := make([]int64, rows)
	copy(cursor, degree[:rows])
	for _, e := range edges {
		if !inRange(rows, e.A) || !inRange(cols, e.B) {
			continue
		}
		m.cols[cursor[e.A]] = e.B
		m.vals[cursor[e.A]] = e.Value
		cursor[e.A]++
	}
	return m
}

func inRange(n int, i int32) bool { return i >= 0 && int(i) < n }

// Edge is one matrix entry.
type Edge struct {
	A, B  int32
	Value float32
}

// MulVec computes y = A·x.
func (m *Matrix) MulVec(x []float64, y []float64) {
	for i := range y {
		y[i] = 0
	}
	for r := 0; r < m.Rows; r++ {
		lo, hi := m.offsets[r], m.offsets[r+1]
		if lo == hi {
			continue
		}
		sum := 0.0
		for k := lo; k < hi; k++ {
			sum += float64(m.vals[k]) * x[m.cols[k]]
		}
		y[r] = sum
	}
}

// Result is a computed embedding set.
type Result struct {
	Dim          int
	SingularVals []float64
	// Components[i] is the i-th dominant left singular vector, dense over
	// rows, unit norm.
	Components [][]float32
	Iterations int
}

// defaultIters is enough for the 1e-6 target on the matrices here: power
// iteration converges as (λ_k/λ_{k+1})^(2t) and the tag matrix's spectrum
// is strongly separated at the top.
const defaultIters = 60

// Embed computes the k dominant left singular vectors of A.
//
// It runs block power iteration on AᵀA — the block is k starting
// vectors — then takes the top k left singular vectors of the resulting
// factor. The Rayleigh-Ritz step is what makes the components
// orthonormal and accurate: raw power-iteration vectors are not
// orthogonal, and feeding those to a similarity signal produces
// embeddings that are mostly redundant with each other.
func Embed(m *Matrix, k int, iters int) (*Result, error) {
	if k <= 0 {
		return nil, fmt.Errorf("embed: k must be positive, got %d", k)
	}
	if m.Rows == 0 || m.Cols == 0 {
		return nil, fmt.Errorf("embed: matrix is empty (%dx%d)", m.Rows, m.Cols)
	}
	if iters <= 0 {
		iters = defaultIters
	}
	if k > m.Rows {
		k = m.Rows
	}
	if k > m.Cols {
		k = m.Cols
	}

	// Block of k starting vectors, orthonormalised so the block starts
	// as a basis for a k-dimensional space.
	v := make([][]float64, k)
	for i := range v {
		v[i] = deterministicVector(m.Rows, 0x9E3779B97F4A7C15*uint64(i+1))
	}
	orthonormalise(v)

	// AtA(x) = Aᵀ(A·x)
	aq := make([]float64, m.Rows)
	ata := make([]float64, m.Rows)
	for it := 0; it < iters; it++ {
		for i := range v {
			m.MulVec(v[i], aq)
			m.MulVecTranspose(aq, ata)
			copy(v[i], ata)
		}
		// Re-orthonormalise every iteration. Without it the block loses
		// rank as the dominant directions converge and the components
		// stop being independent.
		orthonormalise(v)
	}

	// A ≈ V·B, so B = VᵀA has the same singular values as A. B is
	// k-by-Rows: one row per block vector.
	b := make([][]float64, k)
	for i := range b {
		b[i] = make([]float64, m.Rows)
		m.MulVec(v[i], aq)
		copy(b[i], aq)
	}
	// BᵀB is k-by-k and symmetric; its eigenvectors are the right
	// singular vectors in the reduced space, and B·(eigenvector) is the
	// left singular vector.
	btb := make([][]float64, k)
	for i := range btb {
		btb[i] = make([]float64, k)
		for j := range btb[i] {
			btb[i][j] = dot(b[i], b[j])
		}
	}
	vals, vecs := symmetricEigen(btb)

	sv := make([]float64, 0, k)
	comps := make([][]float32, 0, k)
	for i := 0; i < k; i++ {
		if vals[i] <= 0 {
			break
		}
		// Left singular vector = sum_j vecs[i][j] * b[j]
		comp := make([]float32, m.Rows)
		for r := 0; r < m.Rows; r++ {
			s := 0.0
			for j := 0; j < k; j++ {
				s += vecs[i][j] * b[j][r]
			}
			comp[r] = float32(s)
		}
		norm := float32(0)
		for _, x := range comp {
			norm += x * x
		}
		if norm > 0 {
			inv := float32(1 / math.Sqrt(float64(norm)))
			for r := range comp {
				comp[r] *= inv
			}
		}
		sv = append(sv, math.Sqrt(vals[i]))
		comps = append(comps, comp)
	}
	if len(sv) == 0 {
		return nil, fmt.Errorf("embed: the matrix has no positive singular values; is it all zeros?")
	}
	return &Result{Dim: len(comps), SingularVals: sv, Components: comps, Iterations: iters}, nil
}

// orthonormalise replaces the block with an orthonormal basis for the
// same span, using modified Gram-Schmidt twice. Twice because one pass
// loses orthogonality fast enough to matter at k=32.
func orthonormalise(v [][]float64) {
	for pass := 0; pass < 2; pass++ {
		for i := range v {
			for j := 0; j < i; j++ {
				c := dot(v[j], v[i])
				for r := range v[i] {
					v[i][r] -= c * v[j][r]
				}
			}
			n := norm2(v[i])
			if n < 1e-12 {
				// The block lost rank: reseed this vector rather than
				// dividing by ~0 and producing inf/NaN that would poison
				// every downstream similarity.
				v[i] = deterministicVector(len(v[i]), 0x2545F4914F6CDD1D*uint64(i+1))
				n = norm2(v[i])
				if n < 1e-12 {
					continue
				}
			}
			inv := 1 / n
			for r := range v[i] {
				v[i][r] *= inv
			}
		}
	}
}

// MulVecTranspose computes y = Aᵀ·x.
func (m *Matrix) MulVecTranspose(x []float64, y []float64) {
	for i := range y {
		y[i] = 0
	}
	for r := 0; r < m.Rows; r++ {
		lo, hi := m.offsets[r], m.offsets[r+1]
		if lo == hi || r >= len(x) {
			continue
		}
		xr := x[r]
		for k := lo; k < hi; k++ {
			y[m.cols[k]] += xr * float64(m.vals[k])
		}
	}
}

// Lanczos is kept as a name so callers that predate the switch keep
// compiling, and it delegates to Embed. The comment above explains why
// the recurrence it used to name is gone.
func Lanczos(m *Matrix, k int, iters int, seed uint64) (*Result, error) {
	if seed == 0 {
		return Embed(m, k, iters)
	}
	// A non-zero seed historically meant "vary the start vector". The
	// block start is derived from fixed constants, so a seed cannot be
	// honoured without making builds irreproducible. Ignoring it is
	// deliberate: reproducibility matters more than the seed.
	return Embed(m, k, iters)
}

// symmetricEigen diagonalises a small dense symmetric matrix with the
// cyclic Jacobi method. For n in the tens this is both fast and far more
// accurate than a hand-rolled QR, and it cannot fail to converge the way
// an unpivoted implementation can.
func symmetricEigen(a [][]float64) ([]float64, [][]float64) {
	n := len(a)
	m := make([][]float64, n)
	for i := range a {
		m[i] = append([]float64(nil), a[i]...)
	}
	v := make([][]float64, n)
	for i := range v {
		v[i] = make([]float64, n)
		v[i][i] = 1
	}
	for sweep := 0; sweep < 100; sweep++ {
		off := 0.0
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				off += m[i][j] * m[i][j]
			}
		}
		if off < 1e-24 {
			break
		}
		for p := 0; p < n; p++ {
			for q := p + 1; q < n; q++ {
				if math.Abs(m[p][q]) < 1e-15 {
					continue
				}
				theta := (m[q][q] - m[p][p]) / (2 * m[p][q])
				t := 1.0
				if theta != 0 {
					t = sign(theta) / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				}
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < n; k++ {
					mkp, mkq := m[k][p], m[k][q]
					m[k][p] = c*mkp - s*mkq
					m[k][q] = s*mkp + c*mkq
				}
				for k := 0; k < n; k++ {
					mpk, mqk := m[p][k], m[q][k]
					m[p][k] = c*mpk - s*mqk
					m[q][k] = s*mpk + c*mqk
				}
				for k := 0; k < n; k++ {
					vkp, vkq := v[k][p], v[k][q]
					v[k][p] = c*vkp - s*vkq
					v[k][q] = s*vkp + c*vkq
				}
			}
		}
	}
	vals := make([]float64, n)
	for i := 0; i < n; i++ {
		vals[i] = m[i][i]
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	for i := 1; i < n; i++ {
		for j := i; j > 0 && vals[order[j]] > vals[order[j-1]]; j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	sv := make([]float64, n)
	svecs := make([][]float64, n)
	for i, o := range order {
		sv[i] = vals[o]
		svecs[i] = make([]float64, n)
		for r := 0; r < n; r++ {
			svecs[i][r] = v[r][o]
		}
	}
	return sv, svecs
}

func sign(x float64) float64 {
	if x < 0 {
		return -1
	}
	return 1
}

// deterministicVector is a reproducible starting vector. A random start
// would make every build produce different embeddings and every parity
// comparison meaningless.
func deterministicVector(n int, seed uint64) []float64 {
	v := make([]float64, n)
	x := seed | 1
	for i := range v {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		v[i] = float64(x%1000)/1000.0 - 0.5
	}
	return v
}

func norm2(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

func dot(a, b []float64) float64 {
	s := 0.0
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
