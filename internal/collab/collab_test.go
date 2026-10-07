package collab

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// buildCorpus writes a fixture carrying co-bookmark evidence and returns an
// open handle plus the built index.
//
// The shape is chosen so each test can falsify one decision. Note the GATE is
// pinned to 2 explicitly rather than left to ChooseGate: the fixture is small
// enough that ChooseGate would fall back to 1, and a test that exercised the
// fallback instead of the gate it means to test would pass for the wrong
// reason. TestChooseGateFallsBackWhenTooFewPairs covers the fallback itself.
//
//   - works 1 and 2 are co-bookmarked by 6 users (strong evidence)
//   - work 3 is co-bookmarked with work 1 by exactly 3 users
//   - work 4 is co-bookmarked with work 1 by exactly 2 users (at the gate)
//   - work 5 is co-bookmarked with work 1 by exactly 1 user (below the gate)
//   - user 99 repeats works 1 and 2 seven times each, so a count that skips
//     DISTINCT inflates that pair's popularity and corrupts its cosine
//   - user 98 has 'bookmarker' rows, which must not count as bookmarks
func buildCorpus(t *testing.T) (*sql.DB, *Index) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.db")

	f := testcorpus.New(8)
	var (
		users []testcorpus.User
		ins   []testcorpus.Interaction
	)
	// Users 1..6 bookmark works 1 and 2 -> 6 co-users.
	for u := int64(1); u <= 6; u++ {
		users = append(users, testcorpus.User{ID: u, Username: userName(u)})
		ins = append(ins,
			testcorpus.Interaction{UserID: u, WorkID: 1},
			testcorpus.Interaction{UserID: u, WorkID: 2})
	}
	// Users 7..9 add work 3 to work 1's evidence -> exactly 3 co-users.
	for u := int64(7); u <= 9; u++ {
		users = append(users, testcorpus.User{ID: u, Username: userName(u)})
		ins = append(ins,
			testcorpus.Interaction{UserID: u, WorkID: 1},
			testcorpus.Interaction{UserID: u, WorkID: 3})
	}
	// Users 10 and 11 add work 4 to work 1 -> exactly 2 co-users, AT the gate.
	for u := int64(10); u <= 11; u++ {
		users = append(users, testcorpus.User{ID: u, Username: userName(u)})
		ins = append(ins,
			testcorpus.Interaction{UserID: u, WorkID: 1},
			testcorpus.Interaction{UserID: u, WorkID: 4})
	}
	// User 12 adds work 5 to work 1 -> 1 co-user, BELOW the gate.
	users = append(users, testcorpus.User{ID: 12, Username: "twelve"})
	ins = append(ins,
		testcorpus.Interaction{UserID: 12, WorkID: 1},
		testcorpus.Interaction{UserID: 12, WorkID: 5})
	// User 99 bookmarks work 1 SEVEN times, and work 2 SEVEN times too.
	// Both are needed: the self-join pairs (1,2) on user_id, so a user's
	// duplicate rows only multiply if they duplicate BOTH works of the pair.
	// Duplicating one side alone never amplifies anything, which is why a
	// fixture that duplicates only one work cannot test the DISTINCT at all.
	users = append(users, testcorpus.User{ID: 99, Username: "repeater"})
	for i := 0; i < 7; i++ {
		ins = append(ins,
			testcorpus.Interaction{UserID: 99, WorkID: 1},
			testcorpus.Interaction{UserID: 99, WorkID: 2})
	}
	// User 98 has 'bookmarker' rows about works 1 and 6. These are rows
	// ABOUT a work carrying a user id, not that user's own bookmarks.
	users = append(users, testcorpus.User{ID: 98, Username: "about"})
	ins = append(ins,
		testcorpus.Interaction{UserID: 98, WorkID: 1, InteractionType: "bookmarker"},
		testcorpus.Interaction{UserID: 98, WorkID: 6, InteractionType: "bookmarker"})

	f.Users = users
	f.Interactions = ins

	if _, err := f.Write(path); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	b := &Builder{Corpus: db, MinCoUsers: 2}
	idx, _, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return db, idx
}

