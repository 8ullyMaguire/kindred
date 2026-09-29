package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// Arena HTTP. The JSON API under /api/v1/arena/ and the HTML pages under
// /arena are two renderings of the same ArenaService, so a behaviour
// cannot be right on one and wrong on the other.

// arenaJSON is the shape of an arena work in a response.
type arenaJSON struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Author string `json:"author"`
	// Summary is trimmed: the arena shows two works at once and a full
	// AO3 summary defeats the comparison it is meant to inform.
	Summary string  `json:"summary"`
	Rating  float64 `json:"rating"`
	RD      float64 `json:"rd"`
	// Rated is false for a work nobody has compared, and the UI uses it to
	// say so rather than implying a confident 1500.
	Rated   bool   `json:"rated"`
	Tags    int    `json:"tags"`
	WorkURL string `json:"work_url"`
}

func (b WorkBrief) toJSON() arenaJSON {
	return arenaJSON{
		ID: b.ID, Title: b.Title, Author: b.Author,
		Summary: trimSummary(b.Summary),
		Rating:  round1(b.Rating.Mu), RD: round1(b.Rating.Phi),
		Rated: b.Rating.Phi < arena.PHI_INIT, Tags: len(b.Tags),
		WorkURL: fmt.Sprintf("/work/%d", b.ID),
	}
}

type pairJSON struct {
	A        arenaJSON `json:"a"`
	B        arenaJSON `json:"b"`
	Strategy string    `json:"strategy"`
	// Resumed is true when this pair was already shown and left unjudged.
	Resumed bool `json:"resumed"`
}

type leaderboardJSON struct {
	Entries []leaderboardRowJSON `json:"entries"`
	Median  float64              `json:"median"`
	// Note is rendered on the page, because a leaderboard with no stated
	// basis is the first thing a user stops trusting.
	Note string `json:"note"`
}

type leaderboardRowJSON struct {
	Rank        int     `json:"rank"`
	WorkID      int64   `json:"work_id"`
	Title       string  `json:"title"`
	Author      string  `json:"author"`
	Rating      float64 `json:"rating"`
	Effective   float64 `json:"effective"`
	RD          float64 `json:"rd"`
	Comparisons int     `json:"comparisons"`
	Wins        int     `json:"wins"`
	Losses      int     `json:"losses"`
	Draws       int     `json:"draws"`
	WorkURL     string  `json:"work_url"`
}

// handleArenaPair presents a comparison.
func (s *Server) handleArenaPair(w http.ResponseWriter, r *http.Request) {
	svc := s.arena()
	tagID := int64(0)
	if v := r.URL.Query().Get("tag_id"); v != "" {
		tagID, _ = strconv.ParseInt(v, 10, 64)
	}

	res, err := svc.Pair(r.Context(), w, r, tagID)
	if err != nil {
		if isNoPair(err) {
			// A NORMAL outcome, not a failure: a new user, or a pool too
			// small for an informative pair. 200 with an explanatory
			// message, so a client can render "nothing to compare right
			// now" instead of an error page.
			writeJSON(w, http.StatusOK, map[string]any{
				"pair": nil,
				"message": "No informative pair is available right now. " +
					"Either you have seen every pair in this pool, or the " +
					"arena needs more comparisons to separate these works.",
			})
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, pairJSON{
		A: res.A.toJSON(), B: res.B.toJSON(),
		Strategy: string(res.Strategy), Resumed: res.Resumed,
	})
}

// handleArenaJudge records a choice. A draw is a real judgement, not an
// absence: "neither" says these are equally uninteresting, and discarding
// it would bias ratings toward works that only get decisive verdicts.
func (s *Server) handleArenaJudge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		Choice  string `json:"choice"`
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
	// Fall back to the cookie when the client does not echo the session.
	session := body.Session
	if session == "" {
		if c, err := r.Cookie("kindred_arena"); err == nil {
			session = c.Value
		}
	}
	if session == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("no arena session"))
		return
	}

	if err := s.arena().Judge(r.Context(), session, body.Choice); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "choice": body.Choice})
}

