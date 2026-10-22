package api

import (
	"context"
	"fmt"
	"net/http"

	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// Interleaved ranking: the online half of the evaluation loop.
//
// /api/v1/algo/compare shows two lists side by side and asks which is better.
// That answer is explicit and trustworthy, and it costs a comparison; the list
// a reader judges is also, by construction, one of the two worst lists the
// system can produce. Interleaving asks the same question with ONE list: both
// algorithms contribute slots, and what the reader does next is the vote.
//
// The vote here is a READ MARK. It is the strongest signal this deployment
// already has that a work was actually wanted, and it is a deliberate act --
// not a scroll position, not a hover, not a click that might mean the reader
// was leaving. A seen mark (shown, not read) is weaker and is not counted:
// being shown something is not evidence you wanted it, and counting it would
// measure the recommender's own choice.

// creditReadMark is the one place a read mark turns into an interleave credit.
//
// It is called from the page form that records reads. Returning the arm is not
// for the response -- the reader does not care which algorithm showed them a
// story -- it is so the caller can log it when a credit lands on a session
// with no interleave in it, which is a symptom worth seeing.
func (s *Server) creditReadMark(ctx context.Context, ownerKey string, workID int64) (arm string, ok bool) {
	if s.Store == nil {
		return "", false
	}
	arm, _, _, ok, err := s.Store.CreditInterleave(ctx, ownerKey, workID)
	if err != nil {
		// Attribution failing must not fail the read mark: the reader's
		// request is to stop seeing a work, and that is already recorded.
		if s.Log != nil {
			s.Log.Warn("interleave credit failed", "work_id", workID, "err", err)
		}
		return "", false
	}
	return arm, ok
}

// handleInterleave ranks one seed through two tunes and merges the two lists.
//
// Recording the impressions is a WRITE ON A GET, which is unusual and
// deliberate: the impression log IS the response. A merged list the server
// cannot later attribute is a list that produced no evidence, and deferring
// the write to a client call would mean the clients that matter (a reader in a
// browser, following links) never send it.
func (s *Server) handleInterleave(w http.ResponseWriter, r *http.Request) {
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
	if s.Engine == nil || s.Engine.Corpus == nil || s.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this server has no corpus or no store; interleaving needs both",
		})
		return
	}

	algoA, algoB := q.Get("a"), q.Get("b")
	if algoA == "" {
		algoA = "default"
	}
	if algoB == "" {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("?b=<algorithm> is required: interleaving needs two algorithms"))
		return
	}
	if algoA == algoB {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("cannot interleave %q with itself: every slot would be "+
				"attributed to the same arm and the experiment would measure nothing", algoA))
		return
	}
	n := intParam(r, "n", 20)
	// EVEN, and the reason is the merge's own asymmetry: an odd slot count
	// gives the first arm the extra slot, so an odd n is a small systematic
	// advantage with no way to tell it apart from a real preference.
	if n < 2 || n > 50 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("n must be between 2 and 50"))
		return
	}
	if n%2 != 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"n must be even: an odd slot count gives arm a the extra slot, which is a "+
				"bias the standings cannot separate from a preference (got %d)", n))
		return
	}

	// Resolve both tunes before ranking, so an unknown name is a named error
	// rather than a column that silently ranks by the defaults.
	for _, name := range []string{algoA, algoB} {
		if name == "default" {
			continue
		}
		if _, err := s.Store.Tune(r.Context(), name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("no algorithm named %q", name),
				"hint":  "list them with GET /api/v1/algo/list, or define one with `kindred tune set`",
			})
			return
		}
	}

	side := func(algo string) ([]rank.Candidate, error) {
		res, err := s.Engine.Recommend(r.Context(), engine.Request{
			Kind:    seed.Kind,
			Seeds:   []engine.Seed{seed},
			Tune:    tuneFor(algo),
			Exclude: true,
			N:       n,
			// Pure relevance, for the same reason as /algo/compare: MMR's
			// novelty term is scaled against the score, so it would decide
			// the order for two tunes whose scores have different
			// magnitudes and the merge would measure MMR instead.
			Lambda: 1,
		})
		if err != nil {
			return nil, err
		}
		return res.Items, nil
	}

	itemsA, err := side(algoA)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("rank %q: %w", algoA, err))
		return
	}
	itemsB, err := side(algoB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("rank %q: %w", algoB, err))
		return
	}
	merged := rank.TeamDraft(itemsA, itemsB, algoA, algoB, n)

	sessionKey, ownerKey, err := s.arena().SessionKey(w, r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	_ = sessionKey // never logged; the owner key is all the log needs

	slots := make([]store.InterleaveSlot, 0, len(merged))
	for _, it := range merged {
		sideName := "a"
		if it.Arm == algoB {
			sideName = "b"
		}
		slots = append(slots, store.InterleaveSlot{WorkID: it.ID, Side: sideName, Slot: it.Slot})
	}
	if err := s.Store.RecordInterleave(r.Context(), ownerKey, seed.Kind, seed.ID, algoA, algoB, slots); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("record impressions: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"seed":      seedStr,
		"seed_kind": seed.Kind,
		"seed_id":   seed.ID,
		"a":         algoA,
		"b":         algoB,
		"n":         len(merged),
		"items":     merged,
		"exposure":  rank.ArmExposure(merged),
		"shared":    sharedIDs(itemsA, itemsB),
		// Said in the body because it is the contract that makes the
		// numbers mean anything: a read mark on one of these works is the
		// vote, and a second read mark on the same sighting is not.
		"credit": "a read mark on any item credits the arm that showed it, once",
	})
}

// handleInterleaveStandings returns the online results.
func (s *Server) handleInterleaveStandings(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this server has no store",
		})
		return
	}
	arms, pairs, err := s.Store.InterleaveStats(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"arms":        arms,
		"pairs":       pairs,
		"window_days": 30,
		"note": "credits are read marks on slots an arm contributed; impressions are " +
			"all slots shown, credited or not, so the rate is over slots rather than " +
			"over clicks. A difference is A minus B: negative means B was read more.",
	})
}