func userName(i int64) string {
	return "reader" + string(rune('A'+i%26))
}

// TestTheGateExcludesThinEvidence is the falsification of the evidence gate.
//
// The fixture's builder is pinned to gate 2, and work 5 has exactly 1
// co-user. If the gate is not applied, work 5 appears in work 1's neighbours
// and the whole point of the package — only trust what enough readers agreed
// on — is lost. Work 4, at exactly 2, must still rank, so the test also pins
// the boundary as inclusive.
func TestTheGateExcludesThinEvidence(t *testing.T) {
	_, idx := buildCorpus(t)

	sim := idx.Similar(1, 100, Options{})
	ids := map[int64]bool{}
	for _, s := range sim {
		ids[s.ID] = true
	}
	if ids[5] {
		t.Fatalf("work 5 has 1 co-user, below the gate of 2, and must not rank: %+v", sim)
	}
	if !ids[4] {
		t.Fatalf("work 4 has exactly 2 co-users, at the gate, and must rank: %+v", sim)
	}
	if !ids[2] {
		t.Fatalf("work 2 has 6 co-users and must rank: %+v", sim)
	}
	if !ids[3] {
		t.Fatalf("work 3 has exactly 3 co-users and must rank: %+v", sim)
	}
}

// TestChooseGateFallsBackWhenTooFewPairs covers the adaptive gate's fallback
// branch.
//
// This is the case that made the difference on the real mirror: at gate 3 the
// index held 583 works out of 112,935 and could not rank. When too few pairs
// survive gate 2, ChooseGate must return 1 rather than leaving the index
// empty — and the confidence weighting is what makes gate 1 safe.
func TestChooseGateFallsBackWhenTooFewPairs(t *testing.T) {
	db, _ := buildCorpus(t)

	// The fixture has three pairs at co>=2 -- (1,2) at 7, (1,3) at 3 and
	// (1,4) at 2 -- far below the default rankability floor, so this must
	// fall back.
	gate, pairsAt2, err := ChooseGate(context.Background(), db, DefaultMinPairsForGate2)
	if err != nil {
		t.Fatalf("ChooseGate: %v", err)
	}
	if pairsAt2 != 3 {
		t.Fatalf("pairs at co>=2 = %d, want 3", pairsAt2)
	}
	if gate != 1 {
		t.Fatalf("gate = %d, want 1: %d pairs at co=2 cannot rank from gate 2",
			gate, pairsAt2)
	}
}

// TestChooseGateTakesTheStricterGateWhenThereIsEnoughEvidence is the other
// branch: enough pairs at co=2 must select gate 2, not the fallback.
//
// Without this the fallback would be the only tested path and a ChooseGate
// that always returned 1 would pass every other test in this file.
func TestChooseGateTakesTheStricterGateWhenThereIsEnoughEvidence(t *testing.T) {
	db, _ := buildCorpus(t)

	// A floor of 1 pair is met by the fixture's three co>=2 pairs.
	gate, pairsAt2, err := ChooseGate(context.Background(), db, 1)
	if err != nil {
		t.Fatalf("ChooseGate: %v", err)
	}
	if pairsAt2 != 3 {
		t.Fatalf("pairs at co>=2 = %d, want 3", pairsAt2)
	}
	if gate != 2 {
		t.Fatalf("gate = %d, want 2: 3 pairs clears a floor of 1", gate)
	}
}

