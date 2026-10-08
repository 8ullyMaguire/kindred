package store

// Like/dislike feedback, and the tag learning it drives.
//
// ## Why this is a table and not a field on seen_works
//
// seen_works answers "have I shown or read this". Feedback answers "did I
// LIKE this", which is a different and much stronger statement: a reader who
// read a fic and disliked it wants it remembered as a negative, not merely
// hidden. Folding the two together would mean a later "mark unread" silently
// erased a dislike, which is the opposite of what the reader meant.
//
// ## Why it learns through the arena's existing path
//
// The per-reader tag weights in arena_user_tag_weights, their n-damped
// averaging, and arena.LearnTags' distinguishing-tags-only rule are already
// written, already tested, and already wired into two call sites. Like and
// dislike express the same preference in a different gesture, so they push
// through the same machinery rather than a second, parallel one.
//
// The alternative -- a separate online learner for likes -- would mean two
// weight tables for one reader's taste, two normalisations, two decay rates,
// and a ranking that summed a number each had moved independently. That is
// the shape of bug where every individual component is right and the result
// is noise. One table, one learner.
//
// ## What a like and a dislike are NOT
//
// Neither is an arena comparison. A comparison is a forced choice between two
// specific works, so the tags that taught anything are the ones that DISTINGUISH
// them. A like is an absolute judgement about one work, with nothing to
// distinguish it from -- so every tag on it is evidence, including the twenty
// that would have been ignored in a comparison. This is the one place the two
// paths genuinely differ and it is why RecordFeedback does not call LearnTags.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Feedback polarity. The stored value, not an enum name, because the column
// is read by the learning code and by SQL that groups by it.
const (
	// FeedbackLike is a positive: the reader wants more like this.
	FeedbackLike = 1
	// FeedbackDislike is a negative: the reader wants less like this.
	FeedbackDislike = -1
)

// Feedback learning rates, as a multiplier on the arena's per-tag signal.
//
// The values are the ratio between "I compared two fics and picked one" and
// "I pressed like". A comparison is a much stronger signal -- it is forced,
// deliberated, and specifically about the difference between two works -- so
// a like must move the weights less per event or a reader who spams the button
// ends up with a sharper profile than one who thinks at the arena.
//
// 0.5 for a like (deliberate but undirected: every tag votes) and 0.7 for a
// dislike (a stronger claim about fewer works: people reject what they have
// seen far more sharply than they love what they have seen).
var feedbackRates = map[int]float64{
	FeedbackLike:    0.5,
	FeedbackDislike: 0.7,
}

// FeedbackRow is one recorded like or dislike.
type FeedbackRow struct {
	WorkID    int64
	Polarity  int
	CreatedAt time.Time
	// Source names where the gesture came from ("web", "api"), so a later
	// analysis can tell a deliberate press from a bulk import. It is stored
	// rather than inferred because there is no way to tell them apart
	// afterwards, and a bulk import of 200 ratings would otherwise be
	// indistinguishable from a reader who genuinely pressed 200 times.
	Source string
}

