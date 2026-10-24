package corpus

// Word counts for a batch of works, for the length-fit measurement.
//
// The evaluation compares a returned list's median length with the median of the
// works a reader went on to bookmark, and it holds ids, not entities. Reading the
// counts in one batched query keeps that measurement's cost proportional to the
// run rather than to the corpus: a per-id lookup over a 634k-work mirror would be
// the latency bug this project already fixed once.
//
// A work with no row, or with a zero count, is simply absent from the map. That is
// the honest shape: "this mirror does not know how long this work is" is not the
// same claim as "this work is zero words long", and the caller (eval.MeasureLengthFit)
// treats absence as unmeasurable rather than as a length.

import (
	"context"
	"fmt"
)

// WordCounts returns id -> words for the ids the mirror knows a length for.
//
// The signature is eval.WordCountSource's, so the CLI's recommender adapter can
// forward to this without a translation layer (and without eval importing the
// corpus).
func (a *AO3) WordCounts(ctx context.Context, ids []int64) (map[int64]int, error) {
	if len(ids) == 0 {
		return map[int64]int{}, nil
	}
	// SQLite's variable limit is 999 by default in older builds and 32766 in
	// newer ones; 400 keeps this query well inside either, and the chunking is the
	// same shape readamong.go uses for the same reason.
	const chunk = 400
	out := make(map[int64]int, len(ids))
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := a.DB.QueryContext(ctx,
			`SELECT id, word_count FROM works
			 WHERE id IN (`+makePlaceholders(len(args))+`)
			   AND word_count IS NOT NULL AND word_count > 0`, args...)
		if err != nil {
			return nil, fmt.Errorf("word counts: %w", err)
		}
		for rows.Next() {
			var id int64
			var words int
			if err := rows.Scan(&id, &words); err != nil {
				rows.Close()
				return nil, fmt.Errorf("word counts: %w", err)
			}
			out[id] = words
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("word counts: %w", err)
		}
		rows.Close()
	}
	return out, nil
}
