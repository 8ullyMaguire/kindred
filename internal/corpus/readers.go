package corpus

// How many readers took BOTH of two works (#55).
//
// This is the number behind the "readers" explanation, so it is counted the same way the
// co-bookmark panel counts it: DISTINCT users, bookmarks only. The mirror's interaction table
// carries duplicate rows, so a COUNT(*) would report a pair as better evidenced than it is --
// and this number is a sentence a reader is asked to compare against another sentence, which
// makes an inflated count worse than a missing one.

import (
	"context"
	"fmt"
)

// SharedReaders counts the distinct readers who bookmarked both works.
//
// A work counts as read for its own bookmarker only once, and the two ids are ORDERED inside the
// query rather than trusted from the caller: the count is symmetric, and two call sites that pass
// the pair the other way round must not get different answers.
func (a *AO3) SharedReaders(ctx context.Context, x, y int64) (int, error) {
	if a == nil || a.DB == nil {
		return 0, fmt.Errorf("shared readers: no corpus")
	}
	if x == y {
		// A work's readers are not evidence about itself, and the join below would count every
		// bookmark of it.
		return 0, nil
	}
	if x > y {
		x, y = y, x
	}
	var n int
	err := a.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT a.user_id
			  FROM user_work_interactions a
			  JOIN user_work_interactions b ON b.user_id = a.user_id
			 WHERE a.work_id = ? AND b.work_id = ?
			   AND a.interaction_type = 'bookmarked'
			   AND b.interaction_type = 'bookmarked'
			 GROUP BY a.user_id)`,
		x, y).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("shared readers: %w", err)
	}
	return n, nil
}

// SharedReadersMany counts shared readers for many candidates against ONE seed, in one query.
//
// The page asks this for a whole list, and a per-candidate round trip over ~20 candidates is the
// N+1 this package avoids elsewhere. Candidates with no shared reader are absent from the map
// rather than present as zero: the caller's floor is what decides what zero means, and a map that
// pre-fills zeros hides the difference between "counted and found none" and "not looked at".
func (a *AO3) SharedReadersMany(ctx context.Context, seed int64, works []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(works))
	if a == nil || a.DB == nil {
		return out, fmt.Errorf("shared readers: no corpus")
	}
	ids := make([]int64, 0, len(works))
	for _, w := range works {
		if w != seed {
			ids = append(ids, w)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := a.DB.QueryContext(ctx, `
		SELECT b.work_id, COUNT(DISTINCT a.user_id)
		  FROM user_work_interactions a
		  JOIN user_work_interactions b ON b.user_id = a.user_id
		 WHERE a.work_id = ?
		   AND a.interaction_type = 'bookmarked'
		   AND b.interaction_type = 'bookmarked'
		   AND b.work_id IN (`+makePlaceholders(len(ids))+`)
		 GROUP BY b.work_id`, append([]any{seed}, anySlice(ids)...)...)
	if err != nil {
		return out, fmt.Errorf("shared readers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return out, fmt.Errorf("shared readers: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// TagSupport returns how many works carry each tag (#55).
//
// The explanation sentence shows the RAREST shared tags first, and rarity here is support in the
// corpus -- the same number the tree and the seasonal model read from work_tags. One query for
// the whole set: a per-tag count is the N+1 this package avoids.
func (a *AO3) TagSupport(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	if a == nil || a.DB == nil {
		return out, fmt.Errorf("tag support: no corpus")
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := a.DB.QueryContext(ctx, `
		SELECT tag_id, COUNT(*) FROM work_tags
		 WHERE tag_id IN (`+makePlaceholders(len(ids))+`)
		 GROUP BY tag_id`, anySlice(ids)...)
	if err != nil {
		return out, fmt.Errorf("tag support: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return out, fmt.Errorf("tag support: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

func anySlice(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
