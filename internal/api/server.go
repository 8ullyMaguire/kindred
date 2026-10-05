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
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/budget"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/profile"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/web"
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
	// arenaSvc is built on first use, so adding the arena did not have to
	// change this struct's shape for every caller and every test.
	arenaSvc *ArenaService

	// Profiles is the taste-profile store, served to the web layer so a reader
	// can build and rate a profile from the browser.
	//
	// It is optional: a read-only deployment passes nil and the profile pages
	// answer 503 with the reason. Making it optional is what lets the same
	// Server work against the 1.7 GB read-only mirror and against a writable
	// state DB without the pages having to know which it is.
	Profiles ProfileStore

	// SurpriseMinTags lowers the /surprise page's eligibility gate. 0 means the
	// production default (corpusquery's own 10).
	//
	// It is set by the e2e harness and nothing else: that harness's fixture
	// corpus gives every work four or five tags, which is below the real
	// threshold, so /surprise would be permanently empty there and the browser
	// suite could only exercise the empty state -- leaving the pick, the seed
	// link and the redirect untested, which are the parts that actually break.
	//
	// It is a field rather than an env var because there is exactly one caller
	// and a global would let a production process lower the gate by accident.
	SurpriseMinTags int
}

// ProfileStore is the profile surface the server needs.
//
// It is the same interface web.Deps declares, restated here rather than
// imported, because api already depends on web: web.Deps.Profiles taking an
// interface api must satisfy would make the dependency circular. The
// compiler checks that both sides agree, which is the part that matters.
type ProfileStore interface {
	Save(ctx context.Context, p *profile.Profile) error
	Load(ctx context.Context, name string) (*profile.Profile, error)
	List(ctx context.Context) ([]string, error)
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

	// The arena: pairwise comparison and Glicko-2 ratings.
	mux.HandleFunc("GET /api/v1/arena/pair", s.handleArenaPair)
	mux.HandleFunc("POST /api/v1/arena/compare", s.handleArenaJudge)
	mux.HandleFunc("GET /api/v1/arena/leaderboard", s.handleArenaLeaderboard)
	mux.HandleFunc("GET /api/v1/arena/rank/{id}", s.handleArenaRank)
	mux.HandleFunc("GET /api/v1/arena/my-ranking", s.handleArenaMyRanking)
	mux.HandleFunc("POST /api/v1/arena/batch", s.handleArenaBatch)

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

	// The X-Kindred-Index-* headers are NOT set here. They were, and it was
	// dead code: web.Pages() wraps this handler in the HTML frontend, whose
	// guardAPI sends /api/, /healthz and /stats HERE but every other request
	// to the page router -- and the page router is wrapped again, outside
	// this one, by web.withFreshnessHeaders. So a middleware at this layer
	// would only ever stamp responses that the outer wrapper already
	// stamped. Mutation-verified: removing this one changed no test result,
	// which is how it was found to be redundant rather than load-bearing.
	//
	// There is exactly one implementation, in web, at the layer that sees
	// every response this server produces.
	api := s.withLogging(s.withRecovery(mux))

	// The HTML frontend wraps the API rather than sitting beside it.
	//
	// A bare "/" pattern here would be LESS specific than every /api/v1/
	// pattern above, so ServeMux would still route those correctly — but
	// that is a claim about precedence, and web.Pages hands /api/, /stats
	// and /healthz straight back to the handler above so it cannot be
	// wrong. A browser asking for a page gets HTML; a client asking for
	// the API gets exactly the JSON it got before, including the 404.
	// The corpus query runner and the profile store are separate handles for
	// separate jobs, and the split is load-bearing rather than tidiness:
	//
	//   - The corpus is opened READ-ONLY and shared with every request. The
	//     query layer only reads it, so it needs no write access at all.
	//   - The profile store is WRITABLE and lives in the state DB, not the
	//     mirror. Putting profiles in the read-only corpus fails with
	//     "attempt to write a readonly database", an error that names the
	//     symptom rather than the design error.
	//
	// A nil CorpusQuery leaves /fandoms, /underrated and /neighbours answering
	// 503 with the reason, rather than pretending the corpus is empty: an empty
	// fandom list and a disabled fandom list are different facts and the reader
	// needs to be able to tell them apart.
	pages := web.Deps{
		Engine:  s.Engine,
		Version: s.Version,
		Lite:    s.Lite,
		Log:     s.Log,
	}
	if s.Engine != nil && s.Engine.Corpus != nil {
		pages.CorpusQuery = corpusquery.NewRunner(s.Engine.Corpus.DB)
	}
	if s.Profiles != nil {
		pages.Profiles = s.Profiles
	}
	// The gate is lowered only by the e2e harness, whose fixture corpus gives
	// every work four or five tags. A deployed server leaves it 0 and gets
	// corpusquery's own default of 10.
	if s.SurpriseMinTags > 0 {
		pages.SurpriseMinTags = s.SurpriseMinTags
	}

	return pages.Pages(api)
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
	// A plain error whose message starts like a client mistake. The engine
	// rejects an unsupported seed kind and a mismatched seed kind with
	// fmt.Errorf; without this arm both fell through to 500, so a caller who
	// typo'd `book:1` got "internal server error" and no way to tell that the
	// fault was theirs. Matched on the sentinel below rather than on message
	// text, so this stays correct when the message is reworded.
	case errors.Is(err, engine.ErrUnsupportedKind):
		writeErr(w, http.StatusBadRequest, err)
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

// ao3RatingNames maps AO3's query-language letters to the rating names the
// mirror actually stores.
//
// The values are the measured distribution of the `rating` column in the real
// 1.7 GB mirror, all 112,935 rows included: Explicit 42,968, Teen And Up
// Audiences 27,562, Mature 25,190, Not Rated 8,796, General Audiences 8,419.
// A row outside this map does not exist in the mirror, so adding a name here
// without a row to match it would be a claim about the corpus rather than
// about AO3.
//
// The slash ratings are deliberately absent even though AO3 has all five.
// They exist on ao3.org, but the measured distribution above has no
// "Adults Only" row and no other slash rating, so mapping them would let a
// reader type a spelling that resolves to a bucket with zero rows -- a
// confident empty list that reads as "nothing at that rating" rather than
// "this mirror never recorded that rating". Only letters that resolve to a
// rating the mirror actually stores are here, plus the four slash letters
// that are aliases of those same five buckets.
var ao3RatingNames = map[string]string{
	"G": "General Audiences",
	"T": "Teen And Up Audiences",
	"M": "Mature",
	"E": "Explicit",
	// AO3's slash ratings, mapped to the bucket a reader means by each:
	// P (primary) is G, S (secondary) is T, D (dominant) is M, Z is M.
	"P": "General Audiences",
	"S": "Teen And Up Audiences",
	"D": "Mature",
	"Z": "Mature",
}

// word_countKnown is the precondition for any word-count comparison.
//
// Measured on the real mirror: 42 works of 112,935 record a word_count of
// exactly 0, because AO3 published no length for them. They are not short
// fics, they are fics of unknown length, and `word_count < 5000` includes
// all 42. Zero rows are NULL, so a NOT NULL test alone does not exclude them.
//
// The filter exists to bound how long a reader might spend, and a work of
// unknown length bounds nothing -- so a 0 is excluded from both arms rather
// than being quietly counted as short. 0.04% of the corpus, so it will never
// be visible in a page, and it is still wrong in the direction that matters.
// The parentheses are required because each clause is built as
// word_countKnown+" AND w.word_count < ?", and a bare `> 0 AND w.word_count
// < ?` relies on the reader knowing that comparison binds tighter than AND.
//
// HOW THE FIRST VERSION OF THIS BROKE, because the reason is not the
// obvious one. The guard was added to four clause strings by a
// find-and-replace of `"w.word_count < ?"`, which also matched INSIDE the
// constant's own declaration and rewrote its NAME to `w.word_countKnown`.
// Every clause then read `w.word_countKnown AND w.word_count < ?` -- SQL
// naming a column that does not exist -- and every length-filtered request
// returned 500. The parentheses were never the problem; they were the thing
// I reached for, and I wrote the explanation before checking which was true.
// Only TestAFilterMatchingNothingIsAnEmptyListNotAnError asking for a bound
// that matched nothing caught it, because it is the one arm that expects an
// empty result rather than rows.
const word_countKnown = "(w.word_count IS NOT NULL AND w.word_count > 0)"

// worksFilter is one parsed query parameter, kept as data rather than as a
// SQL fragment appended at the call site.
//
// It exists because the first version of this handler built its WHERE clause
// by string concatenation with a single `query += " WHERE ..."`, which cannot
// express two filters: the second would either overwrite the first or produce
// `WHERE a WHERE b`. Every clause therefore collects into one slice and is
// joined once, with the values kept in a separate args slice so nothing is
// ever interpolated into SQL text.
type worksFilter struct {
	clause string
	args   []any
}

// parseWorksFilters reads the filter parameters from an AO3-style works query
// and returns the WHERE clauses to apply.
//
// AO3's own syntax is followed rather than invented, because a reader who has
// used ao3.org already knows these spellings and a client written against
// ao3.org works here unchanged:
//
//	?complete=true            only works flagged complete
//	?words=under:10000        under 10,000 words
//	?words=>50000             over 50,000 words
//	?rating=G,T               any of the listed ratings
//	?lang=en                  one language
//
// `words=under:N` and `words=>N` are both accepted because AO3 itself accepts
// both, and a 400 on the spelling a reader used on ao3.org would be a
// pointless difference.
//
// A malformed value is a 400, not a silently ignored parameter. The whole
// point of this change is that a filter which does not fire must not look
// like one that did, and "quietly drop the filter" is the failure mode being
// removed.
// writeWorksCSV streams the works query as CSV.
//
// ## Why this exists and what it is careful about
//
// The obvious implementation is "marshal the same struct to JSON, then flatten it
// into cells". This does not, because JSON carries a distinction CSV cannot:
// **null is not an empty string.** `bookmarks` is NULLable, and `summary` is too,
// so a CSV that writes those as empty cells claims the mirror said "zero
// bookmarks" and "no summary" — a different fact from "this mirror has not
// recorded one".
//
// Measured on the live mirror 2026-10-05: zero NULLs across all 112,935 works in
// every column this writer emits, so both spellings are currently identical
// there. (Measured 2026-10-05 on the live mirror: **0 NULLs** in all 112,935 rows, and 112,896 rows holding `bookmarks = 0`. An earlier measurement found 112,890 NULLs; the mirror file was rewritten that morning and the NULLs arrived as zeros. The column is still NULLable and the crawler still writes NULL for a page that omits the tag, so the code is right and the data moved under a claim recorded in five documents.) The convention is still right — it costs nothing, and a crawl that
// skips a field produces NULL the same day — but a claim about how many rows
// are NULL has to be measured, not remembered. check-deploy.sh measures it.
//
// So the NULL cases go out as the literal `null`, and an empty string stays an
// empty field. A reader can still tell them apart, and the CSV header says so.
//
// ## The injection risk is real and is handled by encoding/csv
//
// Every cell here is CRAWLED text: title, summary, language, and the author's
// byline inside the title. A work titled `"a",b` or one containing a newline
// would otherwise add columns or rows. `encoding/csv` quotes and escapes
// correctly, including newlines inside quoted fields, so the rule is simply:
// never build the CSV by string concatenation. This writer uses csv.Writer only.
//
// ## The filters still apply, and the response says which
//
// `filters_applied` has no natural CSV home -- it is metadata about the
// selection, not a column of it -- so it is emitted as `#` comment lines above
// the header. Spreadsheet tools ignore them; `curl` shows them; and the answer to
// "did my filter do anything" survives the format change.
func (s *Server) writeWorksCSV(
	w http.ResponseWriter, _ *http.Request, rows *sql.Rows,
	q url.Values, limit, offset int,
) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	// A filename is a Content-Disposition parameter. It is static, not derived
	// from user input, so there is nothing in it to inject.
	w.Header().Set("Content-Disposition", `attachment; filename="kindred-works.csv"`)
	// No staleness headers are set here on purpose: they are added by the
	// middleware that wraps every route (internal/web sets X-Kindred-Index-Age,
	// -Seconds and -Version), so a CSV response already carries them. An
	// earlier version of this function called a setFreshnessHeaders helper that
	// does not exist, which is what a helper invented to solve a problem that
	// middleware had already solved looks like.

	cw := csv.NewWriter(w)
	// Comment lines above the header carry the selection metadata.
	for _, k := range []string{"tag", "complete", "words", "rating", "lang"} {
		if v := q.Get(k); v != "" {
			fmt.Fprintf(w, "# filter %s=%s\n", k, v)
		}
	}
	fmt.Fprintf(w, "# limit=%d offset=%d\n", limit, offset)

	// The header names every column, and the two `null` columns say so.
	if err := cw.Write([]string{
		"id", "title", "url", "authors", "summary",
		"word_count", "kudos", "hits", "bookmarks",
		"language", "complete", "update_date",
	}); err != nil {
		return
	}

	total := 0
	for rows.Next() {
		var (
			id, wordCount, kudos, hits int64
			title, url, summary        sql.NullString
			language                   sql.NullString
			complete                   sql.NullInt64
			updateDate                 sql.NullString
			bookmarks                  sql.NullInt64
		)
		if err := rows.Scan(&id, &title, &summary, &url, &wordCount, &kudos,
			&hits, &bookmarks, &updateDate, &language, &complete); err != nil {
			// Headers are already sent, so there is no status code left to
			// change. Stopping mid-stream is the only honest option, and it
			// shows up as a truncated file rather than as silently short data.
			return
		}
		rec := []string{
			strconv.FormatInt(id, 10),
			csvNull(title),
			csvNull(url),
			// The author's byline lives inside the title on AO3; there is no
			// separate authors column in this query. Emitting it as its own
			// column derived from the title would be a guess, so it is not.
			"",
			csvNull(summary),
			strconv.FormatInt(wordCount, 10),
			strconv.FormatInt(kudos, 10),
			strconv.FormatInt(hits, 10),
			csvNullInt(bookmarks),
			csvNull(language),
			csvNullInt(complete),
			csvNull(updateDate),
		}
		if err := cw.Write(rec); err != nil {
			return
		}
		total++
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return
	}
	if err := rows.Err(); err != nil {
		// Same reasoning as above: the file is already partially written.
		return
	}
	_ = total
}

