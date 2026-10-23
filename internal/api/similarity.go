package api

import (
	"errors"
	"net/http"
	"strconv"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// handleWorkSimilarity answers "how are these two works alike" (#78).
//
// It is a PAIR endpoint, not a ranking one: /recommend answers "what next" with
// a single score per candidate, and the score hides which axis carried it. A
// reader comparing two works wants the axes: which tags they share (and how rare
// those are), and how close their tag centroids sit. This endpoint publishes
// both and the score as a renormalised mean of what ran.
//
// The components are deliberately NOT the ranking's popularity/recency terms:
// two works both published in 2019 are not more alike for it, and a pair report
// that said so would be reporting the mirror's preferences.
func (s *Server) handleWorkSimilarity(w http.ResponseWriter, r *http.Request) {
	a, err := pathInt64(w, r, "id")
	if err != nil {
		return
	}
	b, err := strconv.ParseInt(r.PathValue("other"), 10, 64)
	if err != nil || b <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the second work id must be a positive integer",
			"route": "/api/v1/works/{id}/similarity/{other}",
		})
		return
	}
	if s.Engine == nil || s.Engine.Corpus == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "no corpus is loaded on this server",
		})
		return
	}
	res, err := s.Engine.Similarity(r.Context(), a, b)
	if errors.Is(err, store.ErrNotFound) {
		// Naming WHICH id is missing matters: a pair report of 0.0 for a made-up
		// id reads as "nothing in common", which is the wrong answer to a typo.
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
