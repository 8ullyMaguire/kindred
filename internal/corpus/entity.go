// Package corpus models entities and reads them from a source.
//
// The unit is deliberately kind-agnostic (SPEC §2.1): kind is a free
// string, not an enum, so adding a corpus must never require editing the
// engine. "Recommend books" and "recommend AO3 works" are the same code
// path with a different kind filter.
package corpus

// Entity is anything with identity, a label, and a bag of weighted tags.
type Entity struct {
	ID      int64
	Kind    string
	Title   string
	URL     string
	Summary string
	Stats   map[string]float64
	Tags    []Tag
}

type Tag struct {
	Name   string
	Type   string
	Weight float64
}

// Candidate is a scored-in-context entity. It is a value type on purpose:
// ranking holds tens of thousands of these and pointer-chasing them is
// measurable at that size.
type Candidate struct {
	Entity
	Score    float64
	Evidence []Evidence
}

// Evidence is one signal's contribution, carried into the response so a
// ranking can be argued with (SPEC §7). A recommender that cannot explain
// itself is not one anyone can argue about.
type Evidence struct {
	Signal string  `json:"signal"`
	Value  float64 `json:"value"`
	Reason string  `json:"reason"`
}

// TagSet is a set of tag ids for fast intersection and overlap maths.
// Built once per entity per request, never stored: a map per candidate in
// a pool of 100k is memory this project does not have.
type TagSet map[int32]float64

// Intersect returns the number of shared tags and the sum of the smaller
// weight on each, which is what the Jaccard-style overlap signal needs.
func Intersect(a, b TagSet) (shared int, weight float64) {
	// Iterate the smaller side. Pool properties that do not vary per
	// candidate are the largest cost class in a recommender; picking the
	// smaller side here keeps this O(min(|a|,|b|)) rather than O(|a|).
	if len(b) < len(a) {
		a, b = b, a
	}
	for id, w := range a {
		if other, ok := b[id]; ok {
			shared++
			if other < w {
				weight += other
			} else {
				weight += w
			}
		}
	}
	return shared, weight
}

// Source enumerates entities of one kind into a sink. Written once per
// corpus, never touched by the engine or the API.
type Source interface {
	Kind() string
	Iterate(fn func(Entity) error) error
}
