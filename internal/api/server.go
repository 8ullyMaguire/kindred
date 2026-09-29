// Package api serves the HTTP surface: recommendations, the unofficial AO3
// read API, peers, health and stats.
//
// Two rules hold across every handler:
//
//   - A response states what it could not do. A short list, a skipped
//     signal, a missing index — each is reported rather than papered over.
//     The previous deployment's failure mode was a plausible-looking
//     response built from two silently inert signals.
//   - Nothing in the request path reaches the network. Snapshot
//     publishing is a separate process; a request cannot trigger a fetch.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/budget"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// Server holds the handler dependencies.
type Server struct {
	Engine  *engine.Engine
	Store   *store.Store
	Log     *slog.Logger
	Version string
	// StartedAt is for the uptime figure in /healthz.
	StartedAt time.Time
	// Lite is reported so a client can tell a full run from a Pi run.
	Lite bool
}

// Routes builds the router.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Recommendation.
	mux.HandleFunc("POST /api/v1/recommend", s.handleRecommend)
	mux.HandleFunc("GET /api/v1/recommend", s.handleRecommend)
	mux.HandleFunc("GET /api/v1/tags/{id}/similar", s.handleSimilarTags)

	// The unofficial AO3 read API. Same shape as the endpoints a client
	// would use against ao3.org, so a caller can point at kindred instead.
	mux.HandleFunc("GET /api/v1/ao3/works", s.handleAO3Works)
	mux.HandleFunc("GET /api/v1/ao3/works/{id}", s.handleAO3Work)
	mux.HandleFunc("GET /api/v1/ao3/works/{id}/recommend", s.handleAO3Recommend)
	mux.HandleFunc("GET /api/v1/ao3/tags", s.handleAO3Tags)
	mux.HandleFunc("GET /api/v1/ao3/tags/{id}", s.handleAO3Tag)
	mux.HandleFunc("GET /api/v1/ao3/tags/{id}/works", s.handleAO3TagWorks)

	// Operations.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)

	// A 404 that is not JSON is a 404 a JSON client cannot read, and
	// net/http's default handler returns plain text. Every response this
	// server produces is JSON, including the ones nobody routed — so a
	// catch-all pattern handles them. Registered last, because ServeMux
	// prefers the most specific pattern.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no route for %s %s", r.Method, r.URL.Path))
	})

	return s.withLogging(s.withRecovery(mux))
}

// withRecovery turns a panic into a 500 rather than a dropped connection,
// and logs it. A panic in one signal must not take the process down when
// the whole point is to stay up on a Pi.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log().Error("panic in handler", "path", r.URL.Path, "panic", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": "internal error",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withLogging records method, path, status and duration. Not every
// request: the health check is polled and would drown the log.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log().Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// --- recommendation -------------------------------------------------------