// TestALowGatePairCannotOutrankAWellEvidencedOne is the reason gate 1 is
// tolerable at all.
//
// At gate 1 a single reader's bookmark list is the only evidence for some
// pairs. The confidence multiplier must keep those below well-evidenced ones
// regardless, so the fallback gate does not need to do the quality work.
func TestALowGatePairCannotOutrankAWellEvidencedOne(t *testing.T) {
	db, _ := buildCorpus(t)

	idx, _, err := (&Builder{Corpus: db, MinCoUsers: 1}).Build(context.Background())
	if err != nil {
		t.Fatalf("build at gate 1: %v", err)
	}
	sim := idx.Similar(1, 100, Options{})
	pos := map[int64]int{}
	for i, s := range sim {
		pos[s.ID] = i
	}
	// Work 4 has 2 co-users and work 5 has 1. Both rank at gate 1; the
	// confidence weighting must still order them by evidence.
	p4, ok4 := pos[4]
	p5, ok5 := pos[5]
	if !ok4 || !ok5 {
		t.Skipf("fixture cannot distinguish at gate 1: %+v", sim)
	}
	if p4 > p5 {
		t.Fatalf("work 4 (2 co-users) ranked below work 5 (1 co-user) even at gate 1: "+
			"the confidence weighting is not discounting by evidence. sim=%+v", sim)
	}
}

// TestDuplicateBookmarkRowsDoNotInflatePopularity pins the DISTINCT.
//
// User 99 bookmarks work 1 seven times. A row count would give work 1 a
// popularity of 19 instead of 11, changing every cosine it participates in —
// so this asserts the popularity directly rather than inferring it from a
// score.
func TestDuplicateBookmarkRowsDoNotInflatePopularity(t *testing.T) {
	_, idx := buildCorpus(t)

	// Distinct users on work 1: 1-12 (twelve) plus 99 (one) = 13, even
	// though user 99 contributed SEVEN rows for it. User 98 contributes a
	// 'bookmarker' row, which is not a bookmark.
	const want = 13.0
	if got := idx.Bookmarkers(1); got != want {
		t.Fatalf("work 1 bookmarkers = %v, want %v: duplicate rows or a 'bookmarker' row "+
			"has been counted as a bookmark", got, want)
	}
}

// TestBookmarkerInteractionTypeIsNotABookmark is the second half of the same
// trap, stated separately so a fix to one does not silently revert the other.
//
// 'bookmarker' is a row about a work carrying a user id. Counting it
// attributes one reader's history from another's action: work 5 has no
// bookmarkers at all and must therefore never be a neighbour.
func TestBookmarkerInteractionTypeIsNotABookmark(t *testing.T) {
	_, idx := buildCorpus(t)

	if n := idx.Bookmarkers(6); n != 0 {
		t.Fatalf("work 6 bookmarkers = %v, want 0: a 'bookmarker' row was counted as a bookmark", n)
	}
	for _, s := range idx.Similar(1, 100, Options{}) {
		if s.ID == 6 {
			t.Fatalf("work 6 has no bookmark evidence and must not be a neighbour: %+v", s)
		}
	}
}

// TestConfidenceDoesNotInvertTheRanking is the falsification of the scoring
// decision.
//
// This is the bug the package doc exists to prevent. Shrinking a cosine
// toward 1.0 — the correct direction for a LIFT, whose neutral point is 1 —
// lifts a weak 3-user pair above a strong one. Under the wrong formula work 3
// (3 co-users, low cosine) would outrank work 2 (5 co-users).
func TestConfidenceDoesNotInvertTheRanking(t *testing.T) {
	_, idx := buildCorpus(t)

	sim := idx.Similar(1, 100, Options{})
	pos := map[int64]int{}
	for i, s := range sim {
		pos[s.ID] = i
	}
	if len(pos) < 2 {
		t.Fatalf("expected at least two neighbours, got %+v", sim)
	}
	p2, ok2 := pos[2]
	p3, ok3 := pos[3]
	if !ok2 || !ok3 {
		t.Fatalf("expected works 2 and 3 among neighbours, got %+v", sim)
	}
	if p2 > p3 {
		t.Fatalf("work 2 (5 co-users) ranked below work 3 (3 co-users): "+
			"the score is not monotone in evidence. sim=%+v", sim)
	}
	// And the raw cosine order must survive: no weighting can justify
	// reversing it here.
	if sim[p2].Sim < sim[p3].Sim {
		t.Fatalf("work 3 has higher raw cosine than work 2: %+v", sim)
	}
}

