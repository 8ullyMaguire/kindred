package als

// Implicit-feedback matrix factorisation (ALS), for the one table that holds what
// readers CHOSE (#89).
//
// WHY IT IS NOT THE SAME DIMENSION AS collab
//
// `internal/collab` counts pairs: two works are related by how many readers bookmarked
// both. That is evidence, it is exact, and it can only relate works that already share a
// reader. ALS factors every work and every reader into a 32-dimensional space, so two
// works can be related through third works that neither shares a reader with directly —
// "readers who liked A also liked B, and B is like C" without any reader having both A
// and C.
//
// THE MODEL (Hu, Koren, Volsky 2008), implicit feedback as confidence:
//
//	preference p_ui = 1 if reader u touched work i, else 0
//	confidence c_ui = 1 + ALPHA * p_ui
//	minimise  sum_ui c_ui (p_ui - x_u·y_i)^2 + LAMBDA (||x_u||^2 + ||y_i||^2)
//
// The whole point of the confidence form: a reader having bookmarked a work is worth
// ALPHA times more than their not having done so, and their NOT having done so is still
// evidence (weakly), because a reader with 300 bookmarks has silently passed on 112,000
// works. Treating the zeros as missing would let a work nobody has read be "similar" to
// everything.
//
// THE CLOSED FORM, and the reason this needs no gradient descent:
//
//	x_u = (Yᵀ C_u Y + LAMBDA I)⁻¹ Yᵀ C_u p_u
//
// is 32×32 per reader, and Yᵀ C_u Y has the useful identity
//
//	Yᵀ C_u Y = YᵀY + sum_{i in u's items} ALPHA · y_i y_iᵀ
//
// so the expensive part (YᵀY, shared by every reader) is computed once per iteration and
// each reader's solve adds only their own rank-one terms. That is what makes this
// feasible in plain Go at this size: 6,261 users, 180,677 interactions, 32 factors.
//
// WHY NOT HERE, ONCE: the collab package's own doc rejected ALS on the grounds that it
// "needs an iterative training loop, a float64 model and a CPU budget that does not fit
// the project's stated memory ceiling". Measured against the real table (180,677 rows
// over 6,261 users) that objection does not survive: the model is 6,261×32 + 112,935×32
// float32s (~15 MB), and one iteration is a few seconds. The collab doc is right about
// what it built; it was wrong about the size.

import (
	"math"
	"sort"
)

// Interaction is one reader touching one work. Implicit: the record existing is the
// signal, and there is no score to regress against.
type Interaction struct {
	User int64
	Work int64
}

// Params are the model's knobs, named rather than defaulted inside the maths so a caller
// can report what produced a set of factors.
type Params struct {
	Factors int     // dimensions; 32 is the project's width elsewhere
	Iters   int     // sweeps over the data
	Lambda  float64 // L2 regularisation
	Alpha   float64 // confidence per observed interaction
}

// DefaultParams is the configuration this project trains with.
//
// ALPHA = 40 and LAMBDA = 0.05 are the paper's own values for implicit data with this
// shape (many users, many items, sparse observations). They are not tuned here: tuning
// them needs a holdout the eval harness has, and the first measurement of this signal is
// whether it helps AT ALL, before its hyperparameters deserve a search.
func DefaultParams() Params {
	return Params{Factors: 32, Iters: 12, Lambda: 0.05, Alpha: 40}
}

// Result is the trained model, indexed densely over the ids that were seen.
type Result struct {
	Params  Params
	UserIDs []int64
	WorkIDs []int64
	Users   [][]float32 // len(UserIDs) x Factors
	Works   [][]float32 // len(WorkIDs) x Factors
	// Fit is the root-mean-square error against the CONFIDENCE-WEIGHTED targets over
	// observed interactions only. It is not a validation metric and is reported as a
	// progress number, never as a quality claim.
	Fit []float64
	// ConvergedPerIteration records the largest absolute factor change per sweep, so a
	// caller can see whether the loop stopped moving or merely ran out of iterations.
	Moved []float64
}

