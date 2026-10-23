package store

// Works a reader keeps being shown and keeps not acting on (#60).
//
// # Why this is a reading of the impression log and not a new table
//
// Every interleaved render writes one row per slot: (reader, work, arm, slot,
// credited). That is exactly the question "was this put in front of them, and did
// they do anything about it" — asking it needs no new column and no new event,
// which is why the roadmap row says the input already exists.
//
// # Why the count has to be a threshold and not 1
//
// One unclicked impression is not avoidance: the reader may not have scrolled to
// it, may have been looking for something else, or may simply have been
// interrupted. The list below only names a work after three separate shows with
// no credit, and the page says the threshold, because "you keep skipping this"
// is a claim about the reader that has to be earned.
//
// # Why the algo_impressions log is NOT read here
//
// It stores a whole list of ids as a comma string per show, so grouping by work
// would mean parsing a CSV in SQL. The interleave log is one row per work per
// show, which is the shape this question wants. Two logs, two shapes, and this
// one is the right one for the question.

import (
	"context"
)

// SkippedWork is one work this reader has been shown repeatedly without acting.
type SkippedWork struct {
	WorkID int64 `json:"id"`
	// Shows is how many separate impressions carried it, not how many slots:
	// one render of a list is one show however many times the work appeared.
	Shows int `json:"shows"`
	// LastShown is the most recent impression's timestamp, as stored.
	LastShown string `json:"last_shown"`
	// Arms counts which experiment arms showed it, for the case where the
	// skipping is one arm's fault rather than the work's.
	Arms  []string `json:"arms,omitempty"`
	Title string   `json:"title,omitempty"`
	URL   string   `json:"url,omitempty"`
}

// SkippedWorks lists works shown at least `minShows` times with no credit.
//
// Works the reader has already marked read are excluded: a work they finished is
// not a work they avoided, and a read marks the impression log's credit only when
// it happens through the credited path, so without this filter a reader who read
// a work from a bookmark could still be told they keep skipping it.
func (s *Store) SkippedWorks(ctx context.Context, ownerKey string, minShows, limit int) ([]SkippedWork, error) {
	if s == nil || s.DB == nil || ownerKey == "" {
		return nil, nil
	}
	if minShows < 1 {
		minShows = 3
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	// The NOT EXISTS reads the STATE database's seen_works, which is in the same
	// file as this table — unlike the corpus joins elsewhere, this one is legal.
	const q = `
SELECT ii.work_id,
       COUNT(DISTINCT ii.id) AS shows,
       MAX(ii.shown_at) AS last_shown,
       GROUP_CONCAT(DISTINCT ii.arm) AS arms
FROM interleave_impressions ii
WHERE ii.owner_key = ?
  AND ii.credited = 0
  AND NOT EXISTS (
    SELECT 1 FROM seen_works sw
    WHERE sw.owner_key = ii.owner_key
      AND sw.entity_id = 'ao3_work:' || ii.work_id)
GROUP BY ii.work_id
HAVING shows >= ?
ORDER BY shows DESC, last_shown DESC
LIMIT ?`
	rows, err := s.DB.QueryContext(ctx, q, ownerKey, minShows, limit)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []SkippedWork
	for rows.Next() {
		var w SkippedWork
		var arms string
		if err := rows.Scan(&w.WorkID, &w.Shows, &w.LastShown, &arms); err != nil {
			return nil, err
		}
		for _, a := range splitCSV(arms) {
			w.Arms = append(w.Arms, a)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// splitCSV splits a GROUP_CONCAT result without importing strings for one call
// site's taste: an empty string yields nothing, and blank entries are dropped.
func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
