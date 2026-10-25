package tagtree

// A tag hierarchy built from the tag embeddings (#37).
//
// `internal/tagfamily` holds a HAND-WRITTEN `families.json`: a person decided that the spellings
// of Worm belong together, and the file is right about the universes somebody thought of. It
// cannot grow: nothing in it was derived from the archive, so a tag nobody hand-listed is in no
// family, and there is no notion of a family at all for the 40k tags that are not universes
// (tropes, warnings, relationship shapes).
//
// The state database holds a 32-dimension embedding per tag. This package clusters them into a
// two-level tree -- family, then subfamily -- and reports how well it recovers the hand-written
// universes, which is the only ground truth available and a fair one: two tags a person put in
// the same universe should land together without being told.
//
// What this does NOT claim: that a cluster is a thing with a name. The label is the family's
// most common words, which is a description of its members, not a definition of them.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
)

// Vec is one tag's embedding.
type Vec struct {
	// ID is a TAG id when clustering tags and a WORK id when clustering works: the geometry is
	// identical and the caller knows which space it asked for. Support means works-per-tag in
	// the first case and kudos in the second, and both are only used to order a truncation.
	ID int64
	V  []float32
	// Support is how many works carry the tag. Clustering every tag would put 600k mostly
	// unused spellings in the way of the ones readers actually see; support is the filter.
	Support int
}

// Tree is the result: families, their subfamilies, and the members.
type Tree struct {
	K        int
	Dim      int
	Tags     int
	Families []Family
	// Iterations and Inertia are the k-means evidence: a run that did not converge is a tree
	// nobody should trust, and printing them is cheaper than discovering it later.
	Iterations int
	Inertia    float64
	// OwnCos and RandomCos are the coherence measurement: mean cosine of a member to its OWN
	// family centroid, and to a DIFFERENT family's centroid. A tree where the two are equal has
	// found nothing, and the gap is the only number here that says the clustering is real.
	OwnCos    float64
	RandomCos float64
	// Stability is the adjusted Rand index between two builds with DIFFERENT seeds: 1 is the
	// same partition, 0 is chance. Without ground truth this is the number that says whether the
	// tree is a property of the data or of the seed -- and k-means will happily agree with
	// itself on a partition that means nothing.
	Stability float64
	// Biggest is the share of tags in the largest family. A clustering where one family holds
	// most of the archive is not a hierarchy, whatever its inertia says.
	Biggest float64
}

// Family is a top-level cluster.
type Family struct {
	ID       int
	Label    string
	Members  []Member
	Centroid []float32
	// Subfamilies partition Members; a family of fewer than two members has none.
	Subfamilies []Subfamily
}

// Subfamily is a cluster inside a family.
type Subfamily struct {
	ID      int
	Label   string
	Members []Member
}

// Member is one tag in the tree.
type Member struct {
	ID      int64
	Name    string
	Support int
	// Dist is the cosine distance to the member's subfamily centroid (or family, when the
	// family has no split). The furthest members are how a reader finds the label's edges.
	Dist float64
}

// Options bounds the build.
type Options struct {
	// K is the number of top-level families.
	K int
	// SubK is the number of subfamilies inside each family (0 disables the second level).
	SubK int
	// MinSupport drops tags carried by fewer works. 25 keeps the tree about tags readers meet.
	MinSupport int
	// MaxTags caps the corpus so a build stays inside a minute. Tags are taken by support.
	MaxTags int
	// Seed makes k-means deterministic. Two builds with the same seed must be identical, or a
	// tree cannot be diffed between mirrors.
	Seed int64
	// Iters caps k-means iterations.
	Iters int
	// Balance caps how many tags one family may hold, as a multiple of the average. 0 disables it
	// and is the DEFAULT, on evidence:
	//
	//	unconstrained   25% of tags in one family  stability 0.87  coherence gap 0.64
	//	balanced 1.5     1% of tags in one family  stability 0.45  coherence gap 0.63
	//
	// The version that reads better as a table of sizes is the version that cuts through a dense
	// region arbitrarily, and its instability shows it: two seeds disagree on 55% of the tags.
	// A family of a quarter of the archive is a bad hierarchy; a family made of "whatever fit"
	// is a false one. So the skew is REPORTED (Tree.Biggest) and not papered over, and a caller
	// who wants equal buckets can ask.
	Balance float64
	// CenterSubtract removes the global mean before normalising. Measured on the live archive:
	// without it, one family swallowed 11,753 of 14,049 tags -- the embedding space has a huge
	// shared component (the popular fandoms), and Euclidean distance on unnormalised vectors is
	// mostly distance along it. After centring, what is left is what a tag does DIFFERENTLY.
	Center bool
}

