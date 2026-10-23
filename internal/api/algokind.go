package api

// The per-kind algorithm surface: the registry, a reader's choice, and the
// related list each algorithm produces for a seed.
//
// # Three routes, three questions
//
//	GET  /api/v1/algo/kinds    what can run, per kind, and how it is doing
//	POST /api/v1/algo/choice   choose (or clear) an algorithm for one kind
//	GET  /api/v1/related       the list this algorithm produces for a seed
//
// These sit beside the EXISTING /api/v1/algo/{list,compare,judge,standings},
// which compare named TUNES for works. A tune is one mechanism's knobs; this
// surface chooses the mechanism, per kind. Both are "algorithms" in ordinary
// speech and they are deliberately two routes, because their answers are
// measured differently: tunes are judged in pairs, mechanisms are counted by
// impressions and credits.
//
// # Why the impression is recorded here and not only on the page
//
// The page and this handler serve the same list. The log is written by the thing
// that SERVES it — this handler for API callers, `web` for the page — in the same
// row shape. What must not happen is a page that logs one impression by fetching
// the API and a second while rendering it.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/algo"
	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// handleAlgoKinds serves the registry plus what has actually happened to it.
func (s *Server) handleAlgoKinds(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Kinds    []algo.Catalog   `json:"kinds"`
		Standing []any            `json:"standings"`
		Arms     map[string]int64 `json:"arm_readers"`
		Note     string           `json:"note,omitempty"`
	}{Kinds: algo.List(), Arms: map[string]int64{}, Standing: []any{}}

	if s.Store != nil {
		if stats, err := s.Store.AlgoStandingsByKind(r.Context()); err == nil {
			for _, st := range stats {
				out.Standing = append(out.Standing, st)
			}
		}
		if arms, err := s.Store.AlgoArmCounts(r.Context()); err == nil && arms != nil {
			out.Arms = arms
		}
	}
	if len(out.Standing) == 0 {
		out.Note = "no impressions yet: the standings fill as lists are served"
	}
	writeJSONBody(w, out)
}

// handleAlgoChoice stores or clears a reader's algorithm for one kind.
//
// Both verbs POST to one route because they are one decision: "this algorithm"
// or "no algorithm, follow the experiment".
func (s *Server) handleAlgoChoice(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		http.Error(w, "this server has no state database", http.StatusServiceUnavailable)
		return
	}
	// A choice is a write about a reader, so it MINTS a session when there is
	// none: a programmatic caller has no browser to visit first, and refusing
	// would make the endpoint unusable by the only clients that POST to it.
	// GET routes stay read-only -- a crawler must not mint a session per page.
	_, ownerKey, serr := s.arena().SessionKey(w, r)
	if serr != nil || ownerKey == "" {
		http.Error(w, "an algorithm choice needs a reader session, and one could not be created", http.StatusInternalServerError)
		return
	}
	kind := arena.ParseKind(r.FormValue("kind"))
	raw := strings.TrimSpace(r.FormValue("algo"))

	if r.FormValue("clear") == "1" || raw == "" {
		if err := s.Store.ClearAlgoChoice(r.Context(), ownerKey, kind); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONBody(w, map[string]any{"kind": kind, "cleared": true})
		return
	}

	id, why := algo.Parse(kind, raw)
	if why != "" {
		// A refusal is not a 400: the choice is honoured as the default and the
		// reason travels back, because erroring on "ppr" would hide that ppr is a
		// known algorithm this host cannot run.
		writeJSONBody(w, map[string]any{
			"kind": kind, "chosen": id, "requested": raw, "refused": why, "stored": false,
		})
		return
	}
	if err := s.Store.SetAlgoChoice(r.Context(), ownerKey, kind, string(id)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONBody(w, map[string]any{"kind": kind, "chosen": id, "stored": true})
}

// handleRelated serves the list one algorithm produces for one seed.
func (s *Server) handleRelated(w http.ResponseWriter, r *http.Request) {
	kind := arena.ParseKind(r.FormValue("kind"))
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "?id= must be a positive entity id", http.StatusBadRequest)
		return
	}
	n := 20
	if v := r.FormValue("n"); v != "" {
		if parsed, cerr := strconv.Atoi(v); cerr == nil && parsed > 0 {
			n = parsed
		}
	}

	// A GET here MINTS a session, unlike every other GET in this package.
	//
	// The reason is the surface's whole purpose: a list served to an
	// unassigned reader is not in the experiment, and an A/B whose arms only
	// fill when a browser happens to have visited a page first would measure
	// browsers rather than readers. The cost is a cookie and one row per
	// anonymous caller, which is the price of the log this route exists to
	// produce.
	_, ownerKey, _ := s.arena().SessionKey(w, r)
	var stored string
	if s.Store != nil && ownerKey != "" {
		if choices, cerr := s.Store.AlgoChoices(r.Context(), ownerKey); cerr == nil {
			stored = choices[kind]
		}
	}
	chosen, why := algo.Resolve(kind, r.FormValue("algo"), stored, ownerKey)
	arm := algoArmName(kind, chosen, stored, r.FormValue("algo"))

	res, err := s.Engine.Related(r.Context(), kind, id, chosen, n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Store != nil && ownerKey != "" {
		shown := make([]int64, 0, len(res.Items))
		for _, it := range res.Items {
			shown = append(shown, it.ID)
		}
		if err := s.Store.RecordAlgoImpression(r.Context(), ownerKey, kind, string(chosen), arm, id, shown); err != nil {
			s.Log.Warn("algo impression failed", "kind", kind, "algo", chosen, "err", err)
		}
	}

	writeJSONBody(w, map[string]any{
		"kind":    kind,
		"label":   kind.Label(),
		"seed":    id,
		"algo":    chosen,
		"arm":     arm,
		"refused": why,
		"items":   res.Items,
		"note":    res.Note,
		"missing": res.Missing,
		"took_ms": res.Millis,
		"default": algo.DefaultFor(kind),
	})
}

// algoArmName says why this algorithm ran. It is recorded, not reconstructed
// later: "was this the experiment or a choice" is a fact about the request.
func algoArmName(kind arena.EntityKind, chosen algo.ID, stored, requested string) string {
	switch {
	case requested != "":
		return "request"
	case stored != "":
		return "choice"
	case chosen == algo.DefaultFor(kind):
		return "default"
	default:
		return "challenger"
	}
}

// writeJSONBody is this package's response shape for these routes. (The existing
// algo handlers inline their own encoder; this is shared by the new ones.)
func writeJSONBody(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// handleEmbedDrift serves the last two embedding fingerprints and the sentence
// that compares them (#76).
//
// It lives in this file rather than a new one because it is the same shape of
// answer as the algorithm registry: a fact about the deployment that the site
// must be able to state about itself.
func (s *Server) handleEmbedDrift(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Runs  int               `json:"runs"`
		Drift *store.EmbedDrift `json:"drift,omitempty"`
		Note  string            `json:"note,omitempty"`
	}{}
	if s.Store == nil {
		out.Note = "no state database"
		writeJSONBody(w, out)
		return
	}
	runs, err := s.Store.EmbedRuns(r.Context(), 2)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Runs = len(runs)
	if len(runs) == 2 {
		d := store.DriftBetween(runs[1], runs[0])
		out.Drift = &d
	} else {
		out.Note = "fewer than two embedding builds have been fingerprinted; there is no comparison to make"
	}
	writeJSONBody(w, out)
}
