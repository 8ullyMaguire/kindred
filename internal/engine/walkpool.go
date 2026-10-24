package engine

// The walk's pool source.
//
// The signal alone cannot do the job it was built for. Its candidates come from the
// tag pool, and the tag pool is "works sharing a seed tag" — so a work the walk
// reached through a TWO-hop tag, sharing nothing with the seed, never enters the
// ranking at all. Measured on the live mirror 2026-10-24: at weight 0.06 the walk
// changed no list, with and without it, on every seed tried. The walk was correct
// and inert, for the same reason collab was inert before its own pool source existed
// (see the comment in poolFor): a signal whose candidates all come from one hop
// cannot surface a two-hop find.
//
// So this file admits them: take the tags the walk landed on, hardest first, ask the
// corpus which works carry them, and let the pool's union decide. Nothing here
// decides a score — the signal does that, over the same mass.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
)

// walkPoolTags is how many of the walk's tags are used to find works.
//
// The mass is a long tail: its head is what the walk actually believes, and the tail
// is a thousand tags holding 1e-5 each. 24 is the graph's own per-tag neighbour cap,
// so the source asks for at most about as many tags as one seed's neighbourhood
// would offer, which keeps the SQL's IN list bounded.
const walkPoolTags = 24

// walkCandidates returns up to limit works carrying the tags the walk landed on,
// excluding the seeds and honouring the request's filters, blocks and seen set.
//
// The filters are applied in SQL, not after: the pool is the CANDIDATE SET, and
// filtering a chosen set returns fewer than asked for even when eligible works sit
// further down the ranking.
func walkCandidates(ctx context.Context, db *sql.DB, mass map[int32]float64, limit int, seedIDs []int64, f Filter, blocked map[int64]bool, seen map[string]bool) ([]int64, error) {
	if limit <= 0 || len(mass) == 0 || db == nil {
		return nil, nil
	}
	// Heaviest tags first, id as tiebreak so two identical requests produce the
	// identical pool. Go randomises map iteration, and a pool that reshuffles is a
	// ranking that reshuffles.
	type tagMass struct {
		id   int32
		mass float64
	}
	tags := make([]tagMass, 0, len(mass))
	for id, m := range mass {
		tags = append(tags, tagMass{id, m})
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].mass != tags[j].mass {
			return tags[i].mass > tags[j].mass
		}
		return tags[i].id < tags[j].id
	})
	if len(tags) > walkPoolTags {
		tags = tags[:walkPoolTags]
	}

	args := make([]any, 0, len(tags)+len(seedIDs)+8)
	ph := corpus.Placeholders(len(tags))
	for _, t := range tags {
		args = append(args, t.id)
	}
	q := `SELECT wt.work_id, COUNT(*) AS held FROM work_tags wt
	      JOIN works w ON w.id = wt.work_id
	      WHERE wt.tag_id IN (` + ph + `)`
	q += filterSQL(f, &args)
	q += blockSQL(blocked, &args)
	q += seenSQL(seen, &args)
	if len(seedIDs) > 0 {
		sp := corpus.Placeholders(len(seedIDs))
		q += ` AND wt.work_id NOT IN (` + sp + `)`
		for _, id := range seedIDs {
			args = append(args, id)
		}
	}
	q += ` GROUP BY wt.work_id
	       ORDER BY held DESC, wt.work_id ASC
	       LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("walk pool query: %w", err)
	}
	defer rows.Close()
	out := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		var held int
		if err := rows.Scan(&id, &held); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
