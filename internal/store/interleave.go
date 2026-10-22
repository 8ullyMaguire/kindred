package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Interleaved impressions and their attribution.
//
// An interleaving experiment works only if the impression log and the credit
// log are the same log. Recording "arm A contributed this work" in one place
// and "this work was clicked" in another is how an experiment ends up unable
// to say which arm earned the click, and the usual repair -- assuming the
// first arm -- makes the winner a function of the merge order.
//
// So every shown item is a row, with the arm that contributed it, and a credit
// is a flag on that row. A work that two arms both ranked is one row, credited
// to the arm whose turn reached it.
//
// One credit per impression: the UPDATE is `WHERE id = ? AND credited = 0`, so
// a reader who marks the same work seen twice credits it once. Counting the
// second mark would weight whichever arm the reader happened to revisit.

// InterleaveSlot is one item of a presented merged list, as the store needs it.
type InterleaveSlot struct {
	WorkID int64
	// Side is which arm contributed it: "a" or "b". The arm LABELS live on
	// every row too, so a standings query can group by name without a join.
	Side string
	Slot int
}

// InterleaveArmStat is one arm's totals across every pair it appeared in.
type InterleaveArmStat struct {
	Arm         string `json:"arm"`
	Impressions int    `json:"impressions"`
	Credits     int    `json:"credits"`
	// CreditRate is credits/impressions. It is a rate over SLOTS, not over
	// readers: it says how often a slot this arm won was acted on.
	CreditRate float64 `json:"credit_rate"`
}

// InterleavePairStat is one head-to-head: how many slots each side got and how
// many of those slots were acted on.
type InterleavePairStat struct {
	ArmA         string `json:"arm_a"`
	ArmB         string `json:"arm_b"`
	ImpressionsA int    `json:"impressions_a"`
	ImpressionsB int    `json:"impressions_b"`
	CreditsA     int    `json:"credits_a"`
	CreditsB     int    `json:"credits_b"`
	// Difference is (credits_a/impressions_a) - (credits_b/impressions_b).
	// Reported rather than only the two rates because the question the
	// experiment asks is comparative, and two rates a reader has to subtract
	// in their head is a table, not an answer.
	Difference float64 `json:"difference"`
}

// RecordInterleave logs one presented merged list for an owner.
func (s *Store) RecordInterleave(ctx context.Context, ownerKey, seedKind string, seedID int64, armA, armB string, slots []InterleaveSlot) error {
	if len(slots) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO interleave_impressions
			(owner_key, seed_kind, seed_id, arm_a, arm_b, work_id, arm, slot)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, sl := range slots {
		if sl.Side != "a" && sl.Side != "b" {
			return fmt.Errorf("interleave: side %q is neither a nor b", sl.Side)
		}
		if _, err := stmt.ExecContext(ctx, ownerKey, seedKind, seedID, armA, armB, sl.WorkID, sl.Side, sl.Slot); err != nil {
			return err
		}
	}
	// Retention, in the same transaction so a failed prune cannot leave a
	// rendered list unlogged. See idx_interleave_shown.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM interleave_impressions
		 WHERE shown_at < datetime('now', '-30 days')`); err != nil {
		return err
	}
	return tx.Commit()
}

// CreditInterleave marks the newest uncredited impression of workID as acted
// on, and returns the arm that showed it.
//
// "Newest" because a work can be shown again by a later experiment, and the
// reader is acting on the sighting they just had. ok is false when this owner
// has no uncredited impression of the work -- a seen mark on a work found by
// searching is not evidence about either arm, and it must not be counted as
// one.
func (s *Store) CreditInterleave(ctx context.Context, ownerKey string, workID int64) (arm, armA, armB string, ok bool, err error) {
	var id int64
	var side string
	err = s.DB.QueryRowContext(ctx, `
		SELECT id, arm, arm_a, arm_b
		  FROM interleave_impressions
		 WHERE owner_key = ? AND work_id = ? AND credited = 0
		 ORDER BY id DESC
		 LIMIT 1`, ownerKey, workID).Scan(&id, &side, &armA, &armB)
	if err == sql.ErrNoRows {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE interleave_impressions SET credited = 1
		 WHERE id = ? AND credited = 0`, id)
	if err != nil {
		return "", "", "", false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Another request credited the same impression between the read
		// and the update. The credit belongs to whoever got there first;
		// reporting it here would count one impression twice.
		return "", "", "", false, nil
	}
	if side == "a" {
		return armA, armA, armB, true, nil
	}
	return armB, armA, armB, true, nil
}

// InterleaveStats returns per-arm totals and per-pair head-to-heads.
//
// Uncredited impressions are included in the denominator on purpose: a slot
// nobody acted on is evidence, and dropping it would report the click rate
// among clicks.
func (s *Store) InterleaveStats(ctx context.Context) ([]InterleaveArmStat, []InterleavePairStat, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT arm_a, arm_b, arm,
		       COUNT(*) AS impressions,
		       COALESCE(SUM(credited), 0) AS credits
		  FROM interleave_impressions
		 GROUP BY arm_a, arm_b, arm
		 ORDER BY arm_a, arm_b, arm`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type key struct{ a, b string }
	perArm := map[string]*InterleaveArmStat{}
	var arms []string
	pairs := map[key]*InterleavePairStat{}
	var pairOrder []key

	for rows.Next() {
		var a, b, side string
		var impressions, credits int
		if err := rows.Scan(&a, &b, &side, &impressions, &credits); err != nil {
			return nil, nil, err
		}
		// The arm that contributed is a side; its LABEL is on the row.
		label := a
		if side == "b" {
			label = b
		}
		st, ok := perArm[label]
		if !ok {
			st = &InterleaveArmStat{Arm: label}
			perArm[label] = st
			arms = append(arms, label)
		}
		st.Impressions += impressions
		st.Credits += credits

		k := key{a, b}
		p, ok := pairs[k]
		if !ok {
			p = &InterleavePairStat{ArmA: a, ArmB: b}
			pairs[k] = p
			pairOrder = append(pairOrder, k)
		}
		if side == "a" {
			p.ImpressionsA += impressions
			p.CreditsA += credits
		} else {
			p.ImpressionsB += impressions
			p.CreditsB += credits
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	out := make([]InterleaveArmStat, 0, len(arms))
	for _, name := range arms {
		st := perArm[name]
		st.CreditRate = rate(st.Credits, st.Impressions)
		out = append(out, *st)
	}
	pairOut := make([]InterleavePairStat, 0, len(pairOrder))
	for _, k := range pairOrder {
		p := pairs[k]
		p.Difference = rate(p.CreditsA, p.ImpressionsA) - rate(p.CreditsB, p.ImpressionsB)
		pairOut = append(pairOut, *p)
	}
	return out, pairOut, nil
}

func rate(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}
