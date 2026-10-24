package authorcf

// The measurement (#35): does co-readership rank authors better than the tag profile the
// endpoint ships today?
//
// The experiment is a READER HOLDOUT on author pairs, because that is the only holdout in
// this corpus that can move:
//
//	1. For a seed author S, split S's readers 80/20, deterministically (seeded by the author
//	   key, not the clock -- two runs must produce the same number).
//	2. Candidates are the authors the 80% also read. Nothing else may be a candidate, so no
//	   method can win by proposing someone the evidence never named.
//	3. Truth is the candidate every HELD-OUT reader also read. A pair that only the 80%
//	   supports is not a fact about the 20%, and a metric that counts it measures the
//	   evidence against itself.
//	4. Two rankings over the SAME candidates and the SAME truth:
//	     proxy -- the engine's own `SimilarAuthors` order, filtered to the candidates;
//	              unranked candidates are OUTSIDE its ranking entirely, which is the proxy's
//	              real limitation, not a scoring choice: it cannot place an author whose
//	              works carry none of the seed's distinguishing tags, and giving it credit
//	              for the other arm's order would score a blind ranker as well as
//	              co-readership.
//	     cf    -- the co-reader count from the 80% only.
//
// The proxy is a CALLBACK rather than an import: `authorcf` must not depend on `engine`
// (the engine depends on the corpus and arena, and this package is below both), and the
// caller in `cmd` wires the shipped function in. That also makes the comparison honest by
// construction -- the proxy arm is whatever the endpoint actually runs.

import (
	"hash/fnv"
	"sort"
)

// ProxyRanker returns an author's similar authors in the endpoint's own order.
type ProxyRanker func(author string, limit int) ([]string, error)

// Params configures a measurement run.
type Params struct {
	// Seeds is how many seed authors to evaluate. Each costs one proxy call.
	Seeds int
	// HoldoutPercent is the share of a seed's readers held out as truth (20 = a fifth).
	HoldoutPercent int
	// Candidates caps the candidate set per seed, by co-reader count before the split
	// would rank them; it is a runtime bound, not a scoring rule.
	Candidates int
	// K is the cut-off for recall.
	K int
	// MinReaders is the floor on a seed author's readership: with fewer, the holdout is a
	// handful of people and every metric is noise with four decimal places.
	MinReaders int
}

// DefaultParams is the configuration this project measures with.
func DefaultParams() Params {
	return Params{Seeds: 40, HoldoutPercent: 20, Candidates: 200, K: 10, MinReaders: 30}
}

// SeedResult is one seed author's contribution.
type SeedResult struct {
	Author     string
	Readers    int // total readers of the seed author
	HeldOut    int // how many of them were held out
	Candidates int // candidates offered to both methods
	Truth      int // candidates a held-out reader also read
	ProxyHits  int // truth pairs inside the proxy's top K
	CFHits     int // truth pairs inside co-readership's top K
	// ProxyRanked is how many candidates the proxy could place at all. When it is small,
	// the proxy's recall is bounded by its candidate pool, not by its tag arithmetic.
	ProxyRanked int
}

// Report is a measurement run.
type Report struct {
	Params Params
	Seeds  []SeedResult
	// Skipped counts authors that could not be evaluated, by reason.
	Skipped map[string]int
}

// Measure runs the experiment.
func Measure(idx *Index, proxy ProxyRanker, p Params) Report {
	if p.Seeds <= 0 {
		p = DefaultParams()
	}
	rep := Report{Params: p, Skipped: map[string]int{}}

	// Deterministic seed order: authors with enough readers, by key. Sorting then taking the
	// first N would always evaluate the same alphabetically-early authors, so the sample is
	// spread by a hash of the key -- stable across runs, not biased by spelling.
	var eligible []string
	for _, key := range idx.Authors() {
		if idx.Readers(key) >= p.MinReaders {
			eligible = append(eligible, key)
		}
	}
	if len(eligible) == 0 {
		rep.Skipped["no author has enough readers"] = 1
		return rep
	}
	sort.Slice(eligible, func(a, b int) bool {
		return hashOf(eligible[a]) < hashOf(eligible[b])
	})
	if len(eligible) > p.Seeds {
		eligible = eligible[:p.Seeds]
	}
	sort.Strings(eligible)

	for _, seed := range eligible {
		res, skip := measureOne(idx, proxy, seed, p)
		if skip != "" {
			rep.Skipped[skip]++
			continue
		}
		rep.Seeds = append(rep.Seeds, res)
	}
	return rep
}