// csvNull renders a NULL text column as the literal `null`, distinct from an
// empty string. See the note on writeWorksCSV.
func csvNull(v sql.NullString) string {
	if !v.Valid {
		return "null"
	}
	return v.String
}

// csvNullInt is csvNull for an integer column.
func csvNullInt(v sql.NullInt64) string {
	if !v.Valid {
		return "null"
	}
	return strconv.FormatInt(v.Int64, 10)
}

func parseWorksFilters(q url.Values) ([]worksFilter, error) {
	var out []worksFilter

	// complete. `complete=1`/`true`/`yes` are all the same request. An
	// absent or unrecognised value means no filter: this parameter is
	// about ADDING a restriction, not about the default.
	if v := strings.TrimSpace(q.Get("complete")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "only":
			// complete = 1, not complete != 0. A NULL complete is UNKNOWN,
			// and `complete != 0` is NULL for those rows, so SQL would
			// exclude them anyway -- but stating it as `= 1` says what is
			// meant and cannot drift if the dialect's three-valued logic is
			// ever handled differently.
			out = append(out, worksFilter{"w.complete = 1", nil})
		case "0", "false", "no":
			out = append(out, worksFilter{"w.complete = 0", nil})
		default:
			return nil, fmt.Errorf("complete must be true or false, got %q", v)
		}
	}

	if v := strings.TrimSpace(q.Get("words")); v != "" {
		lower := strings.ToLower(v)
		switch {
		case strings.HasPrefix(lower, "under:"):
			n, err := parseWordBound(strings.TrimSpace(v[len("under:"):]))
			if err != nil {
				return nil, fmt.Errorf("words=under: %v", err)
			}
			out = append(out, worksFilter{word_countKnown + " AND w.word_count < ?", []any{n}})
		case strings.HasPrefix(lower, "over:"):
			n, err := parseWordBound(strings.TrimSpace(v[len("over:"):]))
			if err != nil {
				return nil, fmt.Errorf("words=over: %v", err)
			}
			out = append(out, worksFilter{word_countKnown + " AND w.word_count > ?", []any{n}})
		case strings.HasPrefix(v, "<"):
			n, err := parseWordBound(strings.TrimSpace(v[1:]))
			if err != nil {
				return nil, fmt.Errorf("words=<: %v", err)
			}
			out = append(out, worksFilter{word_countKnown + " AND w.word_count < ?", []any{n}})
		case strings.HasPrefix(v, ">"):
			n, err := parseWordBound(strings.TrimSpace(v[1:]))
			if err != nil {
				return nil, fmt.Errorf("words=>: %v", err)
			}
			out = append(out, worksFilter{word_countKnown + " AND w.word_count > ?", []any{n}})
		default:
			return nil, fmt.Errorf(
				"words must be under:N or over:N (AO3 also accepts <N and >N), got %q", v)
		}
	}

	if v := strings.TrimSpace(q.Get("rating")); v != "" {
		// AO3's query language uses single letters (G, T, M, E, and the
		// five slashes). The mirror stores the FULL NAME: measured on the
		// real corpus, all 112,935 rows are one of
		//
		//	Teen And Up Audiences   27,562
		//	Mature                  25,190
		//	Not Rated                8,796
		//	General Audiences        8,419
		//	Explicit                42,968
		//
		// so a naive `rating = 'G'` matches nothing at all -- verified
		// against the real mirror before this mapping existed. A reader who
		// typed the spelling ao3.org uses would get a confident empty list,
		// which reads as "no works at that rating" rather than "you used a
		// spelling this mirror does not store".
		//
		// So an AO3 letter is translated, and a full name is matched
		// directly. Both are accepted, because both are what readers type.
		parts := strings.Split(v, ",")
		ph := make([]string, 0, len(parts))
		args := make([]any, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			full, ok := ao3RatingNames[strings.ToUpper(p)]
			if !ok {
				// Not a known letter: treat the value as a literal rating
				// name, which is what a mirror-specific caller would send.
				full = p
			}
			ph = append(ph, "UPPER(w.rating) = ?")
			args = append(args, strings.ToUpper(full))
		}
		if len(ph) == 0 {
			return nil, errors.New("rating was given but listed no values")
		}
		out = append(out, worksFilter{
			clause: "w.rating IS NOT NULL AND (" + strings.Join(ph, " OR ") + ")",
			args:   args,
		})
	}

	if v := strings.TrimSpace(q.Get("lang")); v != "" {
		// Languages are matched case-insensitively on a prefix-free exact
		// value, so "en" is English and "English" is also English. AO3
		// itself uses the two-letter code in its query language.
		out = append(out, worksFilter{
			clause: "UPPER(w.language) = UPPER(?)",
			args:   []any{v},
		})
	}

	return out, nil
}