func (s *Server) handleRecommend(w http.ResponseWriter, r *http.Request) {
	req, err := s.parseRecommend(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.Engine.Recommend(r.Context(), *req)
	if err != nil {
		s.writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) parseRecommend(r *http.Request) (*engine.Request, error) {
	q := r.URL.Query()
	// exclude_seeds defaults to true. A recommender that returns the work
	// you seeded from, at the top, with a perfect similarity score, is
	// worse than useless — and the default was off, so the API returned
	// the seed while the CLI excluded it. Two entry points to the same
	// ranking must not disagree about this.
	exclude := true
	if v := q.Get("exclude_seeds"); v != "" {
		exclude = truthy(v)
	}
	req := &engine.Request{
		Kind:        q.Get("kind"),
		Tune:        q.Get("tune"),
		GroupBy:     q.Get("group_by"),
		Exclude:     exclude,
		N:           atoiDefault(q.Get("n"), 10),
		PoolSize:    atoiDefault(q.Get("pool"), 0),
		MaxPerGroup: atoiDefault(q.Get("max_per_group"), 0),
	}

	var seeds []string
	switch r.Method {
	case http.MethodGet:
		// Repeated ?seed= and ?seed=a,b both work; a client that does not
		// know which the server prefers should not have to care.
		for _, s := range q["seed"] {
			for _, part := range strings.Split(s, ",") {
				if part = strings.TrimSpace(part); part != "" {
					seeds = append(seeds, part)
				}
			}
		}
	case http.MethodPost:
		var body struct {
			Seeds       []string `json:"seeds"`
			Kind        string   `json:"kind"`
			N           int      `json:"n"`
			Tune        string   `json:"tune"`
			MaxPerGroup int      `json:"max_per_group"`
			GroupBy     string   `json:"group_by"`
			Pool        int      `json:"pool"`
			Exclude     bool     `json:"exclude_seeds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("body is not JSON: %v", err)
		}
		seeds = body.Seeds
		if body.Kind != "" {
			req.Kind = body.Kind
		}
		if body.N != 0 {
			req.N = body.N
		}
		if body.Tune != "" {
			req.Tune = body.Tune
		}
		if body.MaxPerGroup != 0 {
			req.MaxPerGroup = body.MaxPerGroup
		}
		if body.GroupBy != "" {
			req.GroupBy = body.GroupBy
		}
		if body.Pool != 0 {
			req.PoolSize = body.Pool
		}
		// An absent exclude_seeds in the body keeps the server default of
		// true; an explicit false is honoured, because a client building a
		// "more like this, including this" list may genuinely want it.
		if !body.Exclude {
			req.Exclude = false
		}
	}
	if len(seeds) == 0 {
		return nil, errors.New("at least one seed is required: ?seed=kind:id")
	}
	for _, s := range seeds {
		parsed, err := engine.ParseSeed(s)
		if err != nil {
			return nil, err
		}
		req.Seeds = append(req.Seeds, parsed)
	}
	return req, nil
}

// writeEngineErr maps a domain error to a status. A missing seed is a 404
// because that is what it is, not a 400: the request was well formed and
// the thing it named does not exist.
func (s *Server) writeEngineErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound),
		errors.Is(err, corpus.ErrNotFound),
		errors.Is(err, sql.ErrNoRows):
		// Three sentinels mean "not found", from three layers. The corpus
		// has its own because it is a read-only mirror with its own
		// vocabulary; matching only the store's turned every "no such
		// work" into a 500.
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such entity: %v", err))
	case errors.Is(err, store.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func (s *Server) handleSimilarTags(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(w, r, "id")
	if err != nil {
		return
	}
	if s.Engine.Graph == nil {
		// No index: say so, with a status that says so. A 200 with an
		// empty list would read as "nothing is similar".
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the co-occurrence index is not loaded",
			"hint":  "run the ingest command on this host",
		})
		return
	}
	limit := atoiDefault(r.URL.Query().Get("n"), 20)
	total := 1.0
	if v, err := s.Store.MetaInt(r.Context(), "work_count"); err == nil {
		total = float64(v)
	}
	scores := s.Engine.Graph.NeighbourScore(int32(id), limit, total)
	out := make([]graph.ScoredNeighbour, 0, len(scores))
	for _, n := range scores {
		if n.Count <= 0 {
			// PMI is undefined for a zero-count pair. Report nothing
			// rather than a zero that would sort among the genuine
			// negatives.
			continue
		}
		out = append(out, n)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tag":       s.Engine.Graph.Name(int32(id)),
		"tag_id":    id,
		"similar":   out,
		"returned":  len(out),
		"requested": limit,
	})
}

// --- the unofficial AO3 API ----------------------------------------------

func (s *Server) handleAO3Works(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := clamp(atoiDefault(q.Get("limit"), 20), 1, 100)
	offset := max(0, atoiDefault(q.Get("offset"), 0))

	order := "w.hits DESC"
	switch q.Get("sort") {
	case "kudos":
		order = "w.kudos DESC"
	case "date":
		order = "w.update_date DESC"
	case "words":
		order = "w.word_count DESC"
	}

	query := `SELECT w.id, w.title, w.summary, w.url, w.word_count, w.kudos,
	                 w.hits, w.bookmarks, w.update_date, w.language, w.complete
	          FROM works w`
	args := []any{}
	if tag := q.Get("tag"); tag != "" {
		query += ` JOIN work_tags wt ON wt.work_id = w.id JOIN tags t ON t.id = wt.tag_id`
		args = append(args, tag)
		query += ` WHERE t.name = ?`
	}
	query += ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.Engine.Corpus.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	total := 0
	for rows.Next() {
		item, bookmarksNull, err := scanWorkJSON(rows)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		_ = bookmarksNull
		items = append(items, item)
		total++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"works":  items,
		"count":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) handleAO3Work(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(w, r, "id")
	if err != nil {
		return
	}
	ent, err := s.Engine.Corpus.Entity(r.Context(), id)
	if err != nil {
		s.writeEngineErr(w, err)
		return
	}

	pairs, err := s.Engine.Corpus.TagPairs(r.Context(), []int64{id})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	tags := make([]map[string]any, 0, len(pairs[id]))
	for _, p := range pairs[id] {
		tags = append(tags, map[string]any{"id": p.ID, "name": p.Name})
	}
	writeJSON(w, http.StatusOK, workJSON(ent, tags))
}

func (s *Server) handleAO3Recommend(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(w, r, "id")
	if err != nil {
		return
	}
	// The path parameter and the query parameter mean the same thing here;
	// accepting both means a client can use whichever form it already has.
	seeds := r.URL.Query()["seed"]
	if len(seeds) == 0 {
		seeds = []string{"ao3_work:" + strconv.FormatInt(id, 10)}
	}
	// Reuse the one recommend handler so both entry points behave
	// identically. A second code path here would be a second set of bugs.
	r2 := r.Clone(r.Context())
	q := r2.URL.Query()
	q.Del("seed")
	r2.URL.RawQuery = q.Encode()
	s.handleRecommendWithSeeds(w, r2, seeds)
}

func (s *Server) handleAO3Tags(w http.ResponseWriter, r *http.Request) {
	limit := clamp(atoiDefault(r.URL.Query().Get("limit"), 20), 1, 100)
	q := r.URL.Query().Get("q")

	query := `SELECT id, name FROM tags`
	args := []any{}
	if q != "" {
		query += ` WHERE name LIKE ?`
		args = append(args, "%"+q+"%")
	}
	query += ` ORDER BY name LIMIT ?`
	args = append(args, limit)

	rows, err := s.Engine.Corpus.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	tags := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		tags = append(tags, map[string]any{"id": id, "name": name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags, "count": len(tags)})
}

func (s *Server) handleAO3Tag(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(w, r, "id")
	if err != nil {
		return
	}
	var name string
	if err := s.Engine.Corpus.DB.QueryRowContext(r.Context(),
		`SELECT name FROM tags WHERE id = ?`, id).Scan(&name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("no tag %d", id))
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var n int64
	if err := s.Engine.Corpus.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM work_tags WHERE tag_id = ?`, id).Scan(&n); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": name, "work_count": n,
	})
}

func (s *Server) handleAO3TagWorks(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(w, r, "id")
	if err != nil {
		return
	}
	limit := clamp(atoiDefault(r.URL.Query().Get("limit"), 20), 1, 100)
	rows, err := s.Engine.Corpus.DB.QueryContext(r.Context(), `
		SELECT w.id, w.title, w.summary, w.url, w.word_count, w.kudos,
		       w.hits, w.bookmarks, w.update_date, w.language, w.complete
		FROM work_tags wt JOIN works w ON w.id = wt.work_id
		WHERE wt.tag_id = ? ORDER BY w.kudos DESC LIMIT ?`, id, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, _, err := scanWorkJSON(rows)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tag_id": id, "works": items, "count": len(items), "limit": limit,
	})
}

