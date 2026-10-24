package store

import (
	"context"
	"testing"
)

func algoTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestAlgoComparisonRefusesASelfComparison pins the refusal that keeps the
// standings meaningful.
//
// An A/B judgement between a tune and itself produces two identical lists,
// so the reader's answer is not evidence about anything. Worse, in the
// standings the row would count as a WIN and a LOSS for the same algorithm —
// a number that moves in both directions at once.
func TestAlgoComparisonRefusesASelfComparison(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)
	err := s.RecordAlgoComparison(ctx, "owner", AlgoComparison{
		SeedKind: "ao3_work", SeedID: 1, AlgoA: "recency", AlgoB: "recency", Winner: "a",
	})
	if err == nil {
		t.Fatal("recording a self-comparison succeeded; it must be refused")
	}
}

func TestAlgoComparisonRefusesAnUnknownWinner(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)
	err := s.RecordAlgoComparison(ctx, "owner", AlgoComparison{
		SeedKind: "ao3_work", SeedID: 1, AlgoA: "a", AlgoB: "b", Winner: "left",
	})
	if err == nil {
		t.Fatal("recorded a winner that is not a, b or neither")
	}
}

func TestAlgoComparisonRefusesAMissingSeedKind(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)
	err := s.RecordAlgoComparison(ctx, "owner", AlgoComparison{
		AlgoA: "a", AlgoB: "b", Winner: "a",
	})
	if err == nil {
		t.Fatal("recorded a judgement with no seed kind; it cannot be broken down later")
	}
}

// TestAlgoStandingsCountBothSides checks the arithmetic, including draws.
//
// A draw must count as a tie for BOTH algorithms. Dropping draws is the
// tempting simplification and it inflates every win rate as the reader grows
// more indifferent — precisely when the numbers matter least.
func TestAlgoStandingsCountBothSides(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)
	judgements := []AlgoComparison{
		{SeedKind: "ao3_work", SeedID: 1, AlgoA: "recency", AlgoB: "quality", Winner: "a"},
		{SeedKind: "ao3_work", SeedID: 2, AlgoA: "recency", AlgoB: "quality", Winner: "a"},
		{SeedKind: "ao3_work", SeedID: 3, AlgoA: "recency", AlgoB: "quality", Winner: "b"},
		{SeedKind: "ao3_work", SeedID: 4, AlgoA: "recency", AlgoB: "quality", Winner: "neither"},
	}
	for _, j := range judgements {
		if err := s.RecordAlgoComparison(ctx, "owner", j); err != nil {
			t.Fatalf("recording %v: %v", j, err)
		}
	}
	rows, err := s.AlgoStandings(ctx, "owner")
	if err != nil {
		t.Fatalf("standings: %v", err)
	}
	got := map[string]AlgoStanding{}
	for _, r := range rows {
		got[r.Algo] = r
	}
	rec, ok := got["recency"]
	if !ok {
		t.Fatalf("recency missing from standings: %v", rows)
	}
	if rec.Wins != 2 || rec.Losses != 1 || rec.Ties != 1 || rec.Judged != 4 {
		t.Fatalf("recency = %+v, want 2 wins / 1 loss / 1 tie / 4 judged", rec)
	}
	qual := got["quality"]
	if qual.Wins != 1 || qual.Losses != 2 || qual.Ties != 1 || qual.Judged != 4 {
		t.Fatalf("quality = %+v, want 1 win / 2 losses / 1 tie / 4 judged", qual)
	}
	// Wins first, so recency leads.
	if rows[0].Algo != "recency" {
		t.Fatalf("standings lead with %q, want recency (2 wins)", rows[0].Algo)
	}
}

// TestAlgoStandingsAreScopedToOneReader is the property that makes a
// standings table mean anything: two readers' aesthetic preferences are not
// each other's evidence.
func TestAlgoStandingsAreScopedToOneReader(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)
	if err := s.RecordAlgoComparison(ctx, "alice", AlgoComparison{
		SeedKind: "ao3_work", SeedID: 1, AlgoA: "recency", AlgoB: "quality", Winner: "a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAlgoComparison(ctx, "bob", AlgoComparison{
		SeedKind: "ao3_work", SeedID: 1, AlgoA: "recency", AlgoB: "quality", Winner: "b",
	}); err != nil {
		t.Fatal(err)
	}

	alice, err := s.AlgoStandings(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range alice {
		if r.Algo == "recency" && (r.Wins != 1 || r.Losses != 0) {
			t.Fatalf("alice's recency = %+v; bob's vote leaked in", r)
		}
	}
	// An empty key means every reader, which is what a single-user mirror
	// and the CLI want.
	all, err := s.AlgoStandings(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.Algo == "recency" && r.Judged != 2 {
			t.Fatalf("global recency = %+v, want 2 judged", r)
		}
	}
}

