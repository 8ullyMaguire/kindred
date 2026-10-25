package tagtree

// #37's tests. The subject is a clustering, so the fixtures are built from KNOWN groups: four
// planted directions in 8 dimensions, each tag jittered around its group's centre. A clustering
// that cannot recover four well-separated groups from that is broken, and a purity number of
// 1.0 on it is a real number rather than a coincidence.
//
// The tests that matter most are the two that would pass on a BROKEN tree:
//   - purity on a fixture where every tag is in its own group (trivially 1.0) proves nothing,
//     so one test plants a group whose members are deliberately spread across two directions
//     and asserts the purity reflects it;
//   - a determinism test over two builds compares the TREES, not just their sizes.

import (
	"math"
	"math/rand"
	"testing"
)

// planted builds nGroups tight groups of perGroup tags in dim dimensions.
func planted(nGroups, perGroup, dim int, jitter float64, seed int64) []Vec {
	rng := rand.New(rand.NewSource(seed))
	centres := make([][]float32, nGroups)
	for g := range centres {
		centres[g] = make([]float32, dim)
		for d := range centres[g] {
			centres[g][d] = float32(rng.NormFloat64())
		}
	}
	var out []Vec
	id := int64(100)
	for g := 0; g < nGroups; g++ {
		for i := 0; i < perGroup; i++ {
			v := make([]float32, dim)
			for d := range v {
				v[d] = centres[g][d] + float32(rng.NormFloat64()*jitter)
			}
			out = append(out, Vec{TagID: id, V: v, Support: 100 - i})
			id++
		}
	}
	return out
}