func measureOne(idx *Index, proxy ProxyRanker, seed string, p Params) (SeedResult, string) {
	readers := make([]int64, 0, idx.Readers(seed))
	for r := range idx.byAuthor[seed] {
		readers = append(readers, r)
	}
	if len(readers) < p.MinReaders {
		return SeedResult{}, "seed below the reader floor"
	}
	sort.Slice(readers, func(a, b int) bool { return readers[a] < readers[b] })

	// The split is per READER, seeded by the seed author's key: a fixed fraction, stable
	// across runs, and no dependence on map iteration order.
	evidence := map[int64]struct{}{}
	heldOut := map[int64]struct{}{}
	for _, r := range readers {
		if hashOfInt(seed, r)%100 < uint64(p.HoldoutPercent) {
			heldOut[r] = struct{}{}
		} else {
			evidence[r] = struct{}{}
		}
	}
	if len(heldOut) == 0 || len(evidence) == 0 {
		return SeedResult{}, "the reader split produced an empty side"
	}

	// Candidates: what the EVIDENCE readers read besides the seed.
	counts := map[string]int{}
	for r := range evidence {
		for author := range idx.byReader[r] {
			// The same exclusion the product applies: a candidate ranked by readers that the
			// endpoint would never show would score the measurement against a list it does not
			// serve.
			if author == seed || !isAPerson(author) {
				continue
			}
			counts[author]++
		}
	}
	if len(counts) == 0 {
		return SeedResult{}, "the evidence readers read nobody else"
	}
	cands := idx.rank(counts, p.Candidates)

	// Truth: the candidates a held-out reader also read. A candidate only the evidence
	// supports is not a fact about the held-out fifth.
	truth := map[string]struct{}{}
	for r := range heldOut {
		for author := range idx.byReader[r] {
			if author == seed {
				continue
			}
			if _, isCand := counts[author]; isCand {
				truth[author] = struct{}{}
			}
		}
	}
	res := SeedResult{
		Author:     idx.Display(seed),
		Readers:    len(readers),
		HeldOut:    len(heldOut),
		Candidates: len(cands),
		Truth:      len(truth),
	}

	// Co-readership arm: the candidate order is already by co-reader count over the
	// evidence set.
	for i, c := range cands {
		if i >= p.K {
			break
		}
		if _, ok := truth[c.Key]; ok {
			res.CFHits++
		}
	}

	// Proxy arm: the endpoint's own ranking, filtered to the candidates that survive. An
	// author the proxy does not return ranks last.
	order, err := proxy(idx.Display(seed), 200)
	if err != nil {
		// A proxy failure is reported, not counted as a miss: a method that could not run
		// is not a method that lost.
		return SeedResult{}, "the proxy ranking failed"
	}
	rank := make(map[string]int, len(order))
	for i, name := range order {
		key := Key(name)
		if _, seen := rank[key]; !seen {
			rank[key] = i
		}
	}
	type scored struct {
		key  string
		rank int
	}
	// ONLY candidates the proxy actually returned are in its ranking. An unranked candidate
	// cannot be "last": the proxy has no opinion about it, and letting it inherit the
	// co-reader order would hand the proxy the other arm's work -- a blind proxy then scores
	// exactly what co-readership scores, which is how the first draft of this function
	// reported one hit for a ranker that returned nothing.
	sortable := make([]scored, 0, len(cands))
	for _, c := range cands {
		if r, ok := rank[c.Key]; ok {
			res.ProxyRanked++
			sortable = append(sortable, scored{c.Key, r})
		}
	}
	sort.SliceStable(sortable, func(a, b int) bool { return sortable[a].rank < sortable[b].rank })
	for i, s := range sortable {
		if i >= p.K {
			break
		}
		if _, ok := truth[s.key]; ok {
			res.ProxyHits++
		}
	}
	return res, ""
}

// Totals is the aggregate a report is read through.
type Totals struct {
	Seeds       int
	Truth       int
	ProxyHits   int
	CFHits      int
	ProxyRanked int
	Candidates  int
}

// Totals sums the per-seed rows.
func (r Report) Totals() Totals {
	var t Totals
	for _, s := range r.Seeds {
		t.Seeds++
		t.Truth += s.Truth
		t.ProxyHits += s.ProxyHits
		t.CFHits += s.CFHits
		t.ProxyRanked += s.ProxyRanked
		t.Candidates += s.Candidates
	}
	return t
}

// Recall is the fraction of truth pairs that landed in the top K, pooled over seeds.
//
// Pooled rather than averaged per seed: a seed with four truth pairs must not weigh as much
// as one with forty, and the question is "of the pairs that existed, how many did the method
// find".
func (t Totals) Recall(hits int) float64 {
	if t.Truth == 0 {
		return 0
	}
	return float64(hits) / float64(t.Truth)
}

func hashOf(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

func hashOfInt(s string, n int64) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	var b [8]byte
	u := uint64(n)
	for i := 0; i < 8; i++ {
		b[i] = byte(u >> (8 * i))
	}
	h.Write(b[:])
	return h.Sum64()
}
