package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// The algorithm A/B surface.
//
// An "algorithm" here is a NAMED TUNE — a weight vector over the scoring
// signals, which is exactly what ranks a list. That is the registry this
// deployment already has (`kindred tune set`, the `tunes` table), so there is
// no second place to define what an algorithm is.
//
// The surface is deliberately narrow: the same seed ranked twice through two
// tunes, side by side, and a way to say which list was better. Everything
// else — pair selection, Glicko ratings, the tag learner — belongs to the
// work arena and is NOT reused here. See the comment on `algo_comparisons`
// in the schema for why the two must not share a table.

// algoCompareResult is one side of a comparison.
type algoCompareResult struct {
	Algo string `json:"algo"`
	// Weights are the tune's own weight vector, echoed so a reader can see
	// WHAT differed between two columns whose items look similar. Without
	// it a comparison shows two lists and no cause.
	Weights map[string]float64 `json:"weights,omitempty"`
	Items   []rank.Candidate   `json:"items"`
	Error   string             `json:"error,omitempty"`
}

// handleAlgoCompare ranks one seed through two tunes and returns both lists.
//
// Both lists come from the SAME pool, computed once, so the only thing that
// differs between them is the weight vector. That is what makes the
// comparison a measurement rather than two independent requests whose pools
// happened to differ.
func (s *Server) handleAlgoCompare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	seedStr := q.Get("seed")
	if seedStr == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("?seed=kind:id is required"))
		return
	}
	seed, err := engine.ParseSeed(seedStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.Engine == nil || s.Engine.Corpus == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "no corpus is loaded on this server",
		})
		return
	}

	algoA := q.Get("a")
	algoB := q.Get("b")
	if algoA == "" {
		algoA = "default"
	}
	if algoB == "" {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("?b=<algorithm> is required: a comparison needs two algorithms"))
		return
	}
	// Comparing an algorithm with itself is refused by name rather than
	// answered with two identical lists. Two identical columns that a reader
	// is invited to judge produces a vote about nothing.
	if algoA == algoB {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("cannot compare %q with itself: the two lists would be identical "+
				"and the judgement would mean nothing", algoA))
		return
	}

	n := intParam(r, "n", 5)
	if n <= 0 || n > 50 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("n must be between 1 and 50"))
		return
	}

	// Resolve both tunes BEFORE ranking anything, so an unknown name is a
	// named error rather than a column that silently ranks by the defaults.
	// This is the same contract `?tune=` already has.
	names := map[string]bool{algoA: true, algoB: true}
	for name := range names {
		if name == "default" {
			continue
		}
		if s.Store == nil {
			writeErr(w, http.StatusServiceUnavailable,
				fmt.Errorf("no store: algorithm %q cannot be resolved", name))
			return
		}
		if _, err := s.Store.Tune(r.Context(), name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("no algorithm named %q", name),
				"hint":  "list them with GET /api/v1/algo/list, or define one with `kindred tune set`",
			})
			return
		}
	}

	side := func(algo string) algoCompareResult {
		res, err := s.Engine.Recommend(r.Context(), engine.Request{
			Kind:        seed.Kind,
			Seeds:       []engine.Seed{seed},
			Tune:        tuneFor(algo),
			Exclude:     true,
			N:           n,
			GroupBy:     "",
			MaxPerGroup: 0,
			PoolSize:    0,
		})
		if err != nil {
			return algoCompareResult{Algo: algo, Error: err.Error()}
		}
		return algoCompareResult{Algo: algo, Weights: res.Tune, Items: res.Items}
	}

	a := side(algoA)
	b := side(algoB)
	writeJSON(w, http.StatusOK, map[string]any{
		"seed":      seedStr,
		"seed_kind": seed.Kind,
		"seed_id":   seed.ID,
		"a":         a,
		"b":         b,
		// The overlapping ids, so a caller can see whether the two
		// algorithms disagree at all. Two lists that share every item and
		// differ only in order are still a comparison, but a caller who
		// cannot tell that apart from a real disagreement will misread
		// the vote.
		"shared": sharedIDs(a.Items, b.Items),
	})
}