// DefaultOptions is what `kindred tagtree` runs with.
func DefaultOptions() Options {
	return Options{K: 120, SubK: 6, MinSupport: 25, MaxTags: 40000, Seed: 37, Iters: 40, Center: true}
}

// Load reads the tag embeddings and the support counts.
//
// The embeddings live in the STATE database and the work counts in the CORPUS; a caller that
// has only one of the two gets an error naming which, because a tree built from embeddings with
// zero support everywhere would silently cluster unused spellings.
func Load(ctx context.Context, state, corpus *sql.DB, o Options) ([]Vec, error) {
	if state == nil || corpus == nil {
		return nil, fmt.Errorf("tagtree: both the state database (embeddings) and the corpus (support) are required")
	}
	rows, err := state.QueryContext(ctx,
		`SELECT entity_id, dim, vec FROM embeddings WHERE kind = 'tag'`)
	if err != nil {
		return nil, fmt.Errorf("tagtree: read embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	support := map[int64]int{}
	// Support comes from the corpus in one pass, not one query per tag: 40k round trips is the
	// shape that makes a 20-second build take an hour.
	supRows, err := corpus.QueryContext(ctx,
		`SELECT tag_id, COUNT(*) FROM work_tags GROUP BY tag_id`)
	if err != nil {
		return nil, fmt.Errorf("tagtree: read tag support: %w", err)
	}
	for supRows.Next() {
		var id int64
		var n int
		if err := supRows.Scan(&id, &n); err != nil {
			_ = supRows.Close()
			return nil, fmt.Errorf("tagtree: scan tag support: %w", err)
		}
		support[id] = n
	}
	if err := supRows.Err(); err != nil {
		_ = supRows.Close()
		return nil, fmt.Errorf("tagtree: tag support: %w", err)
	}
	_ = supRows.Close()

	var out []Vec
	for rows.Next() {
		var id int64
		var dim int
		var blob []byte
		if err := rows.Scan(&id, &dim, &blob); err != nil {
			return nil, fmt.Errorf("tagtree: scan embedding: %w", err)
		}
		n := support[id]
		if n < o.MinSupport {
			continue
		}
		if len(blob) != dim*4 {
			return nil, fmt.Errorf("tagtree: tag %d has dim %d but %d bytes", id, dim, len(blob))
		}
		v := make([]float32, dim)
		for i := 0; i < dim; i++ {
			// Little-endian float32, the writer's format (internal/embed).
			bits := uint32(blob[i*4]) | uint32(blob[i*4+1])<<8 | uint32(blob[i*4+2])<<16 | uint32(blob[i*4+3])<<24
			v[i] = math.Float32frombits(bits)
		}
		out = append(out, Vec{ID: id, V: v, Support: n})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tagtree: embeddings: %w", err)
	}
	// Highest support first, so MaxTags keeps the tags readers actually meet, and the order is
	// deterministic (support desc, then id) so a truncated build is reproducible.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Support != out[b].Support {
			return out[a].Support > out[b].Support
		}
		return out[a].ID < out[b].ID
	})
	if o.MaxTags > 0 && len(out) > o.MaxTags {
		out = out[:o.MaxTags]
	}
	return out, nil
}

