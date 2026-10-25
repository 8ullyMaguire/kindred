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