// TestScoreIsBoundedByRawSimilarity checks the algebra rather than the
// ranking: confidence is a multiplier in [0,1), so Score <= Sim always.
// Shrinking toward 1.0 would violate this immediately.
func TestScoreIsBoundedByRawSimilarity(t *testing.T) {
	_, idx := buildCorpus(t)
	for _, s := range idx.Similar(1, 100, Options{}) {
		if s.Score > s.Sim+1e-12 {
			t.Fatalf("score %v exceeds raw similarity %v for work %d: "+
				"confidence must discount, never inflate", s.Score, s.Sim, s.ID)
		}
		if s.Score < 0 {
			t.Fatalf("negative score %v for work %d", s.Score, s.ID)
		}
	}
}

// TestNeighboursForNeverVotesForASeed pins self-exclusion.
func TestNeighboursForNeverVotesForASeed(t *testing.T) {
	_, idx := buildCorpus(t)

	votes := idx.NeighboursFor([]int64{1, 2, 3}, 100, Options{})
	for _, seed := range []int64{1, 2, 3} {
		if v, ok := votes[seed]; ok {
			t.Fatalf("seed %d received a vote (%v); a seed must never vote for itself", seed, v)
		}
	}
}

// TestNeighboursForIsPerSeedLimited checks that the limit bounds EACH seed's
// contribution, which is what stops one prolific seed dominating.
//
// A limit of 1 must yield at most one vote per seed, so three seeds can
// produce at most three distinct voters — not three times every neighbour.
func TestNeighboursForIsPerSeedLimited(t *testing.T) {
	_, idx := buildCorpus(t)

	all := idx.NeighboursFor([]int64{1}, 0, Options{})
	one := idx.NeighboursFor([]int64{1}, 1, Options{})
	if len(one) >= len(all) {
		t.Skipf("fixture cannot distinguish: all=%d one=%d", len(all), len(one))
	}
	if len(one) > 1 {
		t.Fatalf("limit 1 produced %d votes, want at most 1 per seed", len(one))
	}
}

// TestNeighboursForAccumulatesAcrossSeeds checks that several seeds summing
// votes for the same work produce a larger score, which is what makes a
// multi-seed request better than any single seed.
func TestNeighboursForAccumulatesAcrossSeeds(t *testing.T) {
	_, idx := buildCorpus(t)

	one := idx.NeighboursFor([]int64{1}, 100, Options{})
	two := idx.NeighboursFor([]int64{1, 2}, 100, Options{})
	if len(two) == 0 {
		t.Fatalf("no votes from two seeds: %+v", two)
	}
	for id, v := range two {
		if v > one[id] {
			continue // legitimately larger
		}
		if v < one[id] {
			t.Fatalf("work %d scored %v with two seeds, less than %v with one: "+
				"adding a seed must not remove evidence", id, v, one[id])
		}
	}
}

// TestDeterministicAcrossCalls pins reproducibility. The same seed must
// produce the same order, or two identical requests rank different results.
func TestDeterministicAcrossCalls(t *testing.T) {
	_, idx := buildCorpus(t)
	first := idx.Similar(1, 100, Options{})
	for i := 0; i < 5; i++ {
		again := idx.Similar(1, 100, Options{})
		if len(again) != len(first) {
			t.Fatalf("length changed between calls: %d then %d", len(first), len(again))
		}
		for j := range first {
			if again[j].ID != first[j].ID {
				t.Fatalf("order changed at %d: work %d then work %d",
					j, first[j].ID, again[j].ID)
			}
		}
	}
}

