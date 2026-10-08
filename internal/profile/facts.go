package profile

import (
	"context"
	"database/sql"
)

// loadCandidateFacts fetches the tags and kudos for a set of works, indexed by
// the caller's work index.
//
// The indexing is by POSITION IN THE INPUT SLICE, not by work id, because the
// caller's slices (tags[i] next to works[i]) are positional and a map lookup
// per work here would be pure overhead. The doc comment on the caller says the
// same thing, which is the only reason this contract is safe to rely on.
//
// One query per chunk rather than one per work: 257 individual tag queries
// against a 1.7 GB mirror on a network filesystem is the cost class this
// codebase has already measured and refused, and it is why work_tags is read
// in batches everywhere in this package.
func loadCandidateFacts(ctx context.Context, db *sql.DB, works []RatedWork, idx []int) (map[int][]int32, map[int]float64, error) {
	tags := make(map[int][]int32, len(idx))
	kudos := make(map[int]float64, len(idx))

	const chunk = 400
	for start := 0; start < len(idx); start += chunk {
		end := start + chunk
		if end > len(idx) {
			end = len(idx)
		}
		batch := idx[start:end]
		args := make([]any, len(batch))
		for i, wi := range batch {
			args[i] = works[wi].WorkID
		}
		placeholders := ""
		for i := range batch {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
		}

		rows, err := db.QueryContext(ctx,
			`SELECT DISTINCT work_id, tag_id FROM work_tags
			 WHERE work_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		byID := map[int64][]int32{}
		for rows.Next() {
			var wid int64
			var tag int32
			if err := rows.Scan(&wid, &tag); err != nil {
				rows.Close()
				return nil, nil, err
			}
			byID[wid] = append(byID[wid], tag)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}

		// kudos is read in the same loop so a work missing from the tags table
		// still gets a baseline value; a candidate with no tags scores 0 for
		// the profile and its real kudos for the baseline, which is the
		// comparison being asked for.
		krows, err := db.QueryContext(ctx,
			`SELECT id, COALESCE(kudos,0) FROM works WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		kudosByID := map[int64]float64{}
		for krows.Next() {
			var id int64
			var k float64
			if err := krows.Scan(&id, &k); err != nil {
				krows.Close()
				return nil, nil, err
			}
			kudosByID[id] = k
		}
		krows.Close()
		if err := krows.Err(); err != nil {
			return nil, nil, err
		}

		for i, wi := range batch {
			tags[i] = byID[works[wi].WorkID]
			kudos[i] = kudosByID[works[wi].WorkID]
		}
	}
	return tags, kudos, nil
}
