package store

import (
	"context"
	"fmt"
)

// AlgoComparison is one A/B judgement: two named tunes, one seed, one
// preference.
type AlgoComparison struct {
	SeedKind string
	SeedID   int64
	AlgoA    string
	AlgoB    string
	// Winner is "a", "b" or "neither". A draw is stored: dropping draws
	// would inflate every algorithm's win rate as the reader grows more
	// indifferent, which is the opposite of what a standings table is for.
	Winner string
}

// AlgoStanding is one algorithm's record: the judgements it took part in as
// one side of the pair, and how many it won.
//
// The JSON tags are snake_case because this struct IS the wire shape of
// /api/v1/algo/standings. Without them the fields serialise as "Algo" and
// "Wins" while every other response in this API is snake_case, which reads
// as a different API bolted on rather than the same one.
type AlgoStanding struct {
	Algo   string `json:"algo"`
	Wins   int    `json:"wins"`
	Losses int    `json:"losses"`
	Ties   int    `json:"ties"`
	// Judged is Wins + Losses + Ties — the number of judgements this
	// algorithm appeared in. It is stored rather than recomputed by the
	// caller so a standings table cannot disagree with itself.
	Judged int `json:"judged"`
}

// RecordAlgoComparison stores one A/B judgement.
//
// The validation is here rather than only in the handler because this is the
// function that can corrupt the table, and a handler is one caller among
// several (a future CLI, a replay tool). The three properties enforced are
// the ones a bad row would silently poison:
//
//   - winner is one of a, b, neither. The table's CHECK says the same; this
//     returns a sentence rather than a constraint violation.
//   - the two algorithms are different. Comparing a tune with itself
//     produces a judgement whose answer is meaningless, and it would count
//     as both a win and a loss in the standings.
//   - the seed names a kind. A row with an empty kind cannot be broken down
//     later and cannot be told apart from a malformed one.
func (s *Store) RecordAlgoComparison(ctx context.Context, ownerKey string, c AlgoComparison) error {
	switch c.Winner {
	case "a", "b", "neither":
	default:
		return fmt.Errorf("winner must be a, b or neither, got %q", c.Winner)
	}
	if c.AlgoA == "" || c.AlgoB == "" {
		return fmt.Errorf("both algorithms must be named")
	}
	if c.AlgoA == c.AlgoB {
		return fmt.Errorf("cannot compare %q with itself: the answer would be meaningless "+
			"and would count as both a win and a loss", c.AlgoA)
	}
	if c.SeedKind == "" {
		return fmt.Errorf("seed kind must be named")
	}
	if s == nil || s.DB == nil {
		return fmt.Errorf("no store")
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO algo_comparisons
			(owner_key, seed_kind, seed_id, algo_a, algo_b, winner)
		VALUES (?, ?, ?, ?, ?, ?)`,
		ownerKey, c.SeedKind, c.SeedID, c.AlgoA, c.AlgoB, c.Winner)
	if err != nil {
		return fmt.Errorf("record algo comparison: %w", err)
	}
	return nil
}

// AlgoStandings aggregates judgements into per-algorithm win/loss/tie counts.
//
// It is scoped to ONE reader (ownerKey) because two readers' aesthetic
// preferences are not each other's evidence: pooling them would produce a
// standings table about nobody. An empty ownerKey means every reader, which
// is what a single-user mirror wants and what the CLI uses.
//
// A judgement counts for BOTH algorithms it names — the winner gets a win
// from whichever side it was on, the loser a loss — so the counts are not a
// partition of the judgement count but a per-algorithm view of it.
func (s *Store) AlgoStandings(ctx context.Context, ownerKey string) ([]AlgoStanding, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("no store")
	}
	// One pass over the judgements, tallying into a map, rather than two
	// queries (one for the a side, one for the b side) whose results would
	// have to be merged anyway. The table is small and there is no
	// windowing yet.
	rows, err := s.DB.QueryContext(ctx, `
		SELECT algo_a, algo_b, winner FROM algo_comparisons
		WHERE (? = '' OR owner_key = ?)`, ownerKey, ownerKey)
	if err != nil {
		return nil, fmt.Errorf("algo standings: %w", err)
	}
	defer rows.Close()

	byAlgo := map[string]*AlgoStanding{}
	get := func(name string) *AlgoStanding {
		st, ok := byAlgo[name]
		if !ok {
			st = &AlgoStanding{Algo: name}
			byAlgo[name] = st
		}
		return st
	}

	for rows.Next() {
		var a, b, winner string
		if err := rows.Scan(&a, &b, &winner); err != nil {
			return nil, err
		}
		sa, sb := get(a), get(b)
		sa.Judged++
		sb.Judged++
		switch winner {
		case "a":
			sa.Wins++
			sb.Losses++
		case "b":
			sb.Wins++
			sa.Losses++
		case "neither":
			sa.Ties++
			sb.Ties++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]AlgoStanding, 0, len(byAlgo))
	for _, st := range byAlgo {
		out = append(out, *st)
	}
	// Most wins first, then the most-judged, then by name — a stable order
	// so a standings page does not shuffle between identical reloads.
	sortStandings(out)
	return out, nil
}

// sortStandings orders by wins desc, judged desc, then name asc.
//
// Name is the final key on purpose: without a total order the page would
// reorder itself between two identical requests, which reads as the numbers
// changing.
func sortStandings(st []AlgoStanding) {
	for i := 1; i < len(st); i++ {
		for j := i; j > 0; j-- {
			a, b := st[j-1], st[j]
			if a.Wins != b.Wins {
				if a.Wins > b.Wins {
					break
				}
			} else if a.Judged != b.Judged {
				if a.Judged > b.Judged {
					break
				}
			} else if a.Algo <= b.Algo {
				break
			}
			st[j-1], st[j] = st[j], st[j-1]
		}
	}
}