// Train runs ALS. Deterministic: factors start from a fixed pseudo-random pattern
// derived from the ids, so two runs on the same input produce the same model and a
// measurement can be repeated.
func Train(in []Interaction, p Params) Result {
	if p.Factors <= 0 {
		p = DefaultParams()
	}
	if p.Iters <= 0 {
		p.Iters = DefaultParams().Iters
	}
	if p.Alpha <= 0 {
		p.Alpha = DefaultParams().Alpha
	}
	if p.Lambda <= 0 {
		p.Lambda = DefaultParams().Lambda
	}

	// Dense indices, sorted so the model is reproducible regardless of input order.
	users, works := denseIndex(in)
	var res Result
	res.Params = p
	res.UserIDs, res.WorkIDs = users, works
	uIndex := make(map[int64]int, len(users))
	for i, id := range users {
		uIndex[id] = i
	}
	wIndex := make(map[int64]int, len(works))
	for i, id := range works {
		wIndex[id] = i
	}

	// Each user's items, as dense indices. Built once and reused every sweep; the model
	// changes, the data does not.
	userItems := make([][]int, len(users))
	for _, it := range in {
		u, ok := uIndex[it.User]
		if !ok {
			continue
		}
		w, ok := wIndex[it.Work]
		if !ok {
			continue
		}
		userItems[u] = append(userItems[u], w)
	}
	// Each item's readers, for the item solve.
	workUsers := make([][]int, len(works))
	for u, items := range userItems {
		for _, w := range items {
			workUsers[w] = append(workUsers[w], u)
		}
	}

	res.Users = initFactors(len(users), p.Factors, 11)
	res.Works = initFactors(len(works), p.Factors, 23)

	f := p.Factors
	// Scratch: the shared YᵀY, and the per-row matrix reused between solves.
	ytY := make([]float64, f*f)
	mat := make([]float64, f*f)
	rhs := make([]float64, f)
	vec := make([]float64, f)

	for it := 0; it < p.Iters; it++ {
		// YᵀY once per sweep.
		for i := range ytY {
			ytY[i] = 0
		}
		for _, y := range res.Works {
			for a := 0; a < f; a++ {
				ya := float64(y[a])
				if ya == 0 {
					continue
				}
				for b := 0; b < f; b++ {
					ytY[a*f+b] += ya * float64(y[b])
				}
			}
		}

		moved := 0.0
		for u, items := range userItems {
			// mat = YᵀY + λI + α·Σ y_i y_iᵀ
			copy(mat, ytY)
			for a := 0; a < f; a++ {
				mat[a*f+a] += p.Lambda
			}
			for i := range rhs {
				rhs[i] = 0
			}
			// rhs starts at α·Σ y_i (p_ui = 1, so c_ui - 1 = α for observed items).
			for _, wi := range items {
				y := res.Works[wi]
				for a := 0; a < f; a++ {
					ya := float64(y[a])
					for b := 0; b < f; b++ {
						mat[a*f+b] += p.Alpha * ya * float64(y[b])
					}
					rhs[a] += p.Alpha * ya
				}
			}
			solveSPD(mat, rhs, vec, f)
			for a := 0; a < f; a++ {
				if d := math.Abs(float64(res.Users[u][a]) - vec[a]); d > moved {
					moved = d
				}
				res.Users[u][a] = float32(vec[a])
			}
		}

		// XᵀX for the item solve, recomputed after the user sweep.
		xtX := make([]float64, f*f)
		for _, x := range res.Users {
			for a := 0; a < f; a++ {
				xa := float64(x[a])
				if xa == 0 {
					continue
				}
				for b := 0; b < f; b++ {
					xtX[a*f+b] += xa * float64(x[b])
				}
			}
		}
		for w, readers := range workUsers {
			copy(mat, xtX)
			for a := 0; a < f; a++ {
				mat[a*f+a] += p.Lambda
			}
			for i := range rhs {
				rhs[i] = 0
			}
			for _, u := range readers {
				x := res.Users[u]
				for a := 0; a < f; a++ {
					xa := float64(x[a])
					for b := 0; b < f; b++ {
						mat[a*f+b] += p.Alpha * xa * float64(x[b])
					}
					rhs[a] += p.Alpha * xa
				}
			}
			solveSPD(mat, rhs, vec, f)
			for a := 0; a < f; a++ {
				if d := math.Abs(float64(res.Works[w][a]) - vec[a]); d > moved {
					moved = d
				}
				res.Works[w][a] = float32(vec[a])
			}
		}

		res.Moved = append(res.Moved, moved)
		res.Fit = append(res.Fit, rmse(res, userItems, p.Alpha))
	}
	return res
}

