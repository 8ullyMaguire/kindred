package api

// Taste export and import (#36).
//
// The reader's learned tag weights are the only model of THEM this deployment has, and until
// now they could not leave it: no route read them out and none wrote them back. That made a
// taste non-portable and, more importantly, un-explainable -- a reader could see that their
// ranking changed and not what it had learned.
//
//	GET /api/v1/taste            the caller's vector, with evidence counts and tag names
//	PUT /api/v1/taste            import: MERGE by default, ?replace=1 to replace
//
// The owner is the arena session cookie, hashed exactly as the arena hashes it. There is no
// way to address another reader's vector, and adding one would be a different feature with a
// different argument.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// tasteMaxTags bounds an import. The arena learns over dozens of tags; a file with ten
// thousand is a scraper's output, and the signal reads at most 60 on the request path.
const tasteMaxTags = 200

// tasteExportTag is one tag in the exported vector.
type tasteExportTag struct {
	ID     int64   `json:"id"`
	Name   string  `json:"name,omitempty"`
	Weight float64 `json:"weight"`
	N      int     `json:"n"`
}

// tasteImportTag is one tag in an imported vector. A name is preferred and an id is accepted:
// a name survives a re-ingest, an id does not, but a caller exporting from a DIFFERENT mirror
// has ids and no names.
type tasteImportTag struct {
	ID     int64    `json:"id,omitempty"`
	Name   *string  `json:"name,omitempty"`
	Weight *float64 `json:"weight"`
	N      *int     `json:"n,omitempty"`
}

