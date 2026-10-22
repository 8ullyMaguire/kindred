package api

import "net/http"

// handleBridges returns works that sit in this work's fandom and in another.
//
// A crossover is a difference, not a match: the extra fandom is a tag the seed
// does not carry, so neither tag overlap nor the item-to-item collab index
// finds it. The response carries the seed's fandom names and, per item, the
// OTHER fandom names, so a caller can see why a work is in the list.
func (s *Server) handleBridges(w http.ResponseWriter, r *http.Request) {
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
	res, err := s.Engine.Bridges(r.Context(), id, intParam(r, "limit", 5))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"work_id":      id,
		"seed_fandoms": res.SeedFandoms,
		"items":        res.Items,
		"returned":     len(res.Items),
		"note":         res.Note,
	})
}