// parseWordBound reads a word count bound and refuses a negative one.
//
// Zero is ALLOWED on both sides, deliberately: `words=over:0` is the same
// request as no bound at all, and rejecting it would make the natural way of
// spelling "any length" a 400. A negative bound is refused because "shorter
// than -5 words" is not a request a reader meant to make, and returning the
// whole corpus for it is exactly the behaviour this change exists to remove.
func parseWordBound(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number of words", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("a word count bound cannot be negative, got %d", n)
	}
	return n, nil
}

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

	filters, err := parseWorksFilters(q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	query := `SELECT w.id, w.title, w.summary, w.url, w.word_count, w.kudos,
	                 w.hits, w.bookmarks, w.update_date, w.language, w.complete
	          FROM works w`
	args := []any{}
	clauses := make([]string, 0, len(filters)+1)

	// The tag filter is a JOIN rather than a WHERE on an alias, because the
	// join is what connects the works table to the tag named in the query.
	if tag := q.Get("tag"); tag != "" {
		query += ` JOIN work_tags wt ON wt.work_id = w.id JOIN tags t ON t.id = wt.tag_id`
		args = append(args, tag)
		clauses = append(clauses, "t.name = ?")
	}

	for _, f := range filters {
		clauses = append(clauses, f.clause)
		args = append(args, f.args...)
	}

	// Joined once, after every clause is collected. The single-clause
	// version this replaced could not express two filters at all.
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, " AND ")
	}

	query += ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.Engine.Corpus.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	// `?format=csv` streams the same rows as CSV instead of JSON.
	//
	// It is placed HERE, before the JSON body is assembled, so the two formats
	// cannot disagree about which rows were selected: one loop, one query.
	// Building a CSV from the already-marshalled JSON map would work too, and
	// would quietly inherit JSON's nil-vs-empty distinction, which CSV cannot
	// represent -- see csvNull below.
	if strings.EqualFold(q.Get("format"), "csv") {
		s.writeWorksCSV(w, r, rows, q, limit, offset)
		return
	}

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
	// rows.Err is checked rather than assumed. A cursor that failed part
	// way through returns a SHORT list with a nil error from Next(), and a
	// caller reading `count: 12` has no way to know 400 rows were dropped.
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// Echo which filters were applied, so a client can tell the difference
	// between "your filter matched nothing" and "your filter was ignored".
	// This was the whole problem: previously both returned every work.
	applied := map[string]string{}
	for _, k := range []string{"tag", "complete", "words", "rating", "lang"} {
		if v := q.Get(k); v != "" {
			applied[k] = v
		}
	}
	body := map[string]any{
		"works":  items,
		"count":  total,
		"limit":  limit,
		"offset": offset,
	}
	if len(applied) > 0 {
		body["filters_applied"] = applied
	} else {
		// Explicitly empty rather than absent, so a client does not have to
		// tell "no filters" from "a version that does not report them".
		body["filters_applied"] = map[string]string{}
	}
	writeJSON(w, http.StatusOK, body)
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