// BuildAndMeasure runs Build twice, with seeds s and s+1, and reports the adjusted Rand index
// between the two partitions.
//
// The first tree is returned. This costs a second build (a few seconds on the live corpus) and
// it is the only way to answer "would another seed have said the same thing".
func BuildAndMeasure(vecs []Vec, o Options) (*Tree, error) {
	a, err := Build(vecs, o)
	if err != nil {
		return nil, err
	}
	o2 := o
	o2.Seed = o.Seed + 1000
	b, err := Build(vecs, o2)
	if err != nil {
		return nil, err
	}
	a.Stability = AdjustedRand(partition(a), partition(b))
	return a, nil
}

// partition maps tag id -> family id.
func partition(t *Tree) map[int64]int {
	out := make(map[int64]int, t.Tags)
	for _, f := range t.Families {
		for _, m := range f.Members {
			out[m.ID] = f.ID
		}
	}
	return out
}

// AdjustedRand is the chance-corrected agreement between two partitions (Hubert & Arabie).
func AdjustedRand(a, b map[int64]int) float64 {
	// Contingency over the ids present in BOTH partitions.
	type pair struct{ x, y int }
	cont := map[pair]int{}
	rows := map[int]int{}
	cols := map[int]int{}
	n := 0
	for id, x := range a {
		y, ok := b[id]
		if !ok {
			continue
		}
		cont[pair{x, y}]++
		rows[x]++
		cols[y]++
		n++
	}
	if n < 2 {
		return 0
	}
	choose2 := func(k int) float64 { return float64(k) * float64(k-1) / 2 }
	var sumCells, sumRows, sumCols float64
	for _, c := range cont {
		sumCells += choose2(c)
	}
	for _, r := range rows {
		sumRows += choose2(r)
	}
	for _, c := range cols {
		sumCols += choose2(c)
	}
	total := choose2(n)
	expected := sumRows * sumCols / total
	maxIndex := (sumRows + sumCols) / 2
	if maxIndex == expected {
		return 1
	}
	return (sumCells - expected) / (maxIndex - expected)
}

