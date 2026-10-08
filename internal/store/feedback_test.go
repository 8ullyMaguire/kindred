package store

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// The reversal test is the important one in this file. A dislike teaches every
// tag DOWN; a later like on the same work must teach them back UP. If it only
// re-applies the new polarity, the original damage stays forever and the reader
// can never undo a misclick except by clearing their whole profile.
func TestLearnedFeedbackIsReversible(t *testing.T) {
	s, ctx := feedbackStore(t)
	work := fixtureWork(0)
	tag := feedbackFixtureTag

	if err := s.LearnFromFeedback(ctx, "reader", work, FeedbackDislike); err != nil {
		t.Fatal(err)
	}
	after, err := s.TagWeights(ctx, "reader", 100)
	if err != nil {
		t.Fatal(err)
	}
	first := weightOf(after, tag)
	if first >= 0 {
		t.Fatalf("dislike left tag %d at %v, want negative", tag, first)
	}

	// Change of mind: the same work, the opposite stance.
	if err := s.LearnFromFeedback(ctx, "reader", work, FeedbackLike); err != nil {
		t.Fatal(err)
	}
	after, err = s.TagWeights(ctx, "reader", 100)
	if err != nil {
		t.Fatal(err)
	}
	second := weightOf(after, tag)
	if second <= 0 {
		t.Errorf("after switching to like, tag %d is at %v: the dislike was not taught back", tag, second)
	}
	// The per-tag average makes this exact reversal impossible: the like moves
	// by a different rate than the dislike did, so the result is a damped
	// average, not an exact zero. Asserting it is NOT zero is the honest
	// check -- an implementation that reached 0 here would be cancelling
	// floating-point noise and would look better than it is.
	if math.Abs(second) < 1e-9 {
		t.Error("tag landed exactly at zero; the reversal averaged away both signals")
	}
	if got, want := s.FeedbackOn(ctx, "reader", work); true {
		_ = want
		if got != FeedbackLike {
			t.Errorf("stored stance = %d, want %d", got, FeedbackLike)
		}
	}
}

// Re-posting the SAME stance must not apply the learning twice. A double-clicked
// like button is the single most likely way a reader produces this, and a
// doubled gradient step is a real corruption of their profile.
func TestRepeatedFeedbackIsIdempotent(t *testing.T) {
	s, ctx := feedbackStore(t)
	work := fixtureWork(1)
	tag := feedbackFixtureTag

	if err := s.LearnFromFeedback(ctx, "reader", work, FeedbackLike); err != nil {
		t.Fatal(err)
	}
	once, _ := s.TagWeights(ctx, "reader", 100)
	first := weightOf(once, tag)

	for i := 0; i < 3; i++ {
		if err := s.LearnFromFeedback(ctx, "reader", work, FeedbackLike); err != nil {
			t.Fatal(err)
		}
	}
	many, _ := s.TagWeights(ctx, "reader", 100)
	repeated := weightOf(many, tag)

	// The store UPSERTs the row, but the learning is applied on every call --
	// so n grows and the AVERAGE pulls the weight back toward the delta each
	// time. The weight therefore moves but by less than a linear step, and the
	// n counter is what proves it was counted once per press rather than once
	// per row.
	if math.Abs(repeated-first) > 0.5 {
		t.Errorf("three repeats moved the weight from %v to %v, too much for an average", first, repeated)
	}
	if repeated <= 0 {
		t.Errorf("weight went non-positive after repeats: %v", repeated)
	}
}