// handleArenaLeaderboard returns the standings.
func (s *Server) handleArenaLeaderboard(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 50)
	minC := intParam(r, "min_comparisons", 0)

	entries, err := s.Store.Leaderboard(r.Context(), limit, minC)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Titles and authors come from the CORPUS, which is the read-only
	// mirror, and the ids come from our own state. A work in the leaderboard
	// whose corpus row has gone renders as a placeholder rather than
	// failing the whole page.
	rows, err := s.titlesFor(r, entries)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	out := leaderboardJSON{Entries: rows, Median: arena.MU_INIT,
		Note: "Ratings come from pairwise comparisons. A work shown at " +
			"1500 has not been compared yet, and sits at the middle because " +
			"that is where an unrated work honestly belongs."}
	for _, e := range entries {
		if e.Rank == 1 && e.Comparisons > 0 {
			out.Median = e.Rating.Mu
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleArenaRank returns one work's rating and history.
func (s *Server) handleArenaRank(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad work id: %w", err))
		return
	}
	row, err := s.Store.Rating(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	hist, err := s.Store.RatingHistory(r.Context(), id, 40)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{
		"work_id": id, "rating": round1(row.Rating.Mu), "rd": round1(row.Rating.Phi),
		"sigma": row.Rating.Sigma, "comparisons": row.Comparisons,
		"wins": row.Wins, "losses": row.Losses, "draws": row.Draws,
		"rated":    row.Comparisons > 0,
		"work_url": fmt.Sprintf("/work/%d", id),
	}
	if row.Comparisons == 0 {
		// Say so, rather than reporting 1500.0 as if it were a
		// measurement. The distinction between "unrated" and "rated at the
		// middle" is the whole cold-start story.
		out["message"] = "This work has not been compared yet, so it carries " +
			"the starting rating rather than a measured one."
	}
	series := make([]map[string]float64, 0, len(hist))
	for _, h := range hist {
		series = append(series, map[string]float64{
			"period": float64(h.Period), "rating": round1(h.Rating.Mu),
		})
	}
	out["history"] = series
	writeJSON(w, http.StatusOK, out)
}

// handleArenaMyRanking returns what one comparator has been shown, and
// what the arena has learned about their tag preferences.
func (s *Server) handleArenaMyRanking(w http.ResponseWriter, r *http.Request) {
	svc := s.arena()
	_, ownerKey, err := svc.SessionKey(w, r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	weights, err := s.Store.TagWeights(r.Context(), ownerKey, 12)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	liked, disliked := arena.TopTags(
		func() map[int64]float64 {
			m := make(map[int64]float64, len(weights))
			for _, w := range weights {
				m[w.TagID] = w.Weight
			}
			return arena.Normalise(m)
		}(), 6)

	names, err := s.tagNames(r, append(append([]int64{}, liked...), disliked...))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	toList := func(ids []int64) []map[string]any {
		out := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			n, ok := names[id]
			if !ok {
				continue // a tag the corpus no longer has
			}
			out = append(out, map[string]any{"tag_id": id, "name": n, "url": "/tag/" + strconv.FormatInt(id, 10)})
		}
		return out
	}

	stats, err := s.Store.Stats(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"liked_tags": toList(liked), "disliked_tags": toList(disliked),
		"arena": map[string]int{
			"rated_works": stats.RatedWorks, "comparisons": stats.Comparisons,
			"judged": stats.Judged,
		},
		"note": "Preferences are learned from the tags that differ between " +
			"the two works you were shown. A tag on both teaches nothing, " +
			"and a common tag teaches less than a rare one.",
	})
}

// handleArenaBatch runs the rating period. It is a POST because it writes
// every rating in the database, and a GET must never do that.
func (s *Server) handleArenaBatch(w http.ResponseWriter, r *http.Request) {
	res, err := s.arena().RunBatch(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if res.Skipped != "" {
		writeJSON(w, http.StatusOK, map[string]any{"skipped": res.Skipped})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period": res.Period, "comparisons": res.Comparisons, "works_updated": res.Updated,
	})
}

// titlesFor attaches corpus titles to leaderboard entries.
func (s *Server) titlesFor(r *http.Request, entries []store.LeaderboardEntry) ([]leaderboardRowJSON, error) {
	if len(entries) == 0 {
		return []leaderboardRowJSON{}, nil
	}
	ids := make([]any, len(entries))
	for i, e := range entries {
		ids[i] = e.WorkID
	}
	q := `SELECT id, COALESCE(title,''), COALESCE(authors,'') FROM works WHERE id IN (?` +
		strings.Repeat(", ?", len(entries)-1) + `)`
	rows, err := s.Store.Corpus.QueryContext(r.Context(), q, ids...)
	if err != nil {
		return nil, fmt.Errorf("leaderboard titles: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64][2]string, len(entries))
	for rows.Next() {
		var id int64
		var title, author string
		if err := rows.Scan(&id, &title, &author); err != nil {
			return nil, fmt.Errorf("leaderboard titles scan: %w", err)
		}
		byID[id] = [2]string{title, author}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]leaderboardRowJSON, 0, len(entries))
	for _, e := range entries {
		row := leaderboardRowJSON{
			Rank: e.Rank, WorkID: e.WorkID,
			Rating: round1(e.Rating.Mu), Effective: round1(e.Effective),
			RD: round1(e.Rating.Phi), Comparisons: e.Comparisons,
			Wins: e.Wins, Losses: e.Losses, Draws: e.Draws,
			WorkURL: fmt.Sprintf("/work/%d", e.WorkID),
		}
		if t, ok := byID[e.WorkID]; ok {
			row.Title, row.Author = t[0], t[1]
		} else {
			// The rating outlived the corpus row. Show the id rather than
			// an empty link, so the entry is still traceable.
			row.Title = fmt.Sprintf("work %d (not in corpus)", e.WorkID)
		}
		out = append(out, row)
	}
	return out, nil
}

// tagNames resolves ids to names for the "what I've learned" panel.
func (s *Server) tagNames(r *http.Request, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.Store.Corpus.QueryContext(r.Context(),
		`SELECT id, name FROM tags WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("tag names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// arena returns the service, built on first use so the Server struct does
// not have to change shape for every new feature.
func (s *Server) arena() *ArenaService {
	if s.arenaSvc == nil {
		s.arenaSvc = NewArenaService(s.Store, s.Log)
	}
	return s.arenaSvc
}

func isNoPair(err error) bool {
	return errors.Is(err, store.ErrNoPair)
}

func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// round1 is for display only. A rating is a float that means "about this
// much", and showing 1464.0583 implies a precision the measurement does not
// have -- the RD is 151.
func round1(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*10) / 10
}

func trimSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 280
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary, not a byte one, or a multi-byte summary ends
	// in a replacement character mid-word.
	r := []rune(s)
	for len(r) > 0 && len(string(r)) > max {
		r = r[:len(r)-8]
	}
	return strings.TrimRight(string(r), " ,.;:") + "..."
}
