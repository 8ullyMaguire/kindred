package store

// Which of a set of works this reader has already read (#50, "complete your
// collection").
//
// The state database knows what the reader read and the corpus knows what an
// author wrote, and the two are different SQLite files. So the answer is
// computed by reading the READ SET for a bounded list of ids rather than by
// joining: `seen_works` in one file, `works` in the other, no ATTACH.
//
// # Why this takes ids instead of a byline
//
// A byline query is corpus business and belongs at the caller (which already has
// the corpus handle and the LIKE escaping). Handing an owner key and a string to
// the state store would make this method a second, subtly different author
// search — the failure mode this tree has hit before with tag_type='fandoms'.

import (
	"context"
	"strings"
)

// ReadAmong returns the subset of ids this reader has marked read.
//
// A work that was only SHOWN is not read: `seen_works` records both, and the
// difference is the whole point of a "to go" count. `read_at` is the column that
// separates them.
func (s *Store) ReadAmong(ctx context.Context, ownerKey string, ids []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(ids))
	if s == nil || s.DB == nil || ownerKey == "" || len(ids) == 0 {
		return out, nil
	}
	// 400 ids per statement: the marker limit the rest of this tree respects.
	for start := 0; start < len(ids); start += 400 {
		end := start + 400
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		marks := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, ownerKey)
		for _, id := range chunk {
			marks = append(marks, "?")
			args = append(args, WorkEntityID(id))
		}
		q := `SELECT entity_id FROM seen_works
		      WHERE owner_key = ? AND read_at IS NOT NULL
		        AND entity_id IN (` + strings.Join(marks, ",") + `)`
		rows, err := s.DB.QueryContext(ctx, q, args...)
		if err != nil {
			if isMissingTable(err) {
				return out, nil
			}
			return nil, err
		}
		for rows.Next() {
			var ent string
			if err := rows.Scan(&ent); err != nil {
				rows.Close()
				return nil, err
			}
			if id, ok := EntityNumericID(ent); ok {
				out[id] = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