// Build clusters the vectors into families and subfamilies.
//
// k-means++ seeding, then Lloyd iterations until assignments stop moving or Iters is spent.
// k-means on 32 dimensions and 40k rows is a few seconds, and the alternative -- agglomerative
// clustering, which is what a dendrogram wants -- is O(n^2) in both time and memory and does
// not finish here.
func Build(vecs []Vec, o Options) (*Tree, error) {
	if len(vecs) == 0 {
		return nil, fmt.Errorf("tagtree: no tags to cluster (raise MaxTags or lower MinSupport)")
	}
	if o.K <= 0 {
		return nil, fmt.Errorf("tagtree: K must be positive")
	}
	// A zero Iters is a caller who did not care, not a caller asking for a build with no
	// assignments: the loop below is what FILLS `assign`, so Iters 0 would leave every tag at
	// -1 and bucket [-1] would panic. Options is a struct with a zero value, so every field
	// that cannot be zero has to say what zero means.
	if o.Iters <= 0 {
		o.Iters = 40
	}
	k := o.K
	if k > len(vecs) {
		k = len(vecs)
	}
	dim := len(vecs[0].V)
	if o.Center {
		// Build must not rewrite its caller's slice: the geometry pass is a transformation of
		// the input, and a second Build on the same slice would transform an already-centred
		// one. That is not a hypothetical -- it made two identical builds disagree, because
		// re-normalising unit vectors perturbs ties.
		cp := make([]Vec, len(vecs))
		for i, v := range vecs {
			cp[i] = Vec{ID: v.ID, Support: v.Support, V: append([]float32(nil), v.V...)}
		}
		vecs = cp
		centerNormalize(vecs, dim)
	}
	rng := rand.New(rand.NewSource(o.Seed))

	centroids := kmeansPlusPlus(sampleForSeeding(vecs, rng), k, rng)
	assign := make([]int, len(vecs))
	for i := range assign {
		assign[i] = -1
	}
	iters := 0
	inertia := 0.0
	capacity := 0
	if o.Balance > 0 {
		capacity = int(math.Ceil(float64(len(vecs)) / float64(k) * o.Balance))
		if capacity < 1 {
			capacity = 1
		}
	}
	for ; iters < o.Iters; iters++ {
		moved := 0
		inertia = 0
		if capacity > 0 {
			inertia, moved = assignBalanced(vecs, centroids, assign, capacity)
		} else {
			for i, v := range vecs {
				best, bestD := 0, math.Inf(1)
				for c := range centroids {
					d := sqDist(v.V, centroids[c])
					if d < bestD {
						best, bestD = c, d
					}
				}
				inertia += bestD
				if assign[i] != best {
					assign[i] = best
					moved++
				}
			}
		}
		recompute(vecs, centroids, assign)
		if moved == 0 {
			break
		}
	}

	t := &Tree{K: k, Dim: dim, Tags: len(vecs), Iterations: iters + 1, Inertia: inertia}
	buckets := make([][]int, k)
	for i, c := range assign {
		buckets[c] = append(buckets[c], i)
	}
	for c := 0; c < k; c++ {
		if len(buckets[c]) == 0 {
			continue
		}
		fam := Family{ID: c, Centroid: centroids[c]}
		for _, i := range buckets[c] {
			fam.Members = append(fam.Members, Member{
				ID:      vecs[i].ID,
				Support: vecs[i].Support,
				Dist:    math.Sqrt(sqDist(vecs[i].V, centroids[c])),
			})
		}
		if o.SubK > 1 && len(fam.Members) >= o.SubK*2 {
			// SubK is a FLOOR, not the count: a family of 3,600 tags divided six ways has
			// subfamilies of 600, which is not a hierarchy, it is the same grab-bag with a
			// label. sqrt scales the division to the size (3,600 -> 40, 130 -> 8).
			subK := o.SubK
			if want := int(math.Sqrt(float64(len(fam.Members))) / 1.5); want > subK {
				subK = want
			}
			if subK > 32 {
				subK = 32
			}
			fam.Subfamilies = split(fam, vecs, subK, rng, o.Iters)
		}
		t.Families = append(t.Families, fam)
		// Determinism: families in id order, members by support then id.
		sortMembers(t.Families[len(t.Families)-1].Members)
	}
	sort.Slice(t.Families, func(a, b int) bool { return t.Families[a].ID < t.Families[b].ID })
	t.OwnCos, t.RandomCos = coherence(vecs, assign, centroids, k)
	largest := 0
	for _, b := range buckets {
		if len(b) > largest {
			largest = len(b)
		}
	}
	t.Biggest = float64(largest) / float64(len(vecs))
	return t, nil
}

// assignBalanced assigns every vector to its nearest centroid that still has room.
//
// Greedy in order of CONFIDENCE, not of input order: points are sorted by the gap between their
// best and second-best centroid, so a point that clearly belongs somewhere gets it first and an
// ambiguous point takes whatever is left. Input order would make the balance depend on the
// corpus sort, which is the kind of hidden dependence that makes two builds disagree.
func assignBalanced(vecs []Vec, centroids [][]float32, assign []int, capacity int) (inertia float64, moved int) {
	type cand struct {
		i       int
		best    int
		bestD   float64
		secondD float64
	}
	cands := make([]cand, len(vecs))
	for i, v := range vecs {
		best, bestD, secondD := 0, math.Inf(1), math.Inf(1)
		for c := range centroids {
			d := sqDist(v.V, centroids[c])
			if d < bestD {
				best, secondD = c, bestD
				bestD = d
			} else if d < secondD {
				secondD = d
			}
		}
		cands[i] = cand{i: i, best: best, bestD: bestD, secondD: secondD}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		return cands[a].secondD-cands[a].bestD > cands[b].secondD-cands[b].bestD
	})
	load := make([]int, len(centroids))
	for _, c := range cands {
		target := c.best
		if load[target] >= capacity {
			// Nearest centroid with room; if every one is full, stay where the nearest is.
			bestD := math.Inf(1)
			for ci := range centroids {
				if load[ci] >= capacity {
					continue
				}
				if d := sqDist(vecs[c.i].V, centroids[ci]); d < bestD {
					bestD, target = d, ci
				}
			}
		}
		load[target]++
		inertia += sqDist(vecs[c.i].V, centroids[target])
		if assign[c.i] != target {
			assign[c.i] = target
			moved++
		}
	}
	return inertia, moved
}