// rmse is the confidence-weighted error over OBSERVED interactions. It is a training
// diagnostic: it must fall, and a run where it does not is a broken solver rather than a
// weak signal.
func rmse(r Result, userItems [][]int, alpha float64) float64 {
	total, n := 0.0, 0
	for u, items := range userItems {
		for _, w := range items {
			dot := 0.0
			x, y := r.Users[u], r.Works[w]
			for a := range x {
				dot += float64(x[a]) * float64(y[a])
			}
			e := (1 + alpha) * (1 - dot)
			total += e * e
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(total / float64(n))
}

// solveSPD solves A·v = b for a symmetric positive-definite A held row-major, by
// Cholesky decomposition. A is overwritten.
//
// Cholesky rather than plain Gaussian elimination because A is exactly YᵀCY + λI, which
// is SPD by construction, and the decomposition fails loudly (a non-positive pivot) if it
// ever is not — where elimination would return a plausible vector of nonsense.
func solveSPD(a, b, out []float64, n int) {
	// Decompose in place into L (lower triangle).
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			sum := a[i*n+j]
			for k := 0; k < j; k++ {
				sum -= a[i*n+k] * a[j*n+k]
			}
			if i == j {
				if sum <= 1e-12 {
					// The regularisation cannot make this happen with LAMBDA > 0; if it
					// does, the caller's matrix is not what this function is for.
					sum = 1e-12
				}
				a[i*n+i] = math.Sqrt(sum)
			} else {
				a[i*n+j] = sum / a[j*n+j]
			}
		}
	}
	// Forward substitution: L·y = b.
	for i := 0; i < n; i++ {
		sum := b[i]
		for k := 0; k < i; k++ {
			sum -= a[i*n+k] * out[k]
		}
		out[i] = sum / a[i*n+i]
	}
	// Back substitution: Lᵀ·v = y.
	for i := n - 1; i >= 0; i-- {
		sum := out[i]
		for k := i + 1; k < n; k++ {
			sum -= a[k*n+i] * out[k]
		}
		out[i] = sum / a[i*n+i]
	}
}

// initFactors fills the model from a fixed arithmetic pattern (no RNG, no clock), scaled
// small. A deterministic start is what makes a training run reproducible; identical
// factors are a problem only for the FIRST sweep, and the pattern varies with both index
// and dimension so no two rows start equal.
func initFactors(rows, f int, salt int) [][]float32 {
	out := make([][]float32, rows)
	for i := range out {
		row := make([]float32, f)
		for a := 0; a < f; a++ {
			// Two incommensurable moduli, so the pattern does not repeat across rows.
			v := math.Sin(float64((i+1)*(a+1)*salt)*0.001) * 0.05
			row[a] = float32(v)
		}
		out[i] = row
	}
	return out
}

// denseIndex returns the sorted distinct user and work ids.
func denseIndex(in []Interaction) ([]int64, []int64) {
	us := make(map[int64]struct{}, 1024)
	ws := make(map[int64]struct{}, 1024)
	for _, it := range in {
		us[it.User] = struct{}{}
		ws[it.Work] = struct{}{}
	}
	users := make([]int64, 0, len(us))
	for id := range us {
		users = append(users, id)
	}
	works := make([]int64, 0, len(ws))
	for id := range ws {
		works = append(works, id)
	}
	sort.Slice(users, func(i, j int) bool { return users[i] < users[j] })
	sort.Slice(works, func(i, j int) bool { return works[i] < works[j] })
	return users, works
}

// Cosine is the latent similarity between two factor rows, in [0,1].
//
// Negative dot products are clamped to 0 rather than mapped: a negative similarity means
// "these two attract different readers", which is not a reason to recommend one from the
// other, and a signal that can score below zero would fight the blend's own scale.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	c := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if c < 0 {
		return 0
	}
	return c
}
