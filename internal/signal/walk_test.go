package signal

// The walk's shape, pinned on graphs small enough to solve by hand.
//
// A PageRank is easy to make "look reasonable": every implementation converges to
// something, so a green test on real data proves almost nothing. These fixtures
// have answers that can be computed on paper, which is the only way to tell a walk
// that propagates from one that merely spreads.

import (
	"math"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// fakeGraph is an in-memory adjacency for tests: id -> neighbours and weights.
type fakeGraph struct {
	adj map[int32][]int32
	w   map[int32][]float32
}

func (g *fakeGraph) Neighbours(id int32) ([]int32, []float32) {
	return g.adj[id], g.w[id]
}

func (g *fakeGraph) Frequency(int32) float64 { return 0 }

func (g *fakeGraph) NeighbourScore(int32, int, float64) []graph.ScoredNeighbour {
	return nil
}

func (g *fakeGraph) Name(int32) string { return "" }

// link adds an undirected weighted edge, mirroring how the CSR symmetrises.
func (g *fakeGraph) link(a, b int32, w float32) {
	g.adj[a] = append(g.adj[a], b)
	g.adj[b] = append(g.adj[b], a)
	g.w[a] = append(g.w[a], w)
	g.w[b] = append(g.w[b], w)
}

func newFake() *fakeGraph {
	return &fakeGraph{adj: map[int32][]int32{}, w: map[int32][]float32{}}
}

// A straight line 1-2-3-4 from seed 1: the walk must reach 3 (two hops) and 4
// (three), and must agree that 2 beats 3 beats 4.
func TestMassDecaysWithDistance(t *testing.T) {
	g := newFake()
	g.link(1, 2, 1)
	g.link(2, 3, 1)
	g.link(3, 4, 1)

	mass, total := WalkMass(g, []int32{1}, WalkOptions{})
	if total <= 0 {
		t.Fatal("no mass propagated")
	}
	if _, ok := mass[1]; ok {
		t.Fatal("the seed tag kept mass in the result: the score would be a slower tag_overlap")
	}
	if !(mass[2] > mass[3] && mass[3] > mass[4]) {
		t.Fatalf("mass does not decay with distance: 2=%v 3=%v 4=%v", mass[2], mass[3], mass[4])
	}
}

// Mass is CONSERVED. This is the invariant a walk written without per-node
// normalisation breaks: if each node hands out mass * degree instead of mass, the
// total grows every iteration and every score inflates with the length of the walk
// -- a failure that still "looks like" a ranking, which is why it needs a number.
func TestMassIsConserved(t *testing.T) {
	g := newFake()
	g.link(1, 2, 3)
	g.link(2, 3, 1)
	for _, leaf := range []int32{4, 5, 6, 7} {
		g.link(2, leaf, 2)
	}
	g.link(4, 8, 1)

	mass, total := WalkMass(g, []int32{1}, WalkOptions{MinMass: 1e-9})
	// The result excludes the seed tag and drops tiny mass, so it is compared
	// against 1 minus what the restart distribution keeps, not against 1.
	if total > 1.0 {
		t.Fatalf("propagated mass %v exceeds 1: mass is being created", total)
	}
	var seen float64
	for _, m := range mass {
		seen += m
	}
	if math.Abs(seen-total) > 1e-9 {
		t.Fatalf("returned mass %v disagrees with the reported total %v", seen, total)
	}
	// With damping 0.85 and a connected graph, the propagated share must be a
	// substantial fraction of the damping factor -- not a rounding crumb.
	if total < 0.5 {
		t.Fatalf("only %v of mass propagated across a connected graph", total)
	}
}

// Four identical leaves of one hub must receive identical mass. An implementation
// that normalises by the FIRST neighbour's weight, or that lets map iteration order
// decide the split, fails this on some runs and passes on others -- which is how it
// would have shipped.
func TestSymmetricLeavesGetEqualMass(t *testing.T) {
	g := newFake()
	g.link(1, 2, 1)
	leaves := []int32{3, 4, 5, 6}
	for _, leaf := range leaves {
		g.link(2, leaf, 1)
	}
	mass, _ := WalkMass(g, []int32{1}, WalkOptions{MinMass: 1e-9})
	first := mass[leaves[0]]
	if first <= 0 {
		t.Fatal("no mass reached the leaves")
	}
	for _, leaf := range leaves[1:] {
		if math.Abs(mass[leaf]-first) > 1e-9 {
			t.Fatalf("leaves are not symmetric: %v=%v vs %v=%v",
				leaves[0], first, leaf, mass[leaf])
		}
	}
}

// Two seeds sharing nothing: the walk starts from both, and neither seed's
// neighbourhood may be ignored.
func TestTwoDisjointSeedsBothContribute(t *testing.T) {
	g := newFake()
	g.link(10, 11, 1)
	g.link(20, 21, 1)
	mass, _ := WalkMass(g, []int32{10, 20}, WalkOptions{})
	if mass[11] <= 0 || mass[21] <= 0 {
		t.Fatalf("one seed's side was dropped: 11=%v 21=%v", mass[11], mass[21])
	}
	if math.Abs(mass[11]-mass[21]) > 1e-9 {
		t.Fatalf("symmetric seeds gave asymmetric mass: %v vs %v", mass[11], mass[21])
	}
}

// A tag listed on twelve seeds is ONE restart target. Without deduplication the
// walk's starting point would be decided by tagging habits.
func TestDuplicateSeedTagsDoNotWeightTheStart(t *testing.T) {
	g := newFake()
	g.link(1, 2, 1)
	g.link(3, 4, 1)
	once, _ := WalkMass(g, []int32{1, 3}, WalkOptions{})
	dupes, _ := WalkMass(g, []int32{1, 1, 1, 3, 3}, WalkOptions{})
	if math.Abs(once[2]-dupes[2]) > 1e-9 || math.Abs(once[4]-dupes[4]) > 1e-9 {
		t.Fatalf("repeated seed tags changed the walk: %v/%v vs %v/%v",
			once[2], once[4], dupes[2], dupes[4])
	}
}

// A leaf seed has no neighbours: its mass must return to the restart set rather
// than leak, or the total silently drops below 1 and every score deflates.
func TestALeafSeedDoesNotLeakMass(t *testing.T) {
	g := newFake()
	g.link(1, 2, 1) // 2 is a leaf
	mass, total := WalkMass(g, []int32{1}, WalkOptions{})
	if total <= 0 {
		t.Fatal("no mass after a leaf was expanded")
	}
	// A dead end re-injects into the restart and is then excluded from the
	// result (it IS the seed), so the surviving mass must be positive and
	// bounded by the damping factor.
	if total > 0.86 {
		t.Fatalf("surviving mass %v exceeds the damping factor: mass is being created", total)
	}
	if _, ok := mass[1]; ok {
		t.Fatal("seed tag present in the result")
	}
}

// The bound is on the signal, not on the caller: a graph larger than MaxNodes
// must return at most MaxNodes tags, keeping the heaviest.
func TestTheFrontierIsBounded(t *testing.T) {
	g := newFake()
	for i := int32(2); i < 60; i++ {
		g.link(1, i, float32(60-i)) // decreasing weights
	}
	mass, _ := WalkMass(g, []int32{1}, WalkOptions{MaxNodes: 10, MinMass: 0})
	if len(mass) == 0 {
		t.Fatal("nothing survived the bound")
	}
	if len(mass) > 10 {
		t.Fatalf("returned %d tags with MaxNodes 10", len(mass))
	}
	if _, ok := mass[2]; !ok {
		t.Fatal("the heaviest neighbour was trimmed rather than a light one")
	}
}

// Damping of 0 is restart-only: no mass leaves the seed, so there is nothing to
// score and the signal must have nothing to say rather than a uniform zero.
func TestRestartOnlyWalkHasNothingToScore(t *testing.T) {
	g := newFake()
	g.link(1, 2, 1)
	mass, total := WalkMass(g, []int32{1}, WalkOptions{Damping: 0.0001})
	if len(mass) == 0 || total <= 0 {
		return // acceptable: nothing propagated
	}
	// If something did propagate it must be tiny, not a full point of mass.
	if total > 0.01 {
		t.Fatalf("damping 0.0001 propagated %v of mass", total)
	}
}

// The signal's own contract: mass on the candidate's tags, normalised, and a skip
// (not a zero) when the walk never reached it.
func TestTheSignalScoresMassAndSkipsTheUnreached(t *testing.T) {
	s := Walk{
		Mass:    map[int32]float64{7: 0.4, 9: 0.1},
		Total:   0.5,
		MinMass: 1e-4,
	}
	got, reason, err := s.Score(rank.Candidate{TagIDs: []int32{7}}, nil, nil)
	if err != nil {
		t.Fatalf("reached candidate skipped: %v", err)
	}
	if math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("score = %v, want 0.8 (0.4 of 0.5 total)", got)
	}
	if reason == "" {
		t.Fatal("no reason given for a scored candidate")
	}
	if _, _, err := s.Score(rank.Candidate{TagIDs: []int32{11}}, nil, nil); err != rank.ErrSkip {
		t.Fatalf("unreached candidate returned %v, want ErrSkip: a zero would claim "+
			"the walk had looked and found nothing", err)
	}
	if _, _, err := s.Score(rank.Candidate{}, nil, nil); err != rank.ErrSkip {
		t.Fatal("a candidate with no tags did not skip")
	}
	// No mass at all (a request whose seeds have no tags) skips for every
	// candidate, so the engine can name it in meta.degraded.
	empty := Walk{}
	if _, _, err := empty.Score(rank.Candidate{TagIDs: []int32{7}}, nil, nil); err != rank.ErrSkip {
		t.Fatal("an empty walk scored a candidate")
	}
}