// centerNormalize subtracts the mean and scales every vector to unit length, in place.
//
// Cosine geometry is what a "these tags are about the same thing" question means, and Euclidean
// distance on raw embeddings answers a different question: how much two vectors share, which
// the most popular fandoms dominate. Unit vectors make the two agree, and removing the mean
// first stops the shared component from being the only direction with any variance.
func centerNormalize(vecs []Vec, dim int) {
	mean := make([]float64, dim)
	for _, v := range vecs {
		for d := 0; d < dim; d++ {
			mean[d] += float64(v.V[d])
		}
	}
	for d := 0; d < dim; d++ {
		mean[d] /= float64(len(vecs))
	}
	for i := range vecs {
		var norm float64
		for d := 0; d < dim; d++ {
			vecs[i].V[d] = float32(float64(vecs[i].V[d]) - mean[d])
			norm += float64(vecs[i].V[d]) * float64(vecs[i].V[d])
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			// A tag whose vector IS the mean has no direction of its own. Leaving it at zero
			// puts it in whichever family reaches it first, which is the honest outcome.
			continue
		}
		for d := 0; d < dim; d++ {
			vecs[i].V[d] = float32(float64(vecs[i].V[d]) / norm)
		}
	}
}

// coherence compares each member to its own centroid and to another family's centroid.
//
// It is computed HERE, where the member vectors still exist, rather than from the Tree: the
// Tree keeps distances, and a distance cannot be turned back into a cosine without inventing
// the other vector.
func coherence(vecs []Vec, assign []int, centroids [][]float32, k int) (own, random float64) {
	if k < 2 {
		return 0, 0
	}
	var so, sr float64
	n := 0
	for i, v := range vecs {
		c := assign[i]
		if c < 0 {
			continue
		}
		other := (c + 1) % k
		so += Cosine(v.V, centroids[c])
		sr += Cosine(v.V, centroids[other])
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return so / float64(n), sr / float64(n)
}

// split runs a second k-means inside one family, over its own members.
func split(fam Family, vecs []Vec, subK int, rng *rand.Rand, iters int) []Subfamily {
	idx := map[int64][]float32{}
	for _, v := range vecs {
		idx[v.ID] = v.V
	}
	var sub []Vec
	for _, m := range fam.Members {
		sub = append(sub, Vec{ID: m.ID, V: idx[m.ID], Support: m.Support})
	}
	k := subK
	if k > len(sub) {
		k = len(sub)
	}
	centroids := kmeansPlusPlus(sub, k, rng)
	assign := make([]int, len(sub))
	for i := range assign {
		assign[i] = -1
	}
	// The second level IS capacity-limited, unlike the first, and the asymmetry is deliberate:
	// at the top the skew is real structure and cutting through it invents families; INSIDE a
	// family the question is different -- "which corner of this region is this tag in" -- and a
	// division that leaves 76% in one subfamily (measured) answers nothing. Balanced buckets are
	// a worse description and a better index.
	capacity := int(math.Ceil(float64(len(sub)) / float64(k) * 1.5))
	if capacity < 2 {
		capacity = 2
	}
	for it := 0; it < iters; it++ {
		_, moved := assignBalanced(sub, centroids, assign, capacity)
		recompute(sub, centroids, assign)
		if moved == 0 {
			break
		}
	}
	out := make([]Subfamily, k)
	for c := 0; c < k; c++ {
		out[c].ID = c
	}
	for i, c := range assign {
		m := Member{
			ID:      sub[i].ID,
			Support: sub[i].Support,
			Dist:    math.Sqrt(sqDist(sub[i].V, centroids[c])),
		}
		out[c].Members = append(out[c].Members, m)
	}
	// A subfamily needs at least two members to be a division of anything. With the capacity
	// constraint above this filter is UNREACHABLE -- the constraint keeps every bucket far above
	// one member (measured: 11 tags into 2 gives 9 and 2, never 10 and 1) -- and the mutant that
	// deletes it survives, which is the honest outcome for a guard on a path that does not
	// occur. It stays because the constraint is a parameter: relax it and this is what stops a
	// one-tag "division" from being written.
	kept := out[:0]
	for _, s := range out {
		if len(s.Members) >= 2 {
			sortMembers(s.Members)
			kept = append(kept, s)
		}
	}
	return kept
}

func sortMembers(ms []Member) {
	sort.Slice(ms, func(a, b int) bool {
		if ms[a].Support != ms[b].Support {
			return ms[a].Support > ms[b].Support
		}
		return ms[a].ID < ms[b].ID
	})
}

// sampleForSeeding picks at most 2,000 vectors to choose the initial centroids from.
//
// k-means++ is O(n*k*dim) and on 14k tags with k=300 it was most of a 25-second build: the
// centroids are a coarse quantisation of the cloud, and 2,000 points of it is plenty to find
// them. The ITERATIONS still run over every vector -- that is where the assignment is decided.
func sampleForSeeding(vecs []Vec, rng *rand.Rand) []Vec {
	const capN = 2000
	if len(vecs) <= capN {
		return vecs
	}
	idx := rng.Perm(len(vecs))[:capN]
	out := make([]Vec, capN)
	for i, j := range idx {
		out[i] = vecs[j]
	}
	return out
}

func kmeansPlusPlus(vecs []Vec, k int, rng *rand.Rand) [][]float32 {
	centroids := make([][]float32, 0, k)
	first := rng.Intn(len(vecs))
	centroids = append(centroids, append([]float32(nil), vecs[first].V...))
	d2 := make([]float64, len(vecs))
	for len(centroids) < k {
		total := 0.0
		for i, v := range vecs {
			best := math.Inf(1)
			for _, c := range centroids {
				if d := sqDist(v.V, c); d < best {
					best = d
				}
			}
			d2[i] = best
			total += best
		}
		if total == 0 {
			break
		}
		target := rng.Float64() * total
		picked := len(vecs) - 1
		run := 0.0
		for i, w := range d2 {
			run += w
			if run >= target {
				picked = i
				break
			}
		}
		centroids = append(centroids, append([]float32(nil), vecs[picked].V...))
	}
	return centroids
}

func recompute(vecs []Vec, centroids [][]float32, assign []int) {
	dim := len(centroids[0])
	sums := make([][]float64, len(centroids))
	counts := make([]int, len(centroids))
	for c := range sums {
		sums[c] = make([]float64, dim)
	}
	for i, v := range vecs {
		c := assign[i]
		if c < 0 {
			continue
		}
		counts[c]++
		for d := 0; d < dim; d++ {
			sums[c][d] += float64(v.V[d])
		}
	}
	for c := range centroids {
		if counts[c] == 0 {
			continue
		}
		for d := 0; d < dim; d++ {
			centroids[c][d] = float32(sums[c][d] / float64(counts[c]))
		}
	}
}

func sqDist(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i] - b[i])
		s += d * d
	}
	return s
}

