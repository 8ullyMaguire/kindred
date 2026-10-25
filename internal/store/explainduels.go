package store

// The duel between two EXPLANATIONS (#55).
//
// /algo already records which of two weight vectors ranks better. This is a different question
// about the same page: given one work, which SENTENCE explains it better -- the tags it shares
// with the seed, or the readers who took both. The answers go in their own table for the same
// reason `algo_comparisons` is not `arena_comparisons`: a preference between two sentences is not
// evidence about the tags on a work, and nothing that learns from tags may read it as such.

import (
	"context"
	"errors"
	"fmt"

	"git.polarisocial.xyz/kindred/kindred/internal/stats"
)

// ExplanationDuel is one recorded preference between two explanations of one work.
type ExplanationDuel struct {
	SeedID int64
	WorkID int64
	// Winner is "tags", "readers" or "neither". "neither" is a real answer: a reader who finds
	// both sentences unconvincing has said something the standings must count as a tie rather
	// than discard.
	Winner string
}

// Winner values.
const (
	DuelTags    = "tags"
	DuelReaders = "readers"
	DuelNeither = "neither"
)

// ExplanationStanding is one kind's record.
type ExplanationStanding struct {
	Kind   string `json:"kind"`
	Wins   int    `json:"wins"`
	Losses int    `json:"losses"`
	Ties   int    `json:"ties"`
	Judged int    `json:"judged"`
	// P is the two-sided sign-test p over DECISIVE duels, and Verdict is what the record is
	// allowed to say with it. Under the power floor the record is printed and no leader named --
	// 3-0 looks decisive and is not.
	P       float64           `json:"p"`
	Verdict stats.VoteVerdict `json:"verdict"`
}

const explanationDuelSchema = `
CREATE TABLE IF NOT EXISTS explanation_duels (
	id INTEGER PRIMARY KEY,
	owner_key TEXT NOT NULL,
	seed_id INTEGER NOT NULL,
	work_id INTEGER NOT NULL,
	winner TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE (owner_key, seed_id, work_id)
)`

// RecordExplanationDuel stores one reader's preference.
//
// ONE VOTE PER READER PER WORK, enforced by the unique index and an upsert: a reader who changes
// their mind updates their own row rather than adding a second opinion, because a standings table
// that counts clicks is a table that a single reader can move.
func (s *Store) RecordExplanationDuel(ctx context.Context, ownerKey string, d ExplanationDuel) error {
	if s == nil || s.DB == nil {
		return errors.New("explanation duel: no state database")
	}
	switch d.Winner {
	case DuelTags, DuelReaders, DuelNeither:
	default:
		return fmt.Errorf("explanation duel: winner must be tags, readers or neither, got %q", d.Winner)
	}
	if d.SeedID == 0 || d.WorkID == 0 {
		return fmt.Errorf("explanation duel: seed and work are required")
	}
	if d.SeedID == d.WorkID {
		return fmt.Errorf("explanation duel: a work cannot explain itself")
	}
	if ownerKey == "" {
		// A vote with no owner cannot be de-duplicated, so it would let one reader move the
		// standings by reloading. The page mints a session before asking precisely so this
		// cannot happen; refusing here is the second guard.
		return ErrNoIdentifier
	}
	if _, err := s.DB.ExecContext(ctx, explanationDuelSchema); err != nil {
		return fmt.Errorf("explanation duel: schema: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO explanation_duels (owner_key, seed_id, work_id, winner)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(owner_key, seed_id, work_id) DO UPDATE SET
		  winner = excluded.winner,
		  created_at = CURRENT_TIMESTAMP`,
		ownerKey, d.SeedID, d.WorkID, d.Winner); err != nil {
		return fmt.Errorf("explanation duel: %w", err)
	}
	return nil
}

// ExplanationStandings reads the record for both kinds, always both rows.
//
// Both rows are returned even with no votes at all, so a page can say "0 duels" instead of
// rendering an empty table that reads as a broken feature.
func (s *Store) ExplanationStandings(ctx context.Context) ([]ExplanationStanding, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("explanation standings: no state database")
	}
	if _, err := s.DB.ExecContext(ctx, explanationDuelSchema); err != nil {
		return nil, fmt.Errorf("explanation standings: schema: %w", err)
	}
	var wins, losses, ties int
	err := s.DB.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(winner = 'tags'), 0),
		  COALESCE(SUM(winner = 'readers'), 0),
		  COALESCE(SUM(winner = 'neither'), 0)
		FROM explanation_duels`).Scan(&wins, &losses, &ties)
	if err != nil {
		return nil, fmt.Errorf("explanation standings: %w", err)
	}
	out := []ExplanationStanding{
		{Kind: DuelTags, Wins: wins, Losses: losses, Ties: ties},
		{Kind: DuelReaders, Wins: losses, Losses: wins, Ties: ties},
	}
	for i := range out {
		row := &out[i]
		row.Judged = row.Wins + row.Losses + row.Ties
		row.P = stats.SignTest(row.Wins, row.Losses)
		row.Verdict = stats.CompareVotes(row.Kind, row.Wins, row.Losses, row.Ties)
	}
	return out, nil
}
