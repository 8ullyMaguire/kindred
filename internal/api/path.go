package api

import (
	"errors"
	"net/http"
	"strconv"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// handlePath serves a reading path (#40).
//
// A path is not a ranking: each step is chosen against the PREVIOUS work, so the
// list drifts. That is why `n` here is a number of STEPS and not a result count,
// and why every step publishes its similarity to the step before rather than to
// the seed.
func (s *Server) handlePath(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(w, r, "id")
	if err != nil {
		return
	}
	steps := 10
	if v := r.URL.Query().Get("steps"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "steps must be a positive integer",
			})
			return
		}
		steps = n
	}
	if s.Engine == nil || s.Engine.Corpus == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "no corpus is loaded on this server",
		})
		return
	}
	res, err := s.Engine.ReadingPath(r.Context(), id, steps)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