func TestBuildRecoversPlantedGroups(t *testing.T) {
	vecs := planted(4, 10, 8, 0.15, 1)
	tree, err := Build(vecs, Options{K: 4, SubK: 0, Seed: 37, Iters: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Families) != 4 {
		t.Fatalf("families = %d, want 4", len(tree.Families))
	}
	for _, f := range tree.Families {
		if len(f.Members) != 10 {
			t.Fatalf("family %d has %d members, want 10: the planted groups are 10 tags each", f.ID, len(f.Members))
		}
	}
	// Coherence: members sit much closer to their own centroid than to a neighbour's. This is
	// the number that distinguishes a clustering from a partition of arbitrary vectors.
	if tree.OwnCos <= tree.RandomCos {
		t.Fatalf("own cosine %.3f is not above random %.3f: the tree is not a clustering",
			tree.OwnCos, tree.RandomCos)
	}
	if tree.OwnCos < 0.9 {
		t.Fatalf("own cosine %.3f is below 0.9 on well-separated groups", tree.OwnCos)
	}
}

// Purity has to be able to say "these are in the same place" AND "these are split". One test
// for each direction, because a purity function that always returns 1.0 passes the first.
func TestPuritySeesAPlantAndASplit(t *testing.T) {
	tight := planted(2, 10, 8, 0.1, 2)
	tree, err := Build(tight, Options{K: 2, SubK: 0, Seed: 7, Iters: 50})
	if err != nil {
		t.Fatal(err)
	}
	// The first ten tags are one planted group.
	var groupA []int64
	for _, v := range tight[:10] {
		groupA = append(groupA, v.TagID)
	}
	share, _, mixed := Purity(tree, groupA)
	if share != 1.0 || mixed != 0 {
		t.Fatalf("a planted group scattered: purity %.2f, %d outside its modal cluster", share, mixed)
	}

	// Now a "family" that is HALF of one planted group and HALF of another: its modal cluster
	// can hold at most six, so the purity must be 0.6 and five must be counted as mixed.
	mixed50 := []int64{tight[0].TagID, tight[1].TagID, tight[2].TagID, tight[3].TagID, tight[4].TagID,
		tight[10].TagID, tight[11].TagID, tight[12].TagID, tight[13].TagID, tight[14].TagID}
	share, _, mixed = Purity(tree, mixed50)
	if share > 0.6 {
		t.Fatalf("a half-and-half set scored purity %.2f, which a split cannot produce", share)
	}
	if mixed == 0 {
		t.Fatalf("a half-and-half set reported no mixed members")
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	vecs := planted(3, 8, 6, 0.2, 3)
	a, err := Build(vecs, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(vecs, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Families) != len(b.Families) {
		t.Fatalf("two builds made %d and %d families", len(a.Families), len(b.Families))
	}
	for i := range a.Families {
		fa, fb := a.Families[i], b.Families[i]
		if fa.ID != fb.ID || len(fa.Members) != len(fb.Members) {
			t.Fatalf("family %d differs between builds: %d vs %d members", i, len(fa.Members), len(fb.Members))
		}
		for j := range fa.Members {
			if fa.Members[j].TagID != fb.Members[j].TagID {
				t.Fatalf("family %d member %d is tag %d in one build and %d in the other",
					i, j, fa.Members[j].TagID, fb.Members[j].TagID)
			}
		}
	}
	if a.Inertia != b.Inertia {
		t.Fatalf("inertia differs between identical builds: %v vs %v", a.Inertia, b.Inertia)
	}
}

// A tight blob of 23 tags plus one outlier: unconstrained k-means leaves all 23 together, so
// the subfamily level has to impose the capacity or the division answers nothing. Measured on
// the live archive as a 3,560-tag family whose first subfamily held 2,711.
// The capacity constraint is tested WHERE IT LIVES, not through Build.
//
// A synthetic family could not reproduce the live collapse (30 tight points split into k equal
// parts like any other cloud; the live case is 2,711 of 3,560 tags in one bucket out of 29
// centroids, which needs the archive's density profile). Rather than tune a fixture until it
// failed, the constraint gets a direct test: nine points nearest one centroid, one nearest
// another, capacity six -- six and four is the only answer the constraint allows.
func TestAssignBalancedSpillsOverWhenFull(t *testing.T) {
	vecs := make([]Vec, 10)
	for i := 0; i < 9; i++ {
		vecs[i] = Vec{TagID: int64(i), V: []float32{float32(i) * 0.01, 0}}
	}
	vecs[9] = Vec{TagID: 9, V: []float32{10, 10}}
	centroids := [][]float32{{0, 0}, {10, 10}}
	assign := make([]int, len(vecs))
	for i := range assign {
		assign[i] = -1
	}
	if _, moved := assignBalanced(vecs, centroids, assign, 6); moved != len(vecs) {
		t.Fatalf("moved %d of %d on the first pass", moved, len(vecs))
	}
	load := [2]int{}
	for i, c := range assign {
		if c < 0 || c > 1 {
			t.Fatalf("vector %d was assigned to cluster %d", i, c)
		}
		load[c]++
	}
	if load[0] > 6 || load[1] > 6 {
		t.Fatalf("loads %v exceed the capacity of 6", load)
	}
	if load[0]+load[1] != len(vecs) {
		t.Fatalf("loads %v do not cover %d vectors", load, len(vecs))
	}
	if load[0] == 0 {
		t.Fatalf("every vector went to one cluster: the constraint is not splitting anything")
	}
}

func TestSubfamiliesPartitionTheirFamily(t *testing.T) {
	vecs := planted(1, 24, 8, 0.4, 4)
	tree, err := Build(vecs, Options{K: 1, SubK: 3, Seed: 11, Iters: 40})
	if err != nil {
		t.Fatal(err)
	}
	f := tree.Families[0]
	if len(f.Subfamilies) == 0 {
		t.Fatalf("a 24-member family with SubK 3 produced no subfamilies")
	}
	seen := map[int64]int{}
	for _, s := range f.Subfamilies {
		if len(s.Members) < 2 {
			t.Fatalf("subfamily %d has %d members; a division needs two", s.ID, len(s.Members))
		}
		for _, m := range s.Members {
			seen[m.TagID]++
		}
	}
	for _, m := range f.Members {
		if seen[m.TagID] != 1 {
			t.Fatalf("tag %d appears in %d subfamilies, want exactly 1", m.TagID, seen[m.TagID])
		}
	}
	// The subfamily level is CAPACITY-LIMITED: 24 tags into 3 with slack 1.5 caps a bucket at
	// 12. Without the constraint the region stayed in one subfamily (76% measured live), which
	// is the failure this assertion names.
	limit := int(math.Ceil(float64(len(f.Members))/float64(len(f.Subfamilies))*1.5)) + 1
	for _, s := range f.Subfamilies {
		if len(s.Members) > limit {
			t.Fatalf("subfamily %d holds %d of 24 tags (limit %d): the capacity constraint is not applied",
				s.ID, len(s.Members), limit)
		}
	}
}

func TestLabelsNameAClusterAndDropItsWater(t *testing.T) {
	vecs := planted(2, 6, 6, 0.05, 5)
	tree, err := Build(vecs, Options{K: 2, SubK: 0, Seed: 5, Iters: 30})
	if err != nil {
		t.Fatal(err)
	}
	// Every tag in family 0's first cluster is "worm something"; every tag in the other family
	// is "harry something". "the" is in EVERY tag, so it must be dropped as the water.
	names := map[int64]string{}
	for i, f := range tree.Families {
		prefix := "worm"
		if i == 1 {
			prefix = "harry"
		}
		for _, m := range f.Members {
			names[m.TagID] = prefix + " the thing"
		}
	}
	Labels(tree, names)
	for i, f := range tree.Families {
		want := "worm"
		if i == 1 {
			want = "harry"
		}
		if f.Label == "" || f.Label[0:len(want)] != want {
			t.Fatalf("family %d labelled %q, want it to start with %q", i, f.Label, want)
		}
		if containsWord(f.Label, "the") {
			t.Fatalf("family %d labelled %q still carries the word in every member", i, f.Label)
		}
	}
}

// Centring is tested as a TRANSFORMATION, not as a ranking improvement.
//
// I tried to build a fixture where uncentred k-means collapses and centred does not, and could
// not: four separated groups survive either way, and the fixtures I invented to fail produced
// the opposite of the live result (centred 40% in one family against uncentred 28%). The live
// collapse (11,753 of 14,049 tags in one family) is a property of 14k real vectors, not of
// anything forty points can reproduce -- so the claim lives in the handoff as a measurement, and
// the CONTRACT lives here: after centring, the mean is zero and every vector is unit length,
// which is what makes Euclidean distance equal cosine distance downstream.
func TestCentringZeroesTheMeanAndNormalises(t *testing.T) {
	vecs := planted(3, 6, 5, 0.3, 12)
	for i := range vecs {
		for d := 0; d < 5; d++ {
			vecs[i].V[d] += 3
		}
	}
	before := make([][]float32, len(vecs))
	for i := range vecs {
		before[i] = append([]float32(nil), vecs[i].V...)
	}
	if _, err := Build(vecs, Options{K: 3, SubK: 0, Seed: 4, Iters: 20, Center: true}); err != nil {
		t.Fatal(err)
	}
	// Build must not rewrite its caller's slice.
	for i := range vecs {
		for d := range vecs[i].V {
			if vecs[i].V[d] != before[i][d] {
				t.Fatalf("Build mutated its input at [%d][%d]", i, d)
			}
		}
	}

	cp := make([]Vec, len(before))
	for i := range before {
		cp[i] = Vec{TagID: int64(i), V: append([]float32(nil), before[i]...)}
	}
	centerNormalize(cp, 5)
	mean := make([]float64, 5)
	for _, v := range cp {
		var norm float64
		for d := 0; d < 5; d++ {
			mean[d] += float64(v.V[d])
			norm += float64(v.V[d]) * float64(v.V[d])
		}
		if math.Abs(math.Sqrt(norm)-1) > 1e-4 {
			t.Fatalf("vector %d has length %.6f after centring, want 1", v.TagID, math.Sqrt(norm))
		}
	}
	// The mean is NOT exactly zero after both steps and cannot be: subtracting the mean gives a
	// zero-mean cloud, then scaling every vector to unit length moves it again. The claim that
	// survives is that the shared direction is GONE -- the output mean is a small fraction of
	// the offset the input carried (3 in every dimension here).
	for d := 0; d < 5; d++ {
		if got := math.Abs(mean[d] / float64(len(cp))); got > 0.75 {
			t.Fatalf("dimension %d still has a mean of %.3f: the shared offset was not removed", d, got)
		}
	}
}

// The centring pass reaches the clustering through Build, and inertia is how that is visible:
// unit vectors cannot be further apart than 2, so inertia <= 4n. Raw vectors scaled up carry
// more than that, which is the difference between "the pass ran" and "the test called the
// helper itself".
func TestBuildAppliesCentring(t *testing.T) {
	vecs := planted(3, 10, 6, 0.2, 14)
	for i := range vecs {
		for d := 0; d < 6; d++ {
			vecs[i].V[d] *= 9
		}
	}
	centred, err := Build(vecs, Options{K: 3, SubK: 0, Seed: 6, Iters: 30, Center: true})
	if err != nil {
		t.Fatal(err)
	}
	if limit := 4 * float64(len(vecs)); centred.Inertia > limit {
		t.Fatalf("inertia %.1f exceeds %.0f: the clustered vectors are not unit length, so the "+
			"centring/normalising pass did not run", centred.Inertia, limit)
	}
	raw, err := Build(vecs, Options{K: 3, SubK: 0, Seed: 6, Iters: 30, Center: false})
	if err != nil {
		t.Fatal(err)
	}
	if raw.Inertia <= 4*float64(len(vecs)) {
		t.Fatalf("uncentred inertia %.1f is inside the unit-vector bound: this fixture cannot "+
			"tell the two paths apart", raw.Inertia)
	}
}

// Members are ordered by SUPPORT inside a family, which is presentation, not determinism: two
// builds sort identically whether or not the sort is there.
func TestFamilyMembersAreOrderedBySupport(t *testing.T) {
	vecs := planted(2, 12, 6, 0.1, 15)
	// Supports descend with id inside each planted group, so an unsorted tree would show the
	// order the vectors arrived in rather than the order a reader wants.
	// Support ASCENDS with arrival order, so the tree's own order contradicts the input order.
	// (Ascending support with descending ids was the first version, and it made the sort a
	// no-op -- the mutant that deletes the sort survived it.)
	for i := range vecs {
		vecs[i].Support = 10 + (i%12)*10
	}
	tree, err := Build(vecs, Options{K: 2, SubK: 0, Seed: 8, Iters: 30})
	if err != nil {
		t.Fatal(err)
	}
	sortedSeen := false
	for _, f := range tree.Families {
		for i := 1; i < len(f.Members); i++ {
			if f.Members[i-1].Support < f.Members[i].Support {
				t.Fatalf("family %d lists support %d before %d: members are not ordered by support",
					f.ID, f.Members[i-1].Support, f.Members[i].Support)
			}
			if f.Members[i-1].Support != f.Members[i].Support {
				sortedSeen = true
			}
		}
	}
	if !sortedSeen {
		t.Fatalf("no family in this fixture has members of differing support: the ordering " +
			"assertion cannot fail")
	}
}

func TestBuildRefusesNothingToCluster(t *testing.T) {
	if _, err := Build(nil, DefaultOptions()); err == nil {
		t.Fatal("Build(nil) returned no error")
	}
	// A single tag is a legitimate tree with one family; it must not panic on an empty second
	// level or on a k larger than the corpus.
	tree, err := Build(planted(1, 1, 4, 0, 6), Options{K: 8, SubK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Families) != 1 || len(tree.Families[0].Members) != 1 {
		t.Fatalf("one tag built %d families with %d members", len(tree.Families), len(tree.Families[0].Members))
	}
}

func TestCosineIsBounded(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if got := Cosine(a, b); math.Abs(got-1) > 1e-9 {
		t.Fatalf("cosine of identical vectors = %v, want 1", got)
	}
	if got := Cosine(a, []float32{-1, 0, 0}); math.Abs(got+1) > 1e-9 {
		t.Fatalf("cosine of opposites = %v, want -1", got)
	}
	if got := Cosine([]float32{0, 0, 0}, a); got != 0 {
		t.Fatalf("cosine against a zero vector = %v, want 0", got)
	}
}

func containsWord(s, w string) bool {
	for _, f := range words(s) {
		if f == w {
			return true
		}
	}
	return false
}
