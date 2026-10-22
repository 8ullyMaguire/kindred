package rank

// Team-draft interleaving.
//
// The arena asks a reader to look at two lists and say which is better. That
// is a real measurement, but it costs the reader a comparison and shows them
// one list that is, by construction, worse than the other. Interleaving asks
// the same question without asking it: both algorithms contribute to ONE list,
// every item is tagged with the arm that contributed it, and what the reader
// does next (marks it seen, reads it, judges it in the arena) is the vote.
//
// This file is the merge only. It is a pure function over two ranked lists
// because the merge is where the guarantees live: a merge that silently
// favours one arm produces a win rate that measures the merge, not the
// algorithms. Keeping it pure is what lets the invariants below be tested
// without a database, a corpus, or a reader.

// InterleavedItem is one item of a merged list, tagged with its contributing
// arm ("a" or "b").
type InterleavedItem struct {
	Candidate
	// Arm is the algorithm that contributed this item. It is the string the
	// caller used to name the algorithm, not an index, because a stored
	// impression has to survive a rename of the caller's variables.
	Arm string `json:"arm"`
	// Slot is the 0-based position in the merged list.
	Slot int `json:"slot"`
}

// TeamDraft merges a and b into one list of at most n items.
//
// The rule (Radlinski, Kurup & Joachims 2008): alternate whose turn it is to
// contribute; on a turn, take that arm's highest-ranked item that is not
// already in the merged list. When one arm is exhausted the other supplies the
// rest. That gives three properties this function must hold:
//
//  1. No duplicates. An item both arms ranked appears once, credited to the
//     arm whose turn reached it first.
//  2. Equal exposure. Each arm contributes floor(n/2) or ceil(n/2) items
//     whenever it HAS that many. This is what makes a raw credit count
//     comparable across arms: if A got twice the slots, twice the clicks
//     means nothing.
//  3. Within-arm order. The items A contributed appear in the merged list in
//     A's own order, and likewise B's. A merge that reorders an arm's list is
//     ranking for it.
//
// Turn order starts with a, which is a small and deliberate unfairness: with
// an odd n, a contributes the extra slot. The alternative -- randomising the
// first turn -- needs a source of randomness and a stored seed to reproduce
// the merge, and a stored seed is one more thing that can differ between the
// impression and the standings. Odd n is refused at the call sites instead
// (the API requires an even n), so this asymmetry cannot be reached.
func TeamDraft(a, b []Candidate, armA, armB string, n int) []InterleavedItem {
	if n <= 0 || (len(a) == 0 && len(b) == 0) {
		return nil
	}
	if armA == "" {
		armA = "a"
	}
	if armB == "" {
		armB = "b"
	}

	out := make([]InterleavedItem, 0, n)
	taken := make(map[int64]bool, n)
	ia, ib := 0, 0
	turnA := true

	// next returns the next untaken item from a list, advancing its cursor.
	next := func(list []Candidate, i *int) (Candidate, bool) {
		for *i < len(list) {
			c := list[*i]
			*i++
			if taken[c.ID] {
				continue
			}
			return c, true
		}
		return Candidate{}, false
	}

	for len(out) < n {
		var (
			c   Candidate
			arm string
			ok  bool
		)
		// Two passes: the arm whose turn it is, then the other one. The
		// second pass is what keeps the list full when one arm runs dry,
		// and it is why an exhausted arm does not waste the slot.
		if turnA {
			if c, ok = next(a, &ia); ok {
				arm = armA
			} else if c, ok = next(b, &ib); ok {
				arm = armB
			}
		} else {
			if c, ok = next(b, &ib); ok {
				arm = armB
			} else if c, ok = next(a, &ia); ok {
				arm = armA
			}
		}
		if !ok {
			// Both lists are exhausted. This is reachable when n exceeds
			// the union of both lists, which the callers allow.
			break
		}
		taken[c.ID] = true
		out = append(out, InterleavedItem{Candidate: c, Arm: arm, Slot: len(out)})
		turnA = !turnA
	}
	return out
}

// ArmExposure counts how many slots each arm contributed, keyed by the arm
// label the caller supplied.
//
// Keyed rather than returned as an (a, b) pair because the label is arbitrary:
// the first version compared against the literal "a" and reported 0/6 when the
// caller had named its arms "one" and "two". The standings divide by these
// counts, and a credit rate over an unknown number of impressions is the
// classic way an interleaving experiment reports a confident wrong answer.
func ArmExposure(items []InterleavedItem) map[string]int {
	out := map[string]int{}
	for _, it := range items {
		out[it.Arm]++
	}
	return out
}