func (s *Server) handleGetTaste(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no state database"})
		return
	}
	ownerKey, err := s.Store.OwnerKey(r.Context(), s.sessionKeyFor(r))
	if err != nil || ownerKey == "" {
		writeErr(w, http.StatusUnauthorized,
			errors.New("a session is required: taste is learned from your own comparisons"))
		return
	}
	weights, err := s.Store.TagWeights(r.Context(), ownerKey, tasteMaxTags)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	tags := make([]tasteExportTag, 0, len(weights))
	ids := make([]int64, 0, len(weights))
	for _, tw := range weights {
		tags = append(tags, tasteExportTag{ID: tw.TagID, Weight: tw.Weight, N: tw.N})
		ids = append(ids, tw.TagID)
	}
	// Names, because "tag 41" is not something a reader can argue with. A missing corpus is not
	// fatal: the vector is still true without its labels.
	if s.Engine != nil && s.Engine.Corpus != nil && len(ids) > 0 {
		if names, nerr := s.Engine.Corpus.TagNames(r.Context(), ids); nerr == nil {
			for i := range tags {
				tags[i].Name = names[tags[i].ID]
			}
		}
	}
	sort.SliceStable(tags, func(i, j int) bool {
		ai, aj := tags[i].Weight, tags[j].Weight
		if ai < 0 {
			ai = -ai
		}
		if aj < 0 {
			aj = -aj
		}
		if ai != aj {
			return ai > aj
		}
		return tags[i].ID < tags[j].ID
	})
	body := map[string]any{
		"tags":  tags,
		"count": len(tags),
	}
	if len(tags) == 0 {
		body["note"] = "no taste is learned yet: it comes from arena comparisons, and this session has made none"
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePutTaste(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no state database"})
		return
	}
	ownerKey, err := s.Store.OwnerKey(r.Context(), s.sessionKeyFor(r))
	if err != nil || ownerKey == "" {
		writeErr(w, http.StatusUnauthorized,
			errors.New("a session is required: an import writes to the session's own vector"))
		return
	}

	var in struct {
		Tags []tasteImportTag `json:"tags"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	// Unknown fields are refused: a caller sending `weights` instead of `tags` would otherwise
	// import nothing and be told it succeeded.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("body must be {\"tags\":[{\"name\":..,\"weight\":..,\"n\":..}]}: %w", err))
		return
	}
	if len(in.Tags) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("tags is empty: an empty import would either do nothing or delete everything"))
		return
	}
	if len(in.Tags) > tasteMaxTags {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("at most %d tags per import, got %d", tasteMaxTags, len(in.Tags)))
		return
	}

	// Resolve names to ids FIRST, and refuse the whole import if any name is unknown. A partial
	// import that silently drops the tags this mirror has never seen is the failure mode that
	// makes a portable file useless.
	var names []string
	for _, t := range in.Tags {
		if t.Name != nil && strings.TrimSpace(*t.Name) != "" {
			names = append(names, strings.TrimSpace(*t.Name))
		}
	}
	byName := map[string]int64{}
	if len(names) > 0 {
		if s.Engine == nil || s.Engine.Corpus == nil {
			writeErr(w, http.StatusServiceUnavailable,
				errors.New("no corpus is loaded, so a tag NAME cannot be resolved; send ids"))
			return
		}
		m, merr := s.Engine.Corpus.TagIDsByName(r.Context(), names)
		if merr != nil {
			writeErr(w, http.StatusServiceUnavailable, merr)
			return
		}
		byName = m
	}

	// Validate everything before writing anything: a half-applied import is worse than a
	// refused one.
	type resolved struct {
		id     int64
		weight float64
		n      int
	}
	out := make([]resolved, 0, len(in.Tags))
	seen := map[int64]struct{}{}
	for i, t := range in.Tags {
		var id int64
		switch {
		case t.Name != nil && strings.TrimSpace(*t.Name) != "":
			key := strings.ToLower(strings.TrimSpace(*t.Name))
			got, ok := byName[key]
			if !ok {
				writeErr(w, http.StatusUnprocessableEntity,
					fmt.Errorf("tags[%d]: this mirror has no tag named %q", i, strings.TrimSpace(*t.Name)))
				return
			}
			id = got
		case t.ID > 0:
			id = t.ID
		default:
			writeErr(w, http.StatusUnprocessableEntity,
				fmt.Errorf("tags[%d]: needs a name or an id", i))
			return
		}
		if _, dup := seen[id]; dup {
			writeErr(w, http.StatusUnprocessableEntity,
				fmt.Errorf("tags[%d]: tag %d appears twice in one import", i, id))
			return
		}
		seen[id] = struct{}{}
		if t.Weight == nil {
			writeErr(w, http.StatusUnprocessableEntity,
				fmt.Errorf("tags[%d]: weight is required (0 is a legitimate weight)", i))
			return
		}
		if *t.Weight < -1 || *t.Weight > 1 {
			writeErr(w, http.StatusUnprocessableEntity,
				fmt.Errorf("tags[%d]: weight %v is outside [-1, 1]", i, *t.Weight))
			return
		}
		n := 1
		if t.N != nil {
			n = *t.N
		}
		if n < 1 || n > 1000 {
			writeErr(w, http.StatusUnprocessableEntity,
				fmt.Errorf("tags[%d]: n %d is outside [1, 1000]", i, n))
			return
		}
		out = append(out, resolved{id: id, weight: *t.Weight, n: n})
	}

	before, err := s.Store.CountTagWeights(r.Context(), ownerKey)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	replace := strings.TrimSpace(r.URL.Query().Get("replace"))
	replaced := 0
	if replace == "1" || strings.EqualFold(replace, "true") {
		replaced, err = s.Store.ClearTagWeights(r.Context(), ownerKey)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err)
			return
		}
	}
	for _, t := range out {
		if err := s.Store.SetTagWeight(r.Context(), ownerKey, t.id, t.weight, t.n); err != nil {
			writeErr(w, http.StatusServiceUnavailable, err)
			return
		}
	}
	after, err := s.Store.CountTagWeights(r.Context(), ownerKey)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	body := map[string]any{
		"imported": len(out),
		"before":   before,
		"after":    after,
	}
	if replaced > 0 {
		body["replaced"] = replaced
	} else {
		body["note"] = "merged into the existing vector; ?replace=1 replaces it instead"
	}
	writeJSON(w, http.StatusOK, body)
}
