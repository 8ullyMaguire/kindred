package signal

import (
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// The IDF-weighted overlap signal scores tag agreement by how RARE each
// shared tag is. These tests pin the three properties that make it worth
// having over the plain Jaccard:
//
//  1. A rare shared tag outweighs a common one.
//  2. It SKIPS rather than scoring zero when it has nothing to say.
//  3. It measures SIMILARITY, not containment — a candidate carrying the
//     seed's tags PLUS extra tags must score strictly less than one that
//     matches the seed exactly.
//
// The third property is the one that was wrong in the first version and
// was caught by the engine-level test, not here: with the candidate's own
// mass as the denominator, a superset scored 1.0, exactly like a match.

func idfSignal(total float64, freq map[int32]float64) IDFWeightedOverlap {
	return IDFWeightedOverlap{DocFreq: freq, Total: total}
}

func TestIDFOverlapSkipsWithNoTags(t *testing.T) {
	s := idfSignal(1000, map[int32]float64{1: 10})
	_, _, err := s.Score(rank.Candidate{TagIDs: []int32{1}}, nil, nil)
	if err != rank.ErrSkip {
		t.Fatalf("err = %v, want rank.ErrSkip with no seeds", err)
	}
}

func TestIDFOverlapSkipsWhenOnlyUbiquitousTagsAreShared(t *testing.T) {
	// "General Audiences" is on every work in the corpus. Sharing it is not
	// evidence, and scoring it as such would put a zero-information match
	// above a real one.
	s := idfSignal(1000, map[int32]float64{1: 1000})
	_, _, err := s.Score(
		rank.Candidate{TagIDs: []int32{1, 2}},
		[]rank.Candidate{{ID: 7, TagIDs: []int32{1}}},
		nil)
	if err != rank.ErrSkip {
		t.Fatalf("err = %v, want rank.ErrSkip: sharing a universal tag is not evidence", err)
	}
}

func TestIDFRareSharedTagBeatsCommonSharedTag(t *testing.T) {
	// Two candidates, one sharing a tag on 5 of 1000 works, the other
	// sharing a tag on 500 of 1000. The rare one must score higher.
	//
	// The candidate masses must be equal for this to be a fair comparison,
	// so both carry the rare tag as well; only the SHARED tag differs.
	s := idfSignal(1000, map[int32]float64{
		1: 5,   // rare
		2: 500, // common
		3: 100,
	})
	rare, _, err := s.Score(
		rank.Candidate{TagIDs: []int32{1, 3}},
		[]rank.Candidate{{ID: 7, TagIDs: []int32{1, 2}}},
		nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	common, _, err := s.Score(
		rank.Candidate{TagIDs: []int32{2, 3}},
		[]rank.Candidate{{ID: 7, TagIDs: []int32{1, 2}}},
		nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rare <= common {
		t.Fatalf("rare shared tag scored %v, common scored %v; want the rare one higher",
			rare, common)
	}
}

func TestIDFOverlapIsSymmetricNotContainment(t *testing.T) {
	// The candidate carries the seed's tags PLUS extra ones. It must score
	// STRICTLY less than an exact match.
	//
	// This is the regression test for the containment bug: with the
	// candidate's own mass as the denominator, the superset scored 1.0 —
	// the same as the exact match — and a work containing the seed
	// outranked one that merely matched it.
	s := idfSignal(1000, map[int32]float64{1: 10, 2: 20, 3: 30, 4: 40})
	seed := []rank.Candidate{{ID: 7, TagIDs: []int32{1, 2}}}

	exact, _, err := s.Score(rank.Candidate{TagIDs: []int32{1, 2}}, seed, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	superset, _, err := s.Score(rank.Candidate{TagIDs: []int32{1, 2, 3, 4}}, seed, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if superset >= exact {
		t.Fatalf("superset scored %v, exact match scored %v; want the superset strictly lower",
			superset, exact)
	}
}

func TestIDFOverlapScoresAnExactMatchHighest(t *testing.T) {
	s := idfSignal(1000, map[int32]float64{1: 10, 2: 20, 3: 30})
	v, _, err := s.Score(
		rank.Candidate{TagIDs: []int32{1, 2}},
		[]rank.Candidate{{ID: 7, TagIDs: []int32{1, 2}}},
		nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v < 0.99 {
		t.Fatalf("exact match scored %v, want ~1.0", v)
	}
}

func TestIDFOverlapNamesTheRarestSharedTag(t *testing.T) {
	// The reason string names the tag that did the work, so a reader can
	// argue with the ranking by pointing at it.
	s := idfSignal(1000, map[int32]float64{1: 500, 2: 5})
	_, reason, err := s.Score(
		rank.Candidate{TagIDs: []int32{1, 2}},
		[]rank.Candidate{{ID: 7, TagIDs: []int32{1, 2}}},
		nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason == "" {
		t.Fatal("reason is empty")
	}
	// Tag 2 is the rarer one (5 of 1000), so it must be the one named.
	if !strings.Contains(reason, "tag 2") {
		t.Fatalf("reason %q does not name the rarest shared tag (tag 2)", reason)
	}
}

func TestIDFOverlapTreatsAnUnknownTagAsRare(t *testing.T) {
	// A tag missing from the frequency table is a table-completeness gap,
	// not an error. It must not fail the whole ranking.
	s := idfSignal(1000, map[int32]float64{1: 10})
	v, _, err := s.Score(
		rank.Candidate{TagIDs: []int32{1, 999}},
		[]rank.Candidate{{ID: 7, TagIDs: []int32{1, 999}}},
		nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v <= 0 {
		t.Fatalf("value = %v, want > 0", v)
	}
}

func TestIDFOverlapSkipsWithoutADocumentFrequencyTable(t *testing.T) {
	// The mirror has not been ingested, so there is no way to tell a rare
	// tag from a universal one. The signal must SKIP, not fall back to
	// unweighted overlap: answering with a duplicate of tag_overlap under a
	// different name tells a reader two signals agreed when one never ran.
	//
	// rank.Meta reports a signal that skips for every candidate in
	// `degraded`, so this is a NAMED absence rather than a silent one.
	for _, s := range []IDFWeightedOverlap{
		// Nothing loaded at all.
		{},
		// Empty table, no total.
		{DocFreq: map[int32]float64{}, Total: 0},
		// The case that made this test worth writing: the total is known
		// but the table holds no frequencies, which is what a half-written
		// or partially loaded file looks like. Without the skip guard every
		// tag counts as maximally rare, so two works sharing one tag score
		// a perfect 1.0 — the strongest possible claim from no evidence.
		// (The cases above skip anyway, because a zero-work total makes
		// every IDF zero; they are kept so the contract is stated once.)
		{DocFreq: map[int32]float64{}, Total: 1000},
		// A one-work corpus has no rarity to speak of.
		{DocFreq: map[int32]float64{1: 5}, Total: 1},
	} {
		_, _, err := s.Score(
			rank.Candidate{TagIDs: []int32{1}},
			[]rank.Candidate{{ID: 7, TagIDs: []int32{1}}},
			nil)
		if err != rank.ErrSkip {
			t.Fatalf("with %+v: err = %v, want rank.ErrSkip", s, err)
		}
	}
}

func TestIDFOverlapNameIsTheTuneKey(t *testing.T) {
	if got := (IDFWeightedOverlap{}).Name(); got != "idf_overlap" {
		t.Fatalf("Name() = %q, want %q", got, "idf_overlap")
	}
}
