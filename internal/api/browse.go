package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"
)

// handleBrowse answers the general "fandom family x concept family"
// question and returns the evidence for it.
//
// It exists beside /api/v1/ao3/works rather than inside it because the
// works endpoint answers a different question: it returns a list of works
// with a filter applied, and it cannot say how large each pool was or
// which fandom a work crossed into. Those counts are what make an empty
// or short list explainable.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	universeKey := strings.TrimSpace(q.Get("universe"))
	conceptKey := strings.TrimSpace(q.Get("concept"))
	if universeKey == "" && conceptKey == "" {
		writeErr(w, http.StatusBadRequest,
			errors.New("universe and concept are both empty; name at least one family"))
		return
	}
	// The values are FAMILY KEYS -- "worm", "isekai" -- the same
	// vocabulary the works API, the CLI and the /browse page use. They
	// are resolved against the declared families here, so an unknown key
	// is a 400 naming the keys that exist, rather than a query that
	// silently matches nothing.
	fams, err := tagfamily.LoadFamilies()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var universeTags, conceptTags []string
	if universeKey != "" {
		u, ok := fams.Universe(universeKey)
		if !ok {
			writeErr(w, http.StatusBadRequest,
				fmt.Errorf("unknown universe %q; known: %s",
					universeKey, strings.Join(fams.UniverseNames(), ", ")))
			return
		}
		universeTags = u.Spellings
	}
	if conceptKey != "" {
		c, ok := fams.Concept(conceptKey)
		if !ok {
			writeErr(w, http.StatusBadRequest,
				fmt.Errorf("unknown concept %q; known: %s",
					conceptKey, strings.Join(fams.ConceptNames(), ", ")))
			return
		}
		conceptTags = c.Tags
	}
	recent := atoiDefault(q.Get("recent"), tagfamily.DefaultRecentDays)
	limit := clamp(atoiDefault(q.Get("n"), tagfamily.DefaultLimit), 1, 200)
	res, err := tagfamily.Query(r.Context(), s.Engine.Corpus.DB, tagfamily.QueryOptions{
		UniverseTags: universeTags,
		ConceptTags:  conceptTags,
		RecentDays:   recent,
		Limit:        limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Echo the applied filters, for the same reason handleAO3Works does: a
	// client must be able to tell "your filter matched nothing" from
	// "your filter was ignored".
	applied := map[string]string{}
	for _, k := range []string{"universe", "concept"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			applied[k] = v
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   res.Items,
		"meta":    res.Meta,
		"applied": applied,
	})
}