// --- operations -----------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap := budget.Snapshot("healthz")
	capKiB := budget.CapKiB(s.mode())
	overBudget := snap.PeakRSSKiB > capKiB
	body := map[string]any{
		"status":       "ok",
		"version":      s.Version,
		"lite":         s.Lite,
		"uptime_s":     int(time.Since(s.StartedAt).Seconds()),
		"peak_rss_kib": snap.PeakRSSKiB,
		"rss_cap_kib":  capKiB,
		"heap_mib":     snap.HeapAllocMiB,
		"budget_ok":    !overBudget,
	}
	if s.Store != nil {
		// work_count is the key the ingest writes. The first version of
		// this read "corpus_works", which nothing ever wrote, so a healthy
		// service reported corpus_works: 0 — a number that looks like a
		// measurement and is not one.
		if n, err := s.Store.MetaInt(r.Context(), "work_count"); err == nil {
			body["corpus_works"] = n
		}
	}
	// A process that has blown its budget says so, and returns 503: a
	// health check that reports ok while the RSS is 2x the cap is worse
	// than no health check.
	status := http.StatusOK
	if overBudget {
		body["status"] = "over_budget"
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, body)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{"version": s.Version, "lite": s.Lite}

	if s.Engine.Graph != nil {
		gs := s.Engine.Graph.Stats()
		out["graph"] = gs
	} else {
		out["graph"] = map[string]any{"loaded": false}
	}
	if s.Engine.Corpus != nil {
		works, _ := s.Engine.Corpus.CountWorks(ctx)
		tags, _ := s.Engine.Corpus.CountTags(ctx)
		wt, _ := s.Engine.Corpus.CountWorkTags(ctx)
		edges, _ := s.Engine.Corpus.CountEdges(ctx)
		out["corpus"] = map[string]any{
			"works": works, "tags": tags, "work_tags": wt, "cooccurrence_edges": edges,
		}
	}
	snap := budget.Snapshot("stats")
	capKiB := budget.CapKiB(s.mode())
	out["memory"] = map[string]any{
		"peak_rss_kib":  snap.PeakRSSKiB,
		"cap_kib":       capKiB,
		"heap_mib":      snap.HeapAllocMiB,
		"sys_mib":       snap.SysMiB,
		"goroutines":    snap.Goroutines,
		"within_budget": snap.PeakRSSKiB <= capKiB,
	}
	if s.Store != nil {
		if emb, err := s.Store.EmbeddingStats(ctx); err == nil {
			out["embeddings"] = emb
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// mode names the budget tier, so the cap follows the build flag rather
// than being configured twice.
func (s *Server) mode() string {
	if s.Lite {
		return "lite"
	}
	return "full"
}

// --- helpers --------------------------------------------------------------

func (s *Server) handleRecommendWithSeeds(w http.ResponseWriter, r *http.Request, seeds []string) {
	q := r.URL.Query()
	q.Set("seed", strings.Join(seeds, ","))
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = q.Encode()
	s.handleRecommend(w, r2)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The API is public data; caching briefly is a load win on a Pi.
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		// The status line is already sent, so this can only be logged.
		slog.Default().Error("write response", "err", err)
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{
		"error":  err.Error(),
		"status": status,
	})
}

// pathInt64 reads a {name} path value as an integer, writing the 400
// itself if it is not one.
func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%s must be an integer, got %q", name, raw))
		return 0, err
	}
	return n, nil
}

