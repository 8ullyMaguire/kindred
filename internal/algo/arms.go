package algo

// Arm assignment: which algorithm a reader gets when they have not chosen one.
//
// # Why this is a hash and not a counter
//
// A counter (row 1 gets A, row 2 gets B) is stable only while nobody is added or
// removed. This table accumulates readers from a shared cookie space, so the
// counter would silently reassign most of them mid-experiment and the standings
// would be measuring the reassignment. Hashing "kind|owner" is stable forever:
// the same reader lands in the same arm on every request and after every
// deploy, and only their own explicit choice overrides it.
//
// # Why an even split, and why only two arms
//
// Every extra arm divides the evidence. The experiment asks one question per
// kind — "is the default still the best?" — and the challenger is the strongest
// live alternative by the registry's own order. An even split reaches a verdict
// in half the traffic of a 90/10, which for a site measured in tens of readers a
// day is the difference between a month and two.
//
// # The cost, stated
//
// Half the readers on a kind get an algorithm that has not been measured as best
// on that kind. That is what a live experiment costs, it is why the reader can
// opt out with one click, and it is why the page says which arm the reader is
// in rather than leaving it invisible.

import (
	"hash/fnv"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// Challenger picks the second arm for a kind: the first live algorithm that is
// not the default, in registry order.
//
// Registry order is the page's order, and the page's order is the order a person
// would rank them by reading the blurbs — so the challenger is the strongest
// alternative as documented, not the strongest as measured (only the default has
// been measured, which is the point of running the experiment).
func Challenger(kind arena.EntityKind) ID {
	def := DefaultFor(kind)
	for _, id := range LiveIDs(kind) {
		if id != def {
			return id
		}
	}
	return ""
}

// ArmFor returns the algorithm this reader gets for this kind when they have no
// stored choice.
func ArmFor(kind arena.EntityKind, ownerKey string) ID {
	def := DefaultFor(kind)
	alt := Challenger(kind)
	if alt == "" || ownerKey == "" {
		// No session and no challenger both mean: no experiment to be in. The
		// default is the answer, and it is the honest one — an ownerless request
		// cannot be assigned stably, so assigning it would be a coin flip
		// recorded as a reader.
		return def
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(string(kind) + "|" + ownerKey))
	if h.Sum64()%2 == 0 {
		return def
	}
	return alt
}

// Resolve decides the algorithm for one request.
//
// Order of precedence, each step deliberate:
//
//  1. An explicit request parameter — a reader following a link with ?algo= is
//     asking for that algorithm, and an experiment must not override an
//     expressed preference.
//  2. The reader's stored choice for this kind — chosen once, honoured until
//     changed, because a settings page that keeps resetting is worse than none.
//  3. The A/B arm — the default when there is no session or no challenger.
func Resolve(kind arena.EntityKind, requested, stored, ownerKey string) (ID, string) {
	if requested != "" {
		// Even a refused request returns here: the caller wants the message, and
		// the id it falls back to is the default rather than an arm.
		return Parse(kind, requested)
	}
	if stored != "" {
		// A stored choice that is no longer live (an algorithm was retired) falls
		// back to the arm rather than to the default, so a retired choice does
		// not quietly pin the reader out of the experiment.
		if id, why := Parse(kind, stored); why == "" {
			return id, ""
		}
	}
	return ArmFor(kind, ownerKey), ""
}