// RecordFeedback stores a like or dislike and returns the tag deltas the
// caller should apply.
//
// The delta is returned rather than applied here because this is the pure
// part: the learning arithmetic is testable without a database, and the store
// only owns persistence. postFeedback applies both, in that order, so a reader
// never sees their own dislike recorded without the learning that goes with it.
func (s *Store) RecordFeedback(ctx context.Context, ownerKey string, f FeedbackRow) error {
	if ownerKey == "" {
		return errors.New("feedback: an owner key is required")
	}
	if f.WorkID <= 0 {
		return fmt.Errorf("feedback: work id must be positive, got %d", f.WorkID)
	}
	if _, ok := feedbackRates[f.Polarity]; !ok {
		return fmt.Errorf("feedback: polarity must be %d or %d, got %d",
			FeedbackLike, FeedbackDislike, f.Polarity)
	}
	if s == nil || s.DB == nil {
		return errors.New("feedback: no store")
	}
	source := strings.TrimSpace(f.Source)
	if source == "" {
		source = "web"
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is a no-op then

	// Idempotent per (owner, work): a double-submitted form, or a retry after
	// a timeout, must not apply the learning twice. The polarity is UPSERTed
	// rather than the row being ignored, because changing your mind is the
	// whole point of being able to press the other button.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_feedback (owner_key, work_id, polarity, source, created_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT(owner_key, work_id) DO UPDATE SET
		   polarity = excluded.polarity,
		   source = excluded.source,
		   created_at = excluded.created_at`,
		ownerKey, f.WorkID, f.Polarity, source); err != nil {
		return fmt.Errorf("feedback: record work %d: %w", f.WorkID, err)
	}

	// Read the previous polarity INSIDE the transaction. This is what makes
	// "un-doing" a feedback reversible: if the reader disliked a work and now
	// likes it, the tags must be taught the opposite way, and that is only
	// possible if the old value is read before the UPSERT overwrites it.
	var prev sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT polarity FROM work_feedback_history
		  WHERE owner_key = ? AND work_id = ? ORDER BY changed_at DESC, id DESC LIMIT 1`,
		ownerKey, f.WorkID).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_feedback_history (owner_key, work_id, polarity, source)
		VALUES (?, ?, ?, ?)`,
		ownerKey, f.WorkID, f.Polarity, source); err != nil {
		return fmt.Errorf("feedback: history work %d: %w", f.WorkID, err)
	}

	return tx.Commit()
}

// FeedbackDeltas turns a recorded feedback event into per-tag weight deltas.
//
// It is a POINTWISE update and deliberately not arena.LearnTags' pairwise one:
// a like is an absolute claim about a single work, so every tag on it is
// evidence. A comparison teaches only the tags that differ between two works,
// because a tag they share explains nothing about the choice -- there is no
// such thing as "I preferred this one because of the tag they both had".
//
// Pointwise, every tag moves by rate * polarity, so the weights drift toward
// the tags on liked works and away from the tags on disliked ones. That is the
// whole learning rule; the per-tag damping lives in UpsertTagWeight, which
// already averages by evidence count and therefore makes a tag with fifty prior
// judgements move a fiftieth as far as a tag with none.
func (s *Store) FeedbackDeltas(ctx context.Context, workID int64, polarity int) (map[int64]float64, error) {
	rate, ok := feedbackRates[polarity]
	if !ok {
		return nil, fmt.Errorf("feedback: polarity must be %d or %d, got %d",
			FeedbackLike, FeedbackDislike, polarity)
	}
	if s == nil || s.DB == nil {
		return nil, errors.New("feedback: no store")
	}
	tags, err := s.TagsForWorks(ctx, []int64{workID})
	if err != nil {
		return nil, err
	}
	tagIDs := tags[workID]
	out := make(map[int64]float64, len(tagIDs))
	if len(tagIDs) == 0 {
		return out, nil
	}
	delta := feedbackDeltaForTags(rate, polarity, len(tagIDs))
	for _, t := range tagIDs {
		out[t] = delta
	}
	return out, nil
}

// LearnFromFeedback records one like or dislike and applies the tag deltas it
// implies, in that order.
//
// The order matters and is not an implementation detail: the feedback row is
// written FIRST so that a failure in the learning step leaves a recorded
// dislike rather than a silently-dropped one, and so that the intent is
// durable even if the weights cannot be updated. The reverse order would lose
// the reader's explicit statement because of a transient write failure in the
// part they never see.
//
// The reversal case -- a reader changing their mind -- is handled by the
// caller, which asks for the previous stance first and passes the opposite
// polarity when it finds one. Teaching the new stance is not enough on its
// own: the tags of the disliked work were pushed DOWN and need pushing back
// UP, or a reader who fixes a misclick leaves the damage behind.
func (s *Store) LearnFromFeedback(ctx context.Context, ownerKey string, workID int64, polarity int) error {
	prev, err := s.FeedbackOn(ctx, ownerKey, workID)
	if err != nil {
		return err
	}
	if prev != 0 && prev != polarity {
		// Undo the old stance first: every tag on this work moves back by the
		// amount the old stance moved it.
		if err := s.applyDeltas(ctx, ownerKey, workID, -prev); err != nil {
			return err
		}
	}
	if err := s.RecordFeedback(ctx, ownerKey, FeedbackRow{WorkID: workID, Polarity: polarity}); err != nil {
		return err
	}
	return s.applyDeltas(ctx, ownerKey, workID, polarity)
}

// applyDeltas moves one work's tags by the delta its polarity implies.
func (s *Store) applyDeltas(ctx context.Context, ownerKey string, workID int64, polarity int) error {
	deltas, err := s.FeedbackDeltas(ctx, workID, polarity)
	if err != nil {
		return err
	}
	for tag, d := range deltas {
		if err := s.UpsertTagWeight(ctx, ownerKey, tag, d); err != nil {
			return fmt.Errorf("feedback: apply tag %d for work %d: %w", tag, workID, err)
		}
	}
	return nil
}

// FeedbackCount reports how many likes and dislikes a reader has recorded.
//
// Shown on the profile page next to the weights, because "420 feedback events"
// and "0 feedback events" are the difference between a profile that has
// learned something and one that is still exactly what the ratings said -- and
// an introspection page that shows the weights without the count invites the
// reader to believe the weights are better-evidenced than they are.
func (s *Store) FeedbackCount(ctx context.Context, ownerKey string) (like, dislike int, err error) {
	if s == nil || s.DB == nil {
		return 0, 0, nil
	}
	err = s.DB.QueryRowContext(ctx, `
		SELECT
		   COALESCE(SUM(CASE WHEN polarity > 0 THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN polarity < 0 THEN 1 ELSE 0 END), 0)
		FROM work_feedback WHERE owner_key = ?`, ownerKey).Scan(&like, &dislike)
	if isMissingTable(err) {
		return 0, 0, nil
	}
	return like, dislike, err
}

// FeedbackOn returns this reader's current stance on a work, for rendering a
// pressed state on the button.
func (s *Store) FeedbackOn(ctx context.Context, ownerKey string, workID int64) (int, error) {
	if s == nil || s.DB == nil {
		return 0, nil
	}
	var p int
	err := s.DB.QueryRowContext(ctx,
		`SELECT polarity FROM work_feedback WHERE owner_key = ? AND work_id = ?`,
		ownerKey, workID).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if isMissingTable(err) {
		return 0, nil
	}
	return p, err
}

// FeedbackForWorks reads stances for a list of works in one query, so a list
// of 100 results makes 2 statements rather than 100.
//
// Returns a map that is SAFE TO ITERATE WHEN EMPTY, unlike FeedbackOn's
// scalar: a page template ranging over it must not need a nil check, because
// a template that has to distinguish "no feedback" from "nil map" is a
// template that will eventually get it wrong.
func (s *Store) FeedbackForWorks(ctx context.Context, ownerKey string, workIDs []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(workIDs))
	if s == nil || s.DB == nil || len(workIDs) == 0 {
		return out, nil
	}
	const chunk = 400
	for start := 0; start < len(workIDs); start += chunk {
		end := start + chunk
		if end > len(workIDs) {
			end = len(workIDs)
		}
		batch := workIDs[start:end]
		args := make([]any, len(batch))
		placeholders := ""
		for i, id := range batch {
			args[i] = id
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
		}
		rows, err := s.DB.QueryContext(ctx,
			`SELECT work_id, polarity FROM work_feedback
			  WHERE owner_key = ? AND work_id IN (`+placeholders+`)`, append([]any{ownerKey}, args...)...)
		if err != nil {
			if isMissingTable(err) {
				return out, nil
			}
			return nil, err
		}
		for rows.Next() {
			var id int64
			var p int
			if err := rows.Scan(&id, &p); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = p
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ForgetFeedback clears a reader's likes and dislikes.
//
// Separate from ForgetSeen for the same reason the feedback table is separate:
// "stop recommending me things I liked" and "forget I read that" are different
// requests, and clearing one must not clear the other.
func (s *Store) ForgetFeedback(ctx context.Context, ownerKey string) (int64, error) {
	if s == nil || s.DB == nil {
		return 0, errors.New("feedback: no store")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is a no-op then
	res, err := tx.ExecContext(ctx, `DELETE FROM work_feedback WHERE owner_key = ?`, ownerKey)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM work_feedback_history WHERE owner_key = ?`, ownerKey); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM arena_user_tag_weights WHERE owner_key = ?`, ownerKey); err != nil {
		return 0, fmt.Errorf("feedback: clear learned weights: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// clampWeight bounds a delta so one event cannot move a tag arbitrarily far.
//
// The per-tag average in UpsertTagWeight already bounds the WEIGHT; this
// bounds the INCREMENT, because a work with 4,000 freeform tags would
// otherwise deliver 4,000 identical deltas of full size and dominate the
// profile in a single press. Dividing by sqrt(tag count) instead of the count
// keeps a tag-rich work from being able to outvote a tag-poor one by volume,
// while still letting its opinion register at all.
//
// A work with 40 tags -- which is common on this mirror, where the blurb
// scraper flattens freeforms into the tag table -- moves each tag by
// rate/6.3, so the work's total pull is still proportional to its evidence
// while no single tag can be swung to an extreme by one press.
func feedbackDeltaForTags(rate float64, polarity int, tagCount int) float64 {
	if tagCount <= 0 {
		return 0
	}
	delta := rate * float64(polarity) / math.Sqrt(float64(tagCount))
	if delta > 1 {
		return 1
	}
	if delta < -1 {
		return -1
	}
	return delta
}
