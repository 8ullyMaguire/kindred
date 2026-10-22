package engine

import (
	"context"
	"fmt"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/collab"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// CoBookmarkLimit bounds the "readers also bookmarked" panel. It is small on
// purpose: the panel is evidence beside a work, not a second recommendation
// list, and a reader who wants a full ranking has the recommendation list
// directly above it.
const CoBookmarkLimit = 8

// CoBookmarked returns works that readers of workID also bookmarked.
//
// This is the item-to-item half of collaborative filtering, asked as a plain
// question about one work rather than through the ranking signals. It answers
// "who else do the readers of this fic read", which is the one recommendation
// a reader can check by hand: the names in the panel are works, and the
// evidence is a count of shared readers rather than a score.
//
// The in-memory index is used when the server has one. The SQL fallback is
// what makes the method work in a CLI process, a test, or an arena server
// that chose not to build the index: it counts distinct co-bookmarkers
// directly in the mirror's interaction table. Both paths exclude the seed
// itself and both count DISTINCT users, because the mirror's interaction
// table carries duplicate rows and a COUNT(*) would report a pair as more
// strongly evidenced than it is.
func (e *Engine) CoBookmarked(ctx context.Context, workID int64, limit int) ([]rank.Candidate, error) {
	if limit <= 0 {
		limit = CoBookmarkLimit
	}
	if limit > 50 {
		limit = 50
	}
	if e.Corpus == nil {
		return nil, fmt.Errorf("co-bookmarked: this engine has no corpus")
	}

	// work id -> number of distinct shared readers.
	co := map[int64]int{}
	if e.Collab != nil {
		for _, sim := range e.Collab.Similar(workID, limit, collab.Options{}) {
			co[sim.ID] = sim.CoUsers
		}
	} else {
		rows, err := e.Corpus.DB.QueryContext(ctx, `
			SELECT b.work_id, COUNT(DISTINCT a.user_id) AS co_users
			FROM user_work_interactions a
			JOIN user_work_interactions b ON b.user_id = a.user_id
			WHERE a.work_id = ?
			  AND a.interaction_type = 'bookmarked'
			  AND b.interaction_type = 'bookmarked'
			  AND b.work_id <> ?
			GROUP BY b.work_id
			ORDER BY co_users DESC, b.work_id
			LIMIT ?`, workID, workID, limit)
		if err != nil {
			return nil, fmt.Errorf("co-bookmarked: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				return nil, fmt.Errorf("co-bookmarked: %w", err)
			}
			co[id] = n
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("co-bookmarked: %w", err)
		}
	}

	ids := make([]int64, 0, len(co))
	for id := range co {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []rank.Candidate{}, nil
	}
	ents, err := e.Corpus.CandidateRowsSlim(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("co-bookmarked candidates: %w", err)
	}
	cands, err := e.toCandidates(ctx, ents)
	if err != nil {
		return nil, fmt.Errorf("co-bookmarked tags: %w", err)
	}

	out := cands[:0]
	for _, c := range cands {
		n := co[c.ID]
		c.Evidence = []rank.Evidence{{
			Signal: "co_bookmark",
			Value:  float64(n),
			Reason: fmt.Sprintf("%d reader%s bookmarked both", n, plural(n)),
		}}
		if v, ok := c.Stats["readers_who_bookmarked_both"]; !ok || v == 0 {
			if c.Stats == nil {
				c.Stats = map[string]float64{}
			}
			c.Stats["readers_who_bookmarked_both"] = float64(n)
		}
		out = append(out, c)
	}
	// CandidateRowsSlim returns rows in whatever order SQLite chose. The
	// panel's whole claim is "most shared readers first", so the order is
	// restored here rather than left to the driver.
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := co[out[i].ID], co[out[j].ID]
		if ci != cj {
			return ci > cj
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
