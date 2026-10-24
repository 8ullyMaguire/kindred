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
	// `by` names the arm. The default orders by co-readership (measured better on a reader
	// holdout: 0.0412 vs 0.0217 recall@10 over 40 seed authors); `by=tags` reproduces the
	// pre-#35 ordering from the tag profile alone, which is also the only arm available on a
	// mirror with no interaction table.
	by := strings.TrimSpace(r.URL.Query().Get("by"))
	var (
		items []engine.SimilarAuthor
		err   error
	)
	switch by {
	case "", "readers", "co-readers":
		by = "co_readers"
		if s.Engine.CoReaders == nil {
			// No index on this mirror: the tags arm is what answers, and the response must
			// say so rather than claim an ordering nothing computed.
			by = "tag_profile"
		}
		items, err = s.Engine.SimilarAuthors(r.Context(), name, intParam(r, "limit", 10))
	case "tags", "tag_profile":
		by = "tag_profile"
		items, err = s.Engine.SimilarAuthorsByTags(r.Context(), name, intParam(r, "limit", 10))
	default:
		writeErr(w, http.StatusBadRequest,
			errors.New("by must be \"readers\" or \"tags\""))
		return
	}
	if err != nil {
		if errors.Is(err, engine.ErrAuthorProfileEmpty) {
			// 200 with a reason: the author exists, the neighbourhood does not. A 404 here
			// would tell a reader the person is not in the mirror.
			writeJSON(w, http.StatusOK, map[string]any{
				"author":    name,
				"items":     []any{},
				"returned":  0,
				"ranked_by": by,
				"note":      err.Error(),
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
		"author":    name,
		"items":     items,
		"returned":  len(items),
		"ranked_by": by,
	})
}