// scanWorkJSON projects a works row into the API's shape.
//
// Every nullable column is scanned into a sql.Null* and only then
// converted. Scanning straight into a string fails on NULL, and the real
// corpus has NULL summaries — the first version of this returned 500 for
// every list request, which is the same class of bug the corpus reader
// documents: a NULL is not a zero, and a zero is not a value.
//
// A NULL summary is emitted as null, not "". A client can tell "no
// summary" from "an empty summary", which matters for a field that is
// sometimes genuinely empty.
func scanWorkJSON(rows interface{ Scan(...any) error }) (map[string]any, bool, error) {
	var (
		id, wordCount, kudos, hits int64
		title, url, summary        sql.NullString
		language                   sql.NullString
		complete                   sql.NullInt64
		updateDate                 sql.NullString
		bookmarks                  sql.NullInt64
	)
	if err := rows.Scan(&id, &title, &summary, &url, &wordCount, &kudos, &hits,
		&bookmarks, &updateDate, &language, &complete); err != nil {
		return nil, false, err
	}
	item := map[string]any{
		"id":         id,
		"word_count": wordCount,
		"kudos":      kudos,
		"hits":       hits,
	}
	// Nullable text stays null when absent.
	item["title"] = nullStr(title)
	item["summary"] = nullStr(summary)
	item["url"] = nullStr(url)
	item["language"] = nullStr(language)
	item["update_date"] = nullStr(updateDate)
	// A NULL complete flag is not "incomplete": it is unknown. Emitting
	// false would claim a fact the corpus does not have.
	if complete.Valid {
		item["complete"] = complete.Int64 != 0
	} else {
		item["complete"] = nil
	}
	// bookmarks is NULL for 112,890 of 112,935 works, so it is null far
	// more often than not. Emitting 0 would make an unbookmarked work and
	// an unrecorded one indistinguishable, and a client sorting on it
	// would put the emptiest works first.
	item["bookmarks"] = nil
	if bookmarks.Valid {
		item["bookmarks"] = bookmarks.Int64
	}
	return item, !bookmarks.Valid, nil
}

// nullStr converts a nullable string, preserving NULL.
func nullStr(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

// workJSON projects a corpus entity into the API's work shape.
func workJSON(ent corpus.Entity, tags []map[string]any) map[string]any {
	return map[string]any{
		"kind":    ent.Kind,
		"id":      ent.ID,
		"title":   ent.Title,
		"url":     ent.URL,
		"summary": ent.Summary,
		"stats":   ent.Stats,
		"tags":    tags,
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// clamp bounds n into [lo,hi].
//
// A limit of 0 is clamped up to lo rather than treated as "no limit":
// "give me everything" is what an unbounded response looks like, and it is
// not something a query parameter should be able to ask for.
func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func truthy(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
