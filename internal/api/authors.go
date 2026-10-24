package api

import (
	"errors"
	"net/http"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
)

// handleSimilarAuthors ranks authors by the distinguishing tags their works
// share with the named author.
//
// The name is a query parameter rather than a path id because the mirror has
// no author table and therefore no author id to put in a path. An author is a
// byline string, and the /author page already addresses one that way. Inventing
// a hashed id for this route would give the two surfaces different names for
// the same person.
func (s *Server) handleSimilarAuthors(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest,
			errors.New("name is required, e.g. ?name=astolat"))
		return
	}
	if s.Engine == nil || s.Engine.Corpus == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "no corpus is loaded on this server",
		})
		return
	}
	items, err := s.Engine.SimilarAuthors(r.Context(), name, intParam(r, "limit", 10))
	if err != nil {
		if errors.Is(err, engine.ErrAuthorProfileEmpty) {
			// 200 with a reason: the author exists, the neighbourhood does not. A 404 here
			// would tell a reader the person is not in the mirror.
			writeJSON(w, http.StatusOK, map[string]any{
				"author":   name,
				"items":    []any{},
				"returned": 0,
				"note":     err.Error(),
			})
			return
		}
		if errors.Is(err, corpus.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"author":   name,
		"items":    items,
		"returned": len(items),
	})
}