// Cosine is exposed for the coherence measurement.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Labels names a cluster by the words its members share AND other clusters do not.
//
// The first attempt dropped any word appearing in more than half a cluster's members, which
// inverts the goal: in a family whose tags are all "worm - something", "worm" IS the label, and
// that rule threw it away with the boilerplate (the fixture caught it -- every family came back
// "(unnamed)"). A word is water when it appears across MANY CLUSTERS, not when it appears
// throughout one. So the score is
//
//	share of the cluster's members carrying the word  x  rarity across clusters
//
// and "the", "freeform" and "-" fall out because they are everywhere, while "worm" survives
// because it is in one family out of a hundred.
func Labels(t *Tree, names map[int64]string) {
	// One pass over the whole tree: how many families hold each word.
	familiesWith := map[string]int{}
	for _, f := range t.Families {
		seen := map[string]bool{}
		for _, m := range f.Members {
			for _, w := range words(names[m.ID]) {
				seen[w] = true
			}
		}
		for w := range seen {
			familiesWith[w]++
		}
	}
	total := len(t.Families)
	for fi := range t.Families {
		f := &t.Families[fi]
		f.Label = labelWords(f.Members, names, familiesWith, total, 3)
		for si := range f.Subfamilies {
			s := &f.Subfamilies[si]
			if len(s.Members) < 2 {
				continue
			}
			// A subfamily inside a dense family is an INDEX bucket, not a topic: capacity-limited
			// membership cuts across subjects, so shared words are mush ("star wars potter").
			// Naming it after its three biggest members is a sample a reader can check, and it
			// does not claim the tags belong together.
			s.Label = memberNames(s.Members, names, 3)
		}
	}
}