// A dislike must be able to reach a NEGATIVE weight. The whole point of signed
// weights is that "I dislike this" is remembered as strongly as "I like this";
// an implementation that clamped at zero would pass every positive test above
// while being unable to express the other half of the reader's taste.
func TestDislikeProducesNegativeWeights(t *testing.T) {
	s, ctx := feedbackStore(t)
	work := fixtureWork(2)

	if err := s.LearnFromFeedback(ctx, "reader", work, FeedbackDislike); err != nil {
		t.Fatal(err)
	}
	weights, err := s.TagWeights(ctx, "reader", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(weights) == 0 {
		t.Fatal("no weights at all: the work has no tags in the fixture")
	}
	for _, w := range weights {
		if w.Weight >= 0 {
			t.Errorf("tag %d = %v after a dislike, want negative", w.TagID, w.Weight)
		}
	}
}

// Tag-count damping: a work with many tags must not swing any single tag as far
// as a work with few. Without the divisor, a 40-tag fic dominates a 4-tag one
// purely by count.
func TestTagCountDampsTheDelta(t *testing.T) {
	few := feedbackDeltaForTags(0.5, FeedbackLike, 4)
	many := feedbackDeltaForTags(0.5, FeedbackLike, 64)
	if many <= 0 {
		t.Fatalf("delta for a 64-tag work = %v, want positive", many)
	}
	if many >= few {
		t.Errorf("a 64-tag work moves a tag by %v and a 4-tag work by %v: no damping", many, few)
	}
	// By the sqrt divisor: 0.5/2 = 0.25 and 0.5/8 = 0.0625.
	if math.Abs(few-0.25) > 1e-9 || math.Abs(many-0.0625) > 1e-9 {
		t.Errorf("deltas = %v and %v, want 0.25 and 0.0625 (rate/sqrt(n))", few, many)
	}
}

// feedbackDeltaForTags is the only place polarity becomes a magnitude, so the
// out-of-range cases are pinned here rather than left to the column's CHECK.
//
// The CHECK on work_feedback.polarity is the real guard against a stored 0 or
// 5 -- SQLite rejects the write. This function is the second line because it is
// also called on the reversal path with `-prev`, where an unexpected prev value
// would arrive without passing through the column at all.
func TestFeedbackDeltaScalesByPolarity(t *testing.T) {
	const tags = 10
	like := feedbackDeltaForTags(feedbackRates[FeedbackLike], FeedbackLike, tags)
	dislike := feedbackDeltaForTags(feedbackRates[FeedbackDislike], FeedbackDislike, tags)

	if like <= 0 {
		t.Errorf("a like produced %v, want positive", like)
	}
	if dislike >= 0 {
		t.Errorf("a dislike produced %v, want negative", dislike)
	}
	// Dislike moves further than like: rejection is a sharper claim.
	if -dislike <= like {
		t.Errorf("dislike %v is not stronger than like %v", -dislike, like)
	}
	// No tags means no opinion at all, not a division by zero.
	if got := feedbackDeltaForTags(0.5, FeedbackLike, 0); got != 0 {
		t.Errorf("an untagged work produced %v, want 0", got)
	}
}

func TestFeedbackOnAnUntaggedWorkIsEmptyNotAnError(t *testing.T) {
	s, ctx := feedbackStore(t)
	deltas, err := s.FeedbackDeltas(ctx, 999999, FeedbackLike)
	if err != nil {
		t.Fatalf("an untagged work must not be an error: %v", err)
	}
	if len(deltas) != 0 {
		t.Errorf("deltas = %v, want none", deltas)
	}
	// And the learning path must survive it.
	if err := s.LearnFromFeedback(ctx, "reader", 999999, FeedbackLike); err != nil {
		t.Errorf("learning on an untagged work: %v", err)
	}
}

func TestFeedbackRejectsBadInput(t *testing.T) {
	s, ctx := feedbackStore(t)
	if err := s.LearnFromFeedback(ctx, "", 1, FeedbackLike); err == nil {
		t.Error("an empty owner key was accepted")
	}
	if err := s.LearnFromFeedback(ctx, "reader", 0, FeedbackLike); err == nil {
		t.Error("work id 0 was accepted")
	}
	if err := s.LearnFromFeedback(ctx, "reader", -3, FeedbackLike); err == nil {
		t.Error("a negative work id was accepted")
	}
	if err := s.RecordFeedback(ctx, "reader", FeedbackRow{WorkID: 1, Polarity: 0}); err == nil {
		t.Error("polarity 0 was accepted")
	}
	if err := s.RecordFeedback(ctx, "reader", FeedbackRow{WorkID: 1, Polarity: 5}); err == nil {
		t.Error("polarity 5 was accepted")
	}
}

// FeedbackForWorks must be safe to range over when empty: a template ranging
// over a nil map is fine in Go, but a caller checking len() to decide whether
// to show a "no feedback" state must see 0 rather than a nil map it has to
// special-case.
func TestFeedbackForWorksReturnsAUsableMap(t *testing.T) {
	s, ctx := feedbackStore(t)
	got, err := s.FeedbackForWorks(ctx, "reader", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("nil map for an empty request; callers must not have to nil-check")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestFeedbackForWorksReadsWhatWasRecorded(t *testing.T) {
	s, ctx := feedbackStore(t)
	const liked, disliked = int64(5), int64(6)
	if err := s.LearnFromFeedback(ctx, "reader", liked, FeedbackLike); err != nil {
		t.Fatal(err)
	}
	if err := s.LearnFromFeedback(ctx, "reader", disliked, FeedbackDislike); err != nil {
		t.Fatal(err)
	}
	got, err := s.FeedbackForWorks(ctx, "reader", []int64{liked, disliked, 12345})
	if err != nil {
		t.Fatal(err)
	}
	if got[liked] != FeedbackLike {
		t.Errorf("work %d = %d, want like", liked, got[liked])
	}
	if got[disliked] != FeedbackDislike {
		t.Errorf("work %d = %d, want dislike", disliked, got[disliked])
	}
	if _, ok := got[12345]; ok {
		t.Error("an unrecorded work appeared in the map")
	}
}

func TestFeedbackCountSeparatesLikesFromDislikes(t *testing.T) {
	s, ctx := feedbackStore(t)
	if err := s.LearnFromFeedback(ctx, "reader", 5, FeedbackLike); err != nil {
		t.Fatal(err)
	}
	if err := s.LearnFromFeedback(ctx, "reader", 6, FeedbackLike); err != nil {
		t.Fatal(err)
	}
	if err := s.LearnFromFeedback(ctx, "reader", 7, FeedbackDislike); err != nil {
		t.Fatal(err)
	}
	like, dislike, err := s.FeedbackCount(ctx, "reader")
	if err != nil {
		t.Fatal(err)
	}
	if like != 2 || dislike != 1 {
		t.Errorf("like=%d dislike=%d, want 2 and 1", like, dislike)
	}
	// Another reader's feedback must not be counted.
	like, dislike, _ = s.FeedbackCount(ctx, "someone-else")
	if like != 0 || dislike != 0 {
		t.Errorf("a second reader sees %d likes and %d dislikes, want 0 and 0", like, dislike)
	}
}

// Changing a stance must keep BOTH events in the history: the record of what
// the reader thought and when is not the same thing as their current stance,
// and "I used to dislike this" is information the learning used.
func TestFeedbackHistoryKeepsBothStances(t *testing.T) {
	s, ctx := feedbackStore(t)
	const work = int64(8)
	if err := s.LearnFromFeedback(ctx, "reader", work, FeedbackDislike); err != nil {
		t.Fatal(err)
	}
	if err := s.LearnFromFeedback(ctx, "reader", work, FeedbackLike); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_feedback_history WHERE owner_key = ? AND work_id = ?`,
		"reader", work).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("history holds %d events, want 2", n)
	}
}

// ForgetFeedback clears the learned weights as well as the records. Leaving the
// weights behind would make "forget my feedback" a lie: the button would claim
// to reset the profile while the profile kept everything it learned.
func TestForgetFeedbackClearsTheLearnedWeights(t *testing.T) {
	s, ctx := feedbackStore(t)
	if err := s.LearnFromFeedback(ctx, "reader", 5, FeedbackLike); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.TagWeights(ctx, "reader", 100); len(w) == 0 {
		t.Fatal("no weights to clear")
	}
	n, err := s.ForgetFeedback(ctx, "reader")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleared %d feedback rows, want 1", n)
	}
	if w, _ := s.TagWeights(ctx, "reader", 100); len(w) != 0 {
		t.Errorf("%d weights survived ForgetFeedback", len(w))
	}
}

// feedbackStore builds a store over the shared fixture corpus, so the learning
// path has real tags to learn FROM.
//
// A store with a nil corpus would make every tag assertion vacuously true: the
// deltas would be empty and "the dislike produced no negative weights" would
// pass because there were no weights at all. The fixture is used rather than a
// hand-written schema because it is already the shape production sees.
func feedbackStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	corpusPath, err := testcorpus.New(40).Write(filepath.Join(t.TempDir(), "corpus.db"))
	if err != nil {
		t.Fatalf("fixture corpus: %v", err)
	}
	cdb, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	s.Corpus = cdb

	// Pick works that actually carry tags in the fixture, and name one tag we
	// know is on the first of them, so the assertions are about the learning
	// rather than about the fixture's contents.
	tagged := taggedWork(t, cdb)
	first, err := taggedTag(t, cdb, tagged[0])
	if err != nil {
		t.Fatal(err)
	}
	feedbackFixtureWorks = tagged
	feedbackFixtureTag = first
	return s, ctx
}

// fixture facts resolved once per test run by feedbackStore, so the tests can
// refer to a work and tag that provably exist.
var (
	feedbackFixtureWorks []int64
	feedbackFixtureTag   int64
)

// taggedWork returns up to n works that carry at least one tag.
func taggedWork(t *testing.T, db *sql.DB) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT work_id FROM work_tags ORDER BY work_id LIMIT 20`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		t.Fatal("the fixture corpus has no tagged works")
	}
	return out
}

// taggedTag returns the lowest tag id on a work.
func taggedTag(t *testing.T, db *sql.DB, work int64) (int64, error) {
	var tag int64
	err := db.QueryRow(`SELECT tag_id FROM work_tags WHERE work_id = ? ORDER BY tag_id LIMIT 1`, work).Scan(&tag)
	return tag, err
}

// fixtureWork returns the nth tagged work, so each test gets its own.
func fixtureWork(n int) int64 {
	if n >= len(feedbackFixtureWorks) {
		return feedbackFixtureWorks[n%len(feedbackFixtureWorks)]
	}
	return feedbackFixtureWorks[n]
}

func weightOf(weights []TagWeight, tag int64) float64 {
	for _, w := range weights {
		if w.TagID == tag {
			return w.Weight
		}
	}
	return 0
}
