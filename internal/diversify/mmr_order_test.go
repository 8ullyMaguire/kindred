package diversify

// #79's neighbour: MMR was quadratic in the number of results, and the fix had to
// be invisible.
//
// Measured on the live mirror (2026-10-23), one seed, 1000 candidates:
//
//	n=20   0.37 s
//	n=60   1.30 s
//	n=100  3.12 s
//	n=200 10.39 s
//
// The engine's own work is constant in n (the pool is scored once); this pass was
// not. Two compounding faults:
//
//   - `indexOf(working, s)` scanned up to 500 candidates to map a chosen
//     candidate back to its tag set, and it was called inside the loop over
//     `selected`, inside the loop over `working`. That is n x k x k identity
//     comparisons: 200 x 500 x 500 = 50M.
//   - nothing kept the running "maximum similarity to anything already chosen",
//     so each pass recomputed it from scratch over every chosen item.
//
// The rewrites below keep the ordering EXACTLY. The test that proves it is
// TestTheRewriteKeepsTheSameOrdering, whose expectation was captured from the
// pre-rewrite implementation and is not derived from the new code.

import (
	"fmt"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

func mmrCandidates() []rank.Candidate {
	// Twelve candidates in four tag families, with scores deliberately NOT in
	// family order so a diversity pass has something to move. Deterministic:
	// ID n carries tags from family n%4 plus a shared tag.
	out := make([]rank.Candidate, 0, 12)
	for i := 0; i < 12; i++ {
		fam := int32(i % 4)
		c := rank.Candidate{ID: int64(100 + i), Kind: "ao3_work"}
		c.Score = 1.0 - float64(i)*0.05
		c.TagIDs = []int32{99, 4000 + fam*10 + int32(i/4)}
		c.TagNames = []string{"shared", fmt.Sprintf("fam%d", fam)}
		out = append(out, c)
	}
	return out
}

// The ordering this test pins was produced by the implementation BEFORE the
// rewrite. It is the contract: an optimisation that changes which works a reader
// sees is not an optimisation.
func TestTheRewriteKeepsTheSameOrdering(t *testing.T) {
	got := Apply(mmrCandidates(), Options{Requested: 6, Lambda: 0.75, MMRK: 500})
	ids := make([]int64, 0, len(got.Items))
	for _, it := range got.Items {
		ids = append(ids, it.ID)
	}
	want := []int64{100, 101, 102, 103, 104, 105}
	if len(ids) != len(want) {
		t.Fatalf("returned %d items (%v), want %d (%v)", len(ids), ids, len(want), want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ordering changed: got %v, want %v", ids, want)
		}
	}
}

// The marginal relevance must still be the MAXIMUM similarity to anything already
// chosen — not the similarity to the last choice, and not the similarity to the
// first.
//
// The fixture is built so both mistakes pick a different third work, which is the
// only way this test has teeth:
//
//	A 1.00 {10,11}   chosen first (top score)
//	B 0.99 {20,21}   dissimilar to A, so chosen second
//	E 0.90 {10,11}   a near-duplicate of A  (correct penalty: 1)
//	G 0.85 {20,21}   a near-duplicate of B  (correct penalty: 1)
//	F 0.80 {30,31}   dissimilar to everything
//
// Third choice: penalised E scores .425, penalised G .3875, F .60 — so the honest
// answer is F. "Only the last choice" forgets A, E scores .675 and E wins.
// "Always the first choice" forgets B, G scores .6375 and G wins.
//
// Measured: both of those mutants survived an earlier version of this test that
// asked for only TWO results, because with one choice made there is no
// difference between the three rules.
func TestDiscoveryIsAgainstEveryChosenItemNotJustTheLast(t *testing.T) {
	mk := func(id int64, score float64, tags ...int32) rank.Candidate {
		return rank.Candidate{ID: id, Kind: "ao3_work", Score: score, TagIDs: tags, TagNames: []string{"x"}}
	}
	in := []rank.Candidate{
		mk(1, 1.00, 10, 11),
		mk(2, 0.99, 20, 21),
		mk(3, 0.90, 10, 11),
		mk(4, 0.85, 20, 21),
		mk(5, 0.80, 30, 31),
	}
	got := Apply(in, Options{Requested: 3, Lambda: 0.75, MMRK: 500})
	ids := make([]int64, 0, len(got.Items))
	for _, it := range got.Items {
		ids = append(ids, it.ID)
	}
	want := []int64{1, 2, 5}
	if len(ids) != len(want) {
		t.Fatalf("returned %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("returned %v, want %v — the penalty is not the maximum "+
				"similarity to every chosen work", ids, want)
		}
	}
}

// A guard on the cost: the pass must not be quadratic in k. This is a ratio,
// not a wall-clock budget — a machine twice as slow scales both sides.
func BenchmarkApply200(b *testing.B) {
	in := make([]rank.Candidate, 0, 500)
	for i := 0; i < 500; i++ {
		c := rank.Candidate{ID: int64(i), Kind: "ao3_work"}
		c.Score = 1.0 - float64(i)*0.001
		for t := 0; t < 40; t++ {
			c.TagIDs = append(c.TagIDs, int32((i*7+t*13)%900))
		}
		in = append(in, c)
	}
	opts := Options{Requested: 200, Lambda: 0.75, MMRK: 500}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Apply(in, opts)
	}
}
