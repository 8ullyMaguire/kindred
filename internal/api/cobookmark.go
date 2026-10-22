package api

import (
	"net/http"
)

// handleCoBookmarked returns the works this work's readers also bookmarked.
//
// It is deliberately separate from /recommend. That endpoint answers "what
// should this reader read next", which mixes in tags, recency, quality and
// this reader's own taste. This one answers a narrower, checkable question:
// which works share actual readers with this one. A reader can click through
// and see the same names, which is the property that makes it evidence rather
// than a score.
func (s *Server) handleCoBookmarked(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(w, r, "id")
	if err != nil {
		return
	}
	if s.Engine == nil || s.Engine.Corpus == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "no corpus is loaded on this server",
		})
		return
	}
	limit := intParam(r, "limit", 5)
	items, err := s.Engine.CoBookmarked(r.Context(), id, limit)
	if err != nil {
		// The mirror may predate the interaction table. That is a fact about
		// this mirror, not a server fault, so it answers 503 with the reason
		// rather than a 500 or a 200 with an empty list that reads as "nobody
		// shares readers with this work".
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": err.Error(),
			"hint":  "this endpoint needs the mirror's user_work_interactions table",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"work_id":   id,
		"items":     items,
		"returned":  len(items),
		"requested": limit,
	})
}
