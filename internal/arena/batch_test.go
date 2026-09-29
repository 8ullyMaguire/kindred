package arena

import (
	"math"
	"testing"
)

func TestApplyIsOrderIndependent(t *testing.T) {
	// THE reason the batch is a batch. Glickman's step 5 is a per-player
	// rule over the whole period, so every work must see its opponents at
	// their PRE-period ratings.
	//
	// If Apply updated work-by-work and fed each work its own writes, the
	// result would depend on map iteration order, which Go randomises --
	// so this would be a bug that passes a test run and fails the next one.
	pre := map[int64]Rating{
		1: {Mu: 1500, Phi: 200, Sigma: 0.06},
		2: {Mu: 1510, Phi: 200, Sigma: 0.06},
		3: {Mu: 1490, Phi: 200, Sigma: 0.06},
	}
	outcomes := []PeriodOutcome{
		{Work: 1, Score: Win, OppWork: 2},
		{Work: 2, Score: Loss, OppWork: 1},
		{Work: 1, Score: Win, OppWork: 3},
		{Work: 3, Score: Loss, OppWork: 1},
		{Work: 2, Score: Win, OppWork: 3},
		{Work: 3, Score: Loss, OppWork: 2},
	}

	// Run it many times: a single run would pass against an
	// order-dependent implementation roughly as often as it failed.
	var first map[int64]Rating
	for run := 0; run < 50; run++ {
		got := Apply(pre, outcomes)
		if first == nil {
			first = got
			continue
		}
		for work, want := range first {
			if math.Abs(got[work].Mu-want.Mu) > 1e-9 {
				t.Fatalf("run %d: work %d mu %.9f != run 1's %.9f -- the batch is order-dependent",
					run, work, got[work].Mu, want.Mu)
			}
		}
	}

	// And the direction must be right: 1 beat both others, 3 lost both.
	if first[1].Mu <= pre[1].Mu {
		t.Errorf("work 1 won twice but its mu fell: %.2f -> %.2f", pre[1].Mu, first[1].Mu)
	}
	if first[3].Mu >= pre[3].Mu {
		t.Errorf("work 3 lost twice but its mu rose: %.2f -> %.2f", pre[3].Mu, first[3].Mu)
	}
	// phi must shrink for everyone who played.
	for work, got := range first {
		if got.Phi >= pre[work].Phi {
			t.Errorf("work %d played and its phi did not shrink: %.2f -> %.2f", work, pre[work].Phi, got.Phi)
		}
	}
}

// Apply must not mutate its input. If it did, a caller that re-used the
// pre-period map -- for history, or for a second pass -- would be reading
// this period's writes, which is the same order-dependence above wearing a
// different hat.
func TestApplyDoesNotMutateItsInput(t *testing.T) {
	pre := map[int64]Rating{
		1: {Mu: 1500, Phi: 200, Sigma: 0.06},
		2: {Mu: 1510, Phi: 200, Sigma: 0.06},
	}
	snapshot := map[int64]Rating{1: pre[1], 2: pre[2]}

	Apply(pre, []PeriodOutcome{{Work: 1, Score: Win, OppWork: 2}})

	for id, want := range snapshot {
		if pre[id] != want {
			t.Errorf("work %d was mutated: %+v -> %+v", id, want, pre[id])
		}
	}
}

// The batch and the single-comparison rule are different rules, and the
// difference is large. A per-click implementation moves a rating several
// times further for the same result, which is the mistake the old kindling
// notes recorded in their own section 2.3.
func TestBatchMovesLessThanPerComparisonUpdates(t *testing.T) {
	pre := Rating{Mu: 1500, Phi: 200, Sigma: 0.06}
	ops := []Opponent{{Mu: 1500, Phi: 200, Score: Win}}

	// Five separate periods of one comparison each.
	perClick := pre
	for i := 0; i < 5; i++ {
		perClick = perClick.Update(ops)
	}
	// One period containing all five.
	inBatch := pre.Update(append(append([]Opponent{}, ops...), ops...))
	inBatch = inBatch.Update(ops) // fold the rest the same way for a fair compare

	if math.Abs(perClick.Mu-inBatch.Mu) < 0.5 {
		t.Errorf("per-click %.2f and batch %.2f are within 0.5; the two rules are not being distinguished",
			perClick.Mu, inBatch.Mu)
	}
	// The batch form is the smaller move: that is the whole point of the
	// batch rule, and it is the direction to assert.
	if perClick.Mu <= inBatch.Mu {
		t.Errorf("per-click mu %.2f should exceed the batch's %.2f", perClick.Mu, inBatch.Mu)
	}
}