// tuneFor maps an algorithm name onto the engine's tune parameter.
//
// "default" means the built-in weights, which the engine spells as an empty
// tune. Every other name is a tune name.
func tuneFor(algo string) string {
	if algo == "default" {
		return ""
	}
	return algo
}

// sharedIDs counts how many work ids appear in both lists.
func sharedIDs(a, b []rank.Candidate) int {
	seen := map[int64]bool{}
	for _, it := range a {
		seen[it.ID] = true
	}
	n := 0
	for _, it := range b {
		if seen[it.ID] {
			n++
		}
	}
	return n
}

// handleAlgoJudge records which of two algorithms produced the better list.
func (s *Server) handleAlgoJudge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seed   string `json:"seed"`
		A      string `json:"a"`
		B      string `json:"b"`
		Choice string `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	switch body.Choice {
	case "a", "b", "neither":
	default:
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("choice must be a, b or neither, got %q", body.Choice))
		return
	}
	seed, err := engine.ParseSeed(body.Seed)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.A == "" {
		body.A = "default"
	}
	if body.B == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("both algorithms must be named"))
		return
	}
	if s.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no store"})
		return
	}

	// The reader's owner key, when they have a session. A judgement with no
	// owner is still a judgement about the mirror rather than about a
	// person, which is what the empty-key standings read answers; it is
	// recorded rather than refused so an anonymous arena page still works.
	ownerKey := ""
	if key, err := s.Store.OwnerKey(r.Context(), s.sessionKeyFor(r)); err == nil {
		ownerKey = key
	}

	if err := s.Store.RecordAlgoComparison(r.Context(), ownerKey, store.AlgoComparison{
		SeedKind: seed.Kind,
		SeedID:   seed.ID,
		AlgoA:    body.A,
		AlgoB:    body.B,
		Winner:   body.Choice,
	}); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "choice": body.Choice})
}

// handleAlgoStandings returns the win/loss/tie record per algorithm.
func (s *Server) handleAlgoStandings(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no store"})
		return
	}
	ownerKey := ""
	// ?global=1 asks for every reader's judgements, which is what a
	// single-user mirror wants by default. Without it the standings are
	// scoped to the caller, because two readers' aesthetic preferences are
	// not each other's evidence.
	if !truthy(r.URL.Query().Get("global")) {
		if key, err := s.Store.OwnerKey(r.Context(), s.sessionKeyFor(r)); err == nil {
			ownerKey = key
		}
	}
	rows, err := s.Store.AlgoStandings(r.Context(), ownerKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"algorithms": rows,
		"scope":      map[bool]string{true: "every reader", false: "this reader"}[ownerKey == ""],
	})
}

// handleAlgoList names the algorithms that exist: the built-in default plus
// every stored tune.
//
// The list is what makes the compare endpoint discoverable. Without it a
// reader has to know that a tune name is an algorithm, which is the kind of
// undocumented coupling that makes a feature unreachable in practice.
func (s *Server) handleAlgoList(w http.ResponseWriter, r *http.Request) {
	names := []string{"default"}
	if s.Store != nil {
		tunes, err := s.Store.TuneNames(r.Context())
		if err == nil {
			names = append(names, tunes...)
		}
	}
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		entry := map[string]any{"name": n, "builtin": n == "default"}
		if n != "default" && s.Store != nil {
			if weights, err := s.Store.Tune(r.Context(), n); err == nil {
				entry["weights"] = weights
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"algorithms": out,
		"note": "an algorithm is a named tune: a weight vector over the ranking signals. " +
			"Compare two with GET /api/v1/algo/compare?seed=kind:id&a=NAME&b=NAME",
	})
}