// memberNames labels a bucket by its biggest members: a sample, not a shared theme.
func memberNames(members []Member, names map[int64]string, want int) string {
	var out []string
	for _, m := range members {
		if len(out) == want {
			break
		}
		n := names[m.ID]
		if len(n) > 40 {
			n = n[:40] + "…"
		}
		out = append(out, n)
	}
	return strings.Join(out, " · ")
}

func labelWords(members []Member, names map[int64]string, familiesWith map[string]int, totalFamilies, want int) string {
	counts := map[string]int{}
	for _, m := range members {
		for _, w := range words(names[m.ID]) {
			counts[w]++
		}
	}
	type scored struct {
		w string
		s float64
		n int
	}
	var all []scored
	for w, n := range counts {
		if len(w) < 3 || n < 2 {
			continue
		}
		share := float64(n) / float64(len(members))
		rarity := 1.0
		if totalFamilies > 1 {
			rarity = 1 - float64(familiesWith[w])/float64(totalFamilies)
		}
		if rarity <= 0 {
			continue
		}
		all = append(all, scored{w: w, s: share * rarity, n: n})
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].s != all[b].s {
			return all[a].s > all[b].s
		}
		return all[a].w < all[b].w
	})
	var out []string
	for _, c := range all {
		out = append(out, c.w)
		if len(out) == want {
			break
		}
	}
	if len(out) == 0 {
		return "(unnamed)"
	}
	return strings.Join(out, " ")
}

// words splits a tag name into label candidates.
//
// Punctuation is stripped and single/ two-letter tokens dropped: a label like
// "wars) (star skywalker" is what happens when the splitter keeps the brackets, and it reads
// like a bug rather than a family.
func words(name string) []string {
	fields := strings.Fields(strings.ToLower(name))
	out := fields[:0]
	for _, f := range fields {
		f = strings.Trim(f, "()[]{},.;:!?\"'&/|-–—")
		if len(f) < 3 {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Purity measures how well the tree recovers a hand-written family: the share of a universe's
// tags that land in its modal cluster.
//
// This is the only ground truth in the repository, and it is a weak one -- it was written by
// hand for OTHER purposes -- so a low number is not proof the tree is bad. A HIGH number is
// proof it is not arbitrary: nothing told the clustering that the spellings of Worm belong
// together except their embeddings.
func Purity(t *Tree, universe []int64) (share float64, cluster int, mixed int) {
	in := map[int64]bool{}
	for _, id := range universe {
		in[id] = true
	}
	per := map[int]int{}
	total := 0
	for _, f := range t.Families {
		for _, m := range f.Members {
			if in[m.ID] {
				per[f.ID]++
				total++
			}
		}
	}
	if total == 0 {
		return 0, -1, 0
	}
	best, bestN := -1, 0
	for c, n := range per {
		if n > bestN || (n == bestN && c < best) {
			best, bestN = c, n
		}
	}
	return float64(bestN) / float64(total), best, total - bestN
}