// TestAlgoVotesNeverReachTheTagLearner is the invariant this whole surface
// was deferred for.
//
// The work arena's judgements feed the tag learner: choosing work A over work
// B is evidence about the tags both carry. An algorithm preference is
// evidence about WEIGHTS. If a tune vote landed in arena_comparisons, the
// learner would read "this reader prefers recency" as "this reader likes the
// tags shared by this pair of works", and every tag weight downstream would
// carry a preference about something else.
//
// So this test records votes and asserts the arena's own tables did not move.
func TestAlgoVotesNeverReachTheTagLearner(t *testing.T) {
	ctx := context.Background()
	s := algoTestStore(t)

	count := func(table string) int {
		t.Helper()
		var n int
		if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		return n
	}
	beforeComparisons := count("arena_comparisons")
	beforeWeights := count("arena_user_tag_weights")

	for i := int64(1); i <= 5; i++ {
		if err := s.RecordAlgoComparison(ctx, "owner", AlgoComparison{
			SeedKind: "ao3_work", SeedID: i, AlgoA: "recency", AlgoB: "quality", Winner: "a",
		}); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	if got := count("arena_comparisons"); got != beforeComparisons {
		t.Fatalf("arena_comparisons moved %d -> %d: an algorithm vote reached the work arena, "+
			"where the tag learner reads it as evidence about two works' tags",
			beforeComparisons, got)
	}
	if got := count("arena_user_tag_weights"); got != beforeWeights {
		t.Fatalf("arena_user_tag_weights moved %d -> %d: an algorithm vote changed a tag weight",
			beforeWeights, got)
	}
	// And it DID land where it belongs.
	var n int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM algo_comparisons").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("algo_comparisons holds %d rows, want 5: the vote was not recorded at all", n)
	}
}

// The vote record must carry a verdict, and the verdict must distinguish three
// states: leads, behind, and not enough. A field that is always "none" would render
// the same page as a correct implementation on this deployment's data, so each state
// is pinned.
func TestTheVoteStandingCarriesAnEvidenceVerdict(t *testing.T) {
	s := interleaveStore(t)
	ctx := context.Background()
	// default leads tags 12-2 (p = 0.013), and hybrid draws with tags 3-3.
	for i := 0; i < 12; i++ {
		if err := s.RecordAlgoComparison(ctx, "owner1", AlgoComparison{
			SeedKind: "ao3_work", SeedID: int64(100 + i), AlgoA: "default", AlgoB: "tags", Winner: "a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.RecordAlgoComparison(ctx, "owner1", AlgoComparison{
			SeedKind: "ao3_work", SeedID: int64(200 + i), AlgoA: "default", AlgoB: "tags", Winner: "b",
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := s.RecordAlgoComparison(ctx, "owner1", AlgoComparison{
			SeedKind: "ao3_work", SeedID: int64(300 + i), AlgoA: "hybrid", AlgoB: "tags", Winner: "neither",
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.AlgoStandings(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]AlgoStanding{}
	for _, r := range rows {
		by[r.Algo] = r
	}
	if got := by["default"]; got.Evidence != "leads" || got.P >= 0.05 {
		t.Fatalf("default 12-2: evidence %q p = %v, want leads", got.Evidence, got.P)
	}
	if got := by["tags"]; got.Evidence != "behind" {
		t.Fatalf("tags 2-12: evidence %q, want behind", got.Evidence)
	}
	// hybrid's three judgements all tied: no decisive comparison, so nothing.
	if got := by["hybrid"]; got.Evidence != "none" || got.EvidenceWhy == "" {
		t.Fatalf("hybrid (3 ties): evidence %q why %q, want none with a reason", got.Evidence, got.EvidenceWhy)
	}
}