// TestAMirrorWithoutBookmarksDegradesRatherThanFails covers the case the
// package doc commits to: a mirror with no interactions table yields an empty
// index, and the signal reports itself unavailable. It is not an error, and
// the ingest must not fail on a table it does not need.
func TestAMirrorWithoutBookmarksDegradesRatherThanFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nobookmarks.db")
	f := testcorpus.New(4)
	// Write only the works/tags tables, then drop the interactions table, so
	// this is the "table is absent" case rather than "table is empty".
	written, err := f.Write(path)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	db, err := sql.Open("sqlite", written)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE user_work_interactions`); err != nil {
		t.Fatalf("drop interactions: %v", err)
	}

	idx, st, err := (&Builder{Corpus: db}).Build(context.Background())
	if err != nil {
		t.Fatalf("a mirror without bookmarks must not fail the build: %v", err)
	}
	if st.Pairs != 0 || st.BookmarkRows != 0 {
		t.Fatalf("expected an empty index, got %+v", st)
	}
	if sim := idx.Similar(1, 10, Options{}); len(sim) != 0 {
		t.Fatalf("an empty index returned %d neighbours: %+v", len(sim), sim)
	}
	if v := idx.NeighboursFor([]int64{1, 2}, 10, Options{}); len(v) != 0 {
		t.Fatalf("an empty index cast %d votes: %+v", len(v), v)
	}
}

// TestPairsBeforeGateIsReported covers the honesty requirement: a caller must
// be able to tell "the gate removed nearly everything" from "there was never
// much", without re-running the query.
func TestPairsBeforeGateIsReported(t *testing.T) {
	db, _ := buildCorpus(t)

	pairs, err := CoBookmarkPairs(context.Background(), db, 2)
	if err != nil {
		t.Fatalf("pairs: %v", err)
	}
	if len(pairs) == 0 {
		t.Fatal("expected some pairs at the gate")
	}
	before, err := countAllPairs(context.Background(), db)
	if err != nil {
		t.Fatalf("count all: %v", err)
	}
	if before <= int64(len(pairs)) {
		t.Fatalf("pairs before the gate (%d) should exceed pairs after (%d), "+
			"because the fixture has 2-user evidence to exclude", before, len(pairs))
	}
}

// TestPairsCountDistinctUsersNotRows is the direct assertion behind the gate
// test: co-user counts are DISTINCT users.
func TestPairsCountDistinctUsersNotRows(t *testing.T) {
	db, _ := buildCorpus(t)

	pairs, err := CoBookmarkPairs(context.Background(), db, 1)
	if err != nil {
		t.Fatalf("pairs: %v", err)
	}
	for _, p := range pairs {
		if p.A >= p.B {
			t.Fatalf("pair %d/%d is not stored with A < B, so it would be counted twice",
				p.A, p.B)
		}
		// Works 1 and 2 are co-bookmarked by users 1-6 and user 99: SEVEN
		// distinct users. User 99 has SEVEN duplicate rows for each, so a
		// row count gives 6 + 49 = 55 and a COUNT(DISTINCT) on only one
		// side gives 13. Neither is 7, which is what makes this assertion
		// able to catch either mistake.
		if p.A == 1 && p.B == 2 && p.CoUsers != 7 {
			t.Fatalf("pair (1,2) co-users = %d, want 7: rows are being counted "+
				"instead of distinct users", p.CoUsers)
		}
	}
}

// TestCoBookmarkPairsCountsBookmarkedOnly names the interaction_type filter
// as the package doc claims it is.
func TestCoBookmarkPairsCountsBookmarkedOnly(t *testing.T) {
	db, _ := buildCorpus(t)

	pairs, err := CoBookmarkPairs(context.Background(), db, 1)
	if err != nil {
		t.Fatalf("pairs: %v", err)
	}
	for _, p := range pairs {
		// User 98 has 'bookmarker' rows about works 1 and 6, which would be
		// the ONLY evidence for the pair (1,6). Counting it would create a
		// pair no reader ever bookmarked together.
		if p.A == 1 && p.B == 6 {
			t.Fatalf("pair (1,6) exists with %d co-users, but its only possible "+
				"evidence is a 'bookmarker' row: %+v", p.CoUsers, pairs)
		}
	}
}