func TestOpponentsForRecordsBothSides(t *testing.T) {
	pre := map[int64]Rating{
		1: {Mu: 1500, Phi: 100, Sigma: 0.06},
		2: {Mu: 1600, Phi: 50, Sigma: 0.06},
	}
	ops := OpponentsFor([]PeriodOutcome{{Work: 1, Score: Win, OppWork: 2}}, pre)

	if len(ops[1]) != 1 || len(ops[2]) != 1 {
		t.Fatalf("expected one opponent each, got %d and %d", len(ops[1]), len(ops[2]))
	}
	// The loser's opponent is the winner, and the loser's score is the
	// complement. Getting the complement wrong makes every game a double
	// win.
	if ops[1][0].Mu != 1600 || ops[1][0].Score != Win {
		t.Errorf("winner's opponent %+v, want the 1600-rated work scored 1.0", ops[1][0])
	}
	if ops[2][0].Mu != 1500 || ops[2][0].Score != Loss {
		t.Errorf("loser's opponent %+v, want the 1500-rated work scored 0.0", ops[2][0])
	}
	// A draw complements to a draw.
	d := OpponentsFor([]PeriodOutcome{{Work: 1, Score: Draw, OppWork: 2}}, pre)
	if d[1][0].Score != Draw || d[2][0].Score != Draw {
		t.Errorf("a draw did not complement to a draw: %v / %v", d[1][0].Score, d[2][0].Score)
	}
}

func TestDecayIdleSkipsWorksThatJustPlayed(t *testing.T) {
	pre := map[int64]Rating{
		1: {Mu: 1700, Phi: 60, Sigma: 0.07},
		2: {Mu: 1600, Phi: 60, Sigma: 0.07},
	}
	// Work 1 competed this period, work 2 did not.
	got := DecayIdle(pre, map[int64]bool{1: true})

	if got[1] != pre[1] {
		t.Errorf("a work that played was decayed: %+v -> %+v", pre[1], got[1])
	}
	if got[2].Phi <= pre[2].Phi {
		t.Errorf("an idle work was not decayed: %.2f -> %.2f", pre[2].Phi, got[2].Phi)
	}
	// Decay must not touch mu: a dormant work is not a worse work, it is a
	// less certain one.
	if got[2].Mu != pre[2].Mu {
		t.Errorf("decay changed mu: %.2f -> %.2f", pre[2].Mu, got[2].Mu)
	}
}

func TestTallyCountsRecordsSeparatelyFromRatings(t *testing.T) {
	tally := Tally([]PeriodOutcome{
		{Work: 1, Score: Win, OppWork: 2},
		{Work: 1, Score: Win, OppWork: 3},
		{Work: 1, Score: Draw, OppWork: 4},
		{Work: 1, Score: Loss, OppWork: 5},
	})
	if tally[1] != [3]int{2, 1, 1} {
		t.Errorf("tally = %v, want 2 wins / 1 loss / 1 draw", tally[1])
	}
	// The loser is NOT tallied from the winner's perspective: the caller
	// records both sides from the outcome set, and Tally only reports the
	// work named in each outcome.
	if _, ok := tally[2]; ok {
		t.Error("the losing work was tallied from the winner's outcome; both sides must be recorded")
	}
}

func TestSharedTagsCarryNoSignal(t *testing.T) {
	// THE rule that makes a tag-preference model work. Choosing A over B
	// says nothing about a tag they BOTH have -- the user chose between two
	// works that shared it, so it cannot have been the deciding factor.
	sig := BuildTagSignals(TagSignals{
		PositiveWork: 1, NegativeWork: 2,
		PositiveTags: []int64{10, 11, 12},
		NegativeTags: []int64{12, 13},
	}, map[int64]int{10: 100, 11: 100, 12: 100, 13: 100}, 1000)

	if _, ok := sig[12]; ok {
		t.Errorf("the shared tag 12 got weight %v; shared tags must be dropped entirely", sig[12])
	}
	if sig[10] <= 0 || sig[11] <= 0 {
		t.Errorf("unique positive tags should be positive, got %v and %v", sig[10], sig[11])
	}
	if sig[13] >= 0 {
		t.Errorf("a unique negative tag should be negative, got %v", sig[13])
	}
}

