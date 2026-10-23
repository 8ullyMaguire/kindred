package store

// Per-kind algorithm choices, and the impressions that make an A/B possible.
//
// # Why the choice is per KIND and not per reader
//
// A reader who prefers co-bookmark CF for works is not thereby saying so for
// tags; the mechanisms are different and so is what "good" means. So the primary
// key is (owner_key, kind), and there is no global preference row that a kind
// silently inherits — inheriting is how a setting ends up applying to a surface
// nobody chose it for.
//
// # Why an impression is not a comparison
//
// `algo_comparisons` records a JUDGEMENT: a reader looked at two lists and said
// which was better. That is the strongest evidence and the rarest. An IMPRESSION
// records that a list was shown by an algorithm; a credit records that something
// on it was acted on. Live A/B needs the second because nobody judges eight lists
// a day, and a standings table fed only by judgements is empty for weeks.
//
// The two are kept apart on purpose: mixing them into one "score" would let a
// handful of clicks outvote a person's considered comparison without ever saying
// so.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// AlgoChoice is one reader's stored algorithm for one kind.
type AlgoChoice struct {
	OwnerKey string
	Kind     arena.EntityKind
	Algo     string
	ChosenAt time.Time
}

// SetAlgoChoice stores (or replaces) a reader's algorithm for a kind.
//
// The kind and the algorithm are validated by the caller, which owns the
// registry; this function refuses only what would corrupt the table (an empty
// owner or kind, an empty algorithm) and returns a sentence rather than a
// constraint violation.
func (s *Store) SetAlgoChoice(ctx context.Context, ownerKey string, kind arena.EntityKind, algoID string) error {
	if ownerKey == "" {
		return errors.New("an algorithm choice needs a reader: an assignment without an owner cannot be honoured on the next request")
	}
	if !kind.Valid() {
		return fmt.Errorf("unknown kind %q", kind)
	}
	if algoID == "" {
		return errors.New("an algorithm choice needs an algorithm")
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO algo_choices (owner_key, kind, algo, chosen_at) VALUES (?,?,?,?)
		 ON CONFLICT(owner_key, kind) DO UPDATE SET algo = excluded.algo, chosen_at = excluded.chosen_at`,
		ownerKey, string(kind), algoID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// ClearAlgoChoice removes a reader's choice for a kind, returning them to their
// A/B arm. It is a separate verb from Set because "follow the experiment again"
// and "use this algorithm" are different intentions.
func (s *Store) ClearAlgoChoice(ctx context.Context, ownerKey string, kind arena.EntityKind) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM algo_choices WHERE owner_key = ? AND kind = ?`, ownerKey, string(kind))
	return err
}

// AlgoChoices returns every stored choice for a reader, by kind.
func (s *Store) AlgoChoices(ctx context.Context, ownerKey string) (map[arena.EntityKind]string, error) {
	out := map[arena.EntityKind]string{}
	if ownerKey == "" {
		return out, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT kind, algo FROM algo_choices WHERE owner_key = ?`, ownerKey)
	if err != nil {
		if isMissingTable(err) {
			return out, nil
		}
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return out, err
		}
		out[arena.EntityKind(kind)] = id
	}
	return out, rows.Err()
}

// RecordAlgoImpression logs that a list was shown by an algorithm for a kind.
//
// `arm` is "default", "challenger" or "choice": it says WHY this algorithm was
// used, which is the difference between an experiment's two arms and a reader
// who opted out. Without it, a standings table cannot tell a challenger winning
// on its merits from one winning because the readers most likely to click chose
// it.
func (s *Store) RecordAlgoImpression(ctx context.Context, ownerKey string, kind arena.EntityKind, algoID, arm string, seedID int64) error {
	if s == nil || s.DB == nil {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO algo_impressions (owner_key, kind, algo, arm, seed_id, shown_at)
		 VALUES (?,?,?,?,?,?)`,
		ownerKey, string(kind), algoID, arm, seedID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// CreditAlgoImpression marks the most recent impression of this seed for this
// reader as acted on, and reports how many rows it credited (0 or 1).
//
// The WHERE clause pins the row by (owner, kind, seed) and by `credited = 0`:
// a second click on the same list must not count twice, and a click after a
// reader has re-requested the list must credit the show that produced the list
// they clicked, which is the one still open for crediting.
func (s *Store) CreditAlgoImpression(ctx context.Context, ownerKey string, kind arena.EntityKind, seedID int64) (int64, error) {
	if s == nil || s.DB == nil || ownerKey == "" {
		return 0, nil
	}
	res, err := s.DB.ExecContext(ctx,
		`UPDATE algo_impressions SET credited = 1
		 WHERE id = (SELECT id FROM algo_impressions
		             WHERE owner_key = ? AND kind = ? AND seed_id = ? AND credited = 0
		             ORDER BY id DESC LIMIT 1)`,
		ownerKey, string(kind), seedID)
	if err != nil {
		if isMissingTable(err) {
			return 0, nil
		}
		return 0, err
	}
	return res.RowsAffected()
}

// AlgoArmStat is one (kind, algorithm, arm) cell of the live standings.
type AlgoArmStat struct {
	Kind        arena.EntityKind `json:"kind"`
	Algo        string           `json:"algo"`
	Arm         string           `json:"arm"`
	Impressions int64            `json:"impressions"`
	Credits     int64            `json:"credits"`
	// CreditRate is credits/impressions. Zero impressions means 0, not NaN:
	// the wire format has no NaN and a reader should see "no data" rather
	// than the string "NaN".
	CreditRate float64 `json:"credit_rate"`
}

// AlgoStandingsByKind aggregates impressions per kind, algorithm and arm.
//
// Rows with zero impressions are NOT invented for algorithms nobody served: a
// standings table that lists algorithms at 0/0 invites the reading that they
// were tried and lost.
func (s *Store) AlgoStandingsByKind(ctx context.Context) ([]AlgoArmStat, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT kind, algo, arm,
		        COUNT(*) AS impressions,
		        SUM(CASE WHEN credited = 1 THEN 1 ELSE 0 END) AS credits
		 FROM algo_impressions
		 GROUP BY kind, algo, arm
		 ORDER BY kind, impressions DESC`)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []AlgoArmStat
	for rows.Next() {
		var st AlgoArmStat
		var kind, algoID, arm string
		if err := rows.Scan(&kind, &algoID, &arm, &st.Impressions, &st.Credits); err != nil {
			return nil, err
		}
		st.Kind = arena.EntityKind(kind)
		st.Algo = algoID
		st.Arm = arm
		if st.Impressions > 0 {
			st.CreditRate = float64(st.Credits) / float64(st.Impressions)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// AlgoArmCounts reports how many readers each arm has been SERVED to, which is
// the denominator an A/B needs and an impression count is not: one busy reader
// refreshing a page is many impressions and one reader.
func (s *Store) AlgoArmCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT kind || '/' || arm, COUNT(DISTINCT owner_key) FROM algo_impressions GROUP BY 1`)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// isMissingTable is shared with the rest of the store; declared there if it
// already exists, so this file does not redeclare it.
var _ = sql.ErrNoRows