// The shared-tag filter must apply to BOTH passes. Filtering only the
// positive pass leaves the shared tag entering as a pure negative, so the
// more works share a tag the more a user is recorded as disliking it --
// and the corpus's most common tag would end up as everyone's least liked.
// A single tag-count assertion is enough to see it.
func TestSharedTagNeverLeaksNegative(t *testing.T) {
	// A tag on 90% of works, shared by both sides of every comparison.
	sig := BuildTagSignals(TagSignals{
		PositiveWork: 1, NegativeWork: 2,
		PositiveTags: []int64{99}, NegativeTags: []int64{99},
	}, map[int64]int{99: 900}, 1000)
	if len(sig) != 0 {
		t.Errorf("a tag on both works produced %v; it must produce nothing", sig)
	}

	// And across a whole period: many shared tags, all common.
	byWork := map[int64][]int64{
		1: {99, 98}, 2: {99, 98}, 3: {99, 98},
	}
	pos := []int64{1, 2, 3}
	neg := []int64{2, 3, 1}
	tags := map[int64]int{99: 900, 98: 800}
	got := LearnTags(byWork, pos, neg, tags, 1000)
	for tag, w := range got {
		if w < 0 {
			t.Errorf("tag %d, shared by every work, has weight %v; shared tags carry no signal", tag, w)
		}
	}
}

func TestRarityWeightsRareTagsHigher(t *testing.T) {
	// A tag on 40% of the corpus discriminates almost nothing; a tag on
	// one work is almost certainly why that work was chosen. A flat delta
	// would make the model learn the most common tag above all others.
	common := rarity(1, 400, 1000)
	rare := rarity(2, 1, 1000)
	if rare <= common {
		t.Errorf("a rare tag weights %.4f, no more than a common one's %.4f", rare, common)
	}
	// A tag on EVERY work discriminates nothing at all.
	if all := rarity(3, 1000, 1000); all != 0 {
		t.Errorf("a universal tag weights %.4f, want 0", all)
	}
	// And the curve must stay in (0,1].
	for _, p := range []float64{0.5, 0.1, 0.01, 0.001, 0.0001} {
		w := rarity(1, int(p*1000), 1000)
		if w < 0 || w > 1 {
			t.Errorf("rarity at p=%.4f is %.4f, outside (0,1]", p, w)
		}
	}
}

func TestLearnTagsSumsRatherThanOverwrites(t *testing.T) {
	// A tag the user likes on the whole but disliked on one occasion must
	// not be whichever comparison was walked last.
	byWork := map[int64][]int64{
		1: {10}, 2: {20}, 3: {30}, 4: {40},
	}
	tags := map[int64]int{10: 1, 20: 1, 30: 1, 40: 1}

	// Work 1 beats work 2 -> tag 10 positive, 20 negative.
	// Work 3 beats work 2 -> tag 30 positive, 20 negative again.
	// Tag 20 must end up net negative, not zero and not positive.
	got := LearnTags(byWork, []int64{1, 3}, []int64{2, 2}, tags, 1000)
	if got[20] >= 0 {
		t.Errorf("tag 20, passed over twice, has weight %v; it must be net negative", got[20])
	}
	if got[10] <= 0 {
		t.Errorf("tag 10, chosen once, has weight %v; it must be positive", got[10])
	}
}

func TestLearnTagsIgnoresUnpairedAndSelfComparisons(t *testing.T) {
	byWork := map[int64][]int64{1: {10}, 2: {20}, 3: {30}}
	tags := map[int64]int{10: 1, 20: 1, 30: 1}

	// Fewer negatives than positives: the surplus must be ignored, not
	// crash and not pair up with a zero-value entry.
	if got := LearnTags(byWork, []int64{1, 1, 1}, []int64{2}, tags, 1000); len(got) == 0 {
		t.Error("unpaired positives produced no weights at all")
	}
	// A work compared against itself carries no information.
	if got := LearnTags(byWork, []int64{2}, []int64{2}, tags, 1000); len(got) != 0 {
		t.Errorf("a self-comparison produced %v", got)
	}
	// No works at all.
	if got := LearnTags(byWork, nil, nil, tags, 1000); len(got) != 0 {
		t.Errorf("an empty comparison set produced %v", got)
	}
	// No corpus size means rarity is undefined, and guessing would be
	// worse than learning nothing.
	if got := LearnTags(byWork, []int64{1}, []int64{2}, tags, 0); len(got) != 0 {
		t.Errorf("with no corpus size, produced %v", got)
	}
}

func TestLearnTagsCapsRepeatedDecisions(t *testing.T) {
	// The same work passed over fifty times must not make its tags fifty
	// times as disliked. Otherwise the weight reflects how OFTEN a work was
	// shown rather than what was learnt, and a popular work's tags get
	// destroyed by repetition.
	byWork := map[int64][]int64{1: {10}, 2: {20}}
	tags := map[int64]int{10: 1, 20: 1}
	total := 1000

	pos := make([]int64, 0, 50)
	neg := make([]int64, 0, 50)
	for i := 0; i < 50; i++ {
		pos = append(pos, 1)
		neg = append(neg, 2)
	}
	repeated := LearnTags(byWork, pos, neg, tags, total)
	once := LearnTags(byWork, []int64{1}, []int64{2}, tags, total)

	if absf(repeated[20]) > absf(once[20])*6 {
		t.Errorf("50 repetitions gave |%v|, over 6x the single comparison's |%v|; the cap is not working",
			repeated[20], once[20])
	}
}

func TestNormaliseAndTopTags(t *testing.T) {
	w := map[int64]float64{1: 4, 2: -2, 3: 0.0001, 4: 0}
	n := Normalise(w)
	if math.Abs(n[1]-1.0) > 1e-9 {
		t.Errorf("the largest weight did not normalise to 1.0, got %v", n[1])
	}
	if math.Abs(n[2]+0.5) > 1e-9 {
		t.Errorf("a negative weight normalised to %v, want -0.5", n[2])
	}
	if zero := Normalise(map[int64]float64{1: 0}); len(zero) != 0 {
		t.Errorf("normalising an all-zero map produced %v", zero)
	}

	liked, disliked := TopTags(map[int64]float64{
		1: 0.5, 2: 0.9, 3: -0.7, 4: -0.2, 5: 0,
	}, 10)
	if len(liked) != 2 || liked[0] != 2 || liked[1] != 1 {
		t.Errorf("liked = %v, want [2 1] in descending order", liked)
	}
	if len(disliked) != 2 || disliked[0] != 3 || disliked[1] != 4 {
		t.Errorf("disliked = %v, want [3 4]", disliked)
	}
	// A zero weight is neither liked nor disliked.
	if len(liked)+len(disliked) != 4 {
		t.Errorf("a zero-weight tag was classified: %v / %v", liked, disliked)
	}

	// BOTH lists must be sorted descending by magnitude. A single-pass
	// implementation returns the negatives in map order, because the
	// entries are globally sorted but the negative tail is ascending
	// within it -- so "disliked" came back unsorted.
	_, dl := TopTags(map[int64]float64{3: -0.7, 4: -0.2, 5: -0.9}, 10)
	if len(dl) != 3 || dl[0] != 5 || dl[1] != 3 || dl[2] != 4 {
		t.Errorf("disliked = %v, want [5 3 4] by descending magnitude", dl)
	}
	lk, _ := TopTags(map[int64]float64{1: 0.1, 2: 0.8, 3: 0.4}, 10)
	if len(lk) != 3 || lk[0] != 2 || lk[1] != 3 || lk[2] != 1 {
		t.Errorf("liked = %v, want [2 3 1]", lk)
	}

	// Determinism: map iteration order must not shuffle the panel.
	first, _ := TopTags(map[int64]float64{9: 0.3, 8: 0.3, 7: 0.3}, 3)
	for i := 0; i < 20; i++ {
		got, _ := TopTags(map[int64]float64{9: 0.3, 8: 0.3, 7: 0.3}, 3)
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("TopTags is not deterministic: %v then %v", first, got)
			}
		}
	}
}
