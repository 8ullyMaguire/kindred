package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// Deps is what the pages need from the rest of kindred.
//
// The corpus is reached through Engine.Corpus rather than passed in
// separately, because that is the only path the existing API handlers use
// and a second one is a second set of bugs.
type Deps struct {
	Engine  *engine.Engine
	Version string
	Lite    bool
	Log     *slog.Logger
}

// Pages returns the handler for every HTML page.
//
// It is mounted in front of the API, so it must never swallow an unmatched
// /api/v1/ request: a JSON client parsing an HTML error page gets a parse
// error and no message, which is strictly worse than the JSON 404 the API
// has always returned. So API paths are handed straight back to the API
// handler. See guardAPI, and the test that exists because this is a claim
// about ServeMux precedence rather than something to reason about.
func (d Deps) Pages(api http.Handler) http.Handler {
	pages := http.NewServeMux()
	pages.Handle("/static/", http.StripPrefix("/static/", Static()))
	pages.HandleFunc("/", d.route)
	return d.guardAPI(api, pages)
}

func (d Deps) guardAPI(api http.Handler, pages http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"),
			r.URL.Path == "/healthz",
			r.URL.Path == "/stats":
			api.ServeHTTP(w, r)
		default:
			pages.ServeHTTP(w, r)
		}
	})
}

func (d Deps) route(w http.ResponseWriter, r *http.Request) {
	// Only GET and HEAD render a page. A POST to a page URL is not a page,
	// and answering it with HTML answers a different question than the one
	// asked. Writes go to the API under /api/v1/, and the API takes them.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		d.fail(w, r, http.StatusMethodNotAllowed,
			fmt.Errorf("%s is not a page method; writes go to the API under /api/v1/", r.Method))
		return
	}

	p := r.URL.Path
	switch {
	case p == "/":
		d.renderSearch(w, r, "")
	case p == "/search":
		d.renderSearch(w, r, r.URL.Query().Get("q"))
	case strings.HasPrefix(p, "/work/"):
		d.renderWork(w, r, strings.TrimPrefix(p, "/work/"))
	case strings.HasPrefix(p, "/tag/"):
		d.renderTag(w, r, strings.TrimPrefix(p, "/tag/"))
	case p == "/recommend":
		d.renderRecommend(w, r)
	default:
		d.notFound(w, r)
	}
}

// -- pages ------------------------------------------------------------------

func (d Deps) renderSearch(w http.ResponseWriter, r *http.Request, q string) {
	ctx := r.Context()
	base := d.base("kindred", "")
	base.Query = q
	page := SearchPage{Base: base}

	if q != "" {
		const stmt = `SELECT id, name FROM tags WHERE name LIKE ? ESCAPE '\' ORDER BY name LIMIT ?`
		rows, err := d.Engine.Corpus.DB.QueryContext(ctx, stmt, "%"+likeEscape(q)+"%", 100)
		if err != nil {
			d.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var h TagHit
			if err := rows.Scan(&h.ID, &h.Name); err != nil {
				d.fail(w, r, http.StatusInternalServerError, err)
				return
			}
			page.Results = append(page.Results, h)
		}
		if err := rows.Err(); err != nil {
			d.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		page.Total = len(page.Results)
		page.Limited = len(page.Results) == 100
	}
	d.page(w, "search.html", page, http.StatusOK)
}

func (d Deps) renderWork(w http.ResponseWriter, r *http.Request, idStr string) {
	ctx := r.Context()
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		d.notFound(w, r)
		return
	}

	ent, err := d.Engine.Corpus.Entity(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			d.notFound(w, r)
			return
		}
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	page := WorkPage{Base: d.base(ent.Title, ent.Title), Work: ent}

	if pairs, err := d.Engine.Corpus.TagPairs(ctx, []int64{id}); err == nil {
		page.Tags = pairs[id]
	} else {
		// The page is still worth rendering without the tag list; say so
		// rather than dropping the whole work because one of three
		// lookups failed.
		d.log().Warn("tag pairs unavailable", "work", id, "err", err)
		page.RecommendErr = "the tag list for this work could not be read: " + err.Error()
	}

	// The recommendation is best-effort by design. A work page with no
	// recommendations has a hole in it; a 500 loses the work too, and the
	// work is the part the reader asked for. So the failure is rendered.
	res, err := d.Engine.Recommend(ctx, engine.Request{
		Seeds:   []engine.Seed{{Kind: corpus.AO3Kind, ID: id}},
		Kind:    corpus.AO3Kind,
		N:       10,
		Exclude: true,
	})
	if err != nil {
		d.log().Warn("recommend failed for work page", "work", id, "err", err)
		if page.RecommendErr == "" {
			page.RecommendErr = err.Error()
		}
	} else {
		page.Similar = res.Items
	}

	d.page(w, "work.html", page, http.StatusOK)
}

func (d Deps) renderTag(w http.ResponseWriter, r *http.Request, idStr string) {
	ctx := r.Context()
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		d.notFound(w, r)
		return
	}

	var name string
	if err := d.Engine.Corpus.DB.QueryRowContext(ctx,
		`SELECT name FROM tags WHERE id = ?`, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			d.notFound(w, r)
			return
		}
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	n := clampInt(atoiDefault(r.URL.Query().Get("n"), 20), 1, 100)
	page := TagPage{Base: d.base(name, name), Tag: TagInfo{ID: id, Name: name}}

	// The heading needs the real total, which is not the length of the
	// page. Conflating them is how a page ends up claiming a tag with
	// 3,000 works has twenty.
	var total int64
	if err := d.Engine.Corpus.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_tags WHERE tag_id = ?`, id).Scan(&total); err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	page.Tag.WorkCount = total
	page.Total = int(total)
	page.Limited = total > int64(n)

	// authors, url, summary and language are all NULLable in the corpus.
	// Scanning them into *string fails the whole page on a row that is
	// perfectly ordinary, which is what happened the first time: one
	// work with no author recorded took out every tag page that listed it.
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, `
		SELECT w.id, w.title, w.authors, w.url, w.summary,
		       w.kudos, w.hits, w.word_count, w.language, w.complete
		FROM work_tags wt JOIN works w ON w.id = wt.work_id
		WHERE wt.tag_id = ? ORDER BY w.kudos DESC, w.id LIMIT ?`, id, n)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var h WorkHit
		// Every one of these is NULLable: the real corpus has defaults
		// for some of them and a hand-built fixture omits all of them.
		// Discovering that one 500 at a time is how the first version of
		// this got three columns wrong in a row.
		var author, url, summary, language sql.NullString
		var kudos, hits, words, complete sql.NullInt64
		if err := rows.Scan(&h.ID, &h.Title, &author, &url, &summary,
			&kudos, &hits, &words, &language, &complete); err != nil {
			d.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		h.Author, h.URL, h.Summary, h.Language =
			author.String, url.String, summary.String, language.String
		h.Kudos, h.Hits, h.WordCount, h.Complete =
			kudos.Int64, hits.Int64, words.Int64, complete.Int64
		page.Works = append(page.Works, h)
	}
	if err := rows.Err(); err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	if d.Engine.Graph != nil {
		page.Neighbours = d.neighbours(ctx, id)
	}
	d.page(w, "tag.html", page, http.StatusOK)
}

// renderRecommend handles the multi-seed form:
// /recommend?seed=ao3_work:1&seed=ao3_work:2
func (d Deps) renderRecommend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	raw := r.URL.Query()["seed"]
	if len(raw) == 0 {
		d.notFound(w, r)
		return
	}

	seeds := make([]engine.Seed, 0, len(raw))
	for _, s := range raw {
		kind, idStr, ok := strings.Cut(s, ":")
		if !ok {
			d.fail(w, r, http.StatusBadRequest,
				fmt.Errorf("seed %q should look like ao3_work:20151202", s))
			return
		}
		n, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			d.fail(w, r, http.StatusBadRequest,
				fmt.Errorf("seed %q does not end in a number", s))
			return
		}
		seeds = append(seeds, engine.Seed{Kind: kind, ID: n})
	}
	// Five is enough to describe a taste and few enough that the ranking
	// still means something. A twentieth seed averages the signal away.
	if len(seeds) > 5 {
		seeds = seeds[:5]
	}

	res, err := d.Engine.Recommend(ctx, engine.Request{
		Seeds:   seeds,
		Kind:    corpus.AO3Kind,
		N:       20,
		Exclude: true,
	})
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	heading := "Recommendations"
	if len(seeds) == 1 {
		heading = "Recommendations from one seed"
	} else {
		heading = fmt.Sprintf("Recommendations from %d seeds", len(seeds))
	}
	page := WorkPage{Base: d.base(heading, heading), Similar: res.Items}

	// The first seed becomes the page subject so there is a real title
	// block, but a failure to load it is not fatal: the ranking is the
	// point of this page, not the metadata of one work.
	if ent, err := d.Engine.Corpus.Entity(ctx, seeds[0].ID); err == nil {
		page.Work = ent
	} else {
		d.log().Warn("seed work unavailable on recommend page", "seed", seeds[0].ID, "err", err)
	}
	d.page(w, "recommend.html", page, http.StatusOK)
}

// neighbours scores the tags around one tag and attaches their names.
//
// The scores come from the in-memory CSR, which is fast. The names cannot:
// the CSR drops its name table after building, to give 25 MiB back to the
// memory budget, so the corpus is the only place the strings still are.
// One query for the whole page, not one per neighbour.
func (d Deps) neighbours(ctx context.Context, id int64) []Neighbour {
	total := 1.0
	var works int64
	if err := d.Engine.Corpus.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM works`).Scan(&works); err == nil && works > 0 {
		total = float64(works)
	}

	scored := d.Engine.Graph.NeighbourScore(int32(id), 25, total)
	if len(scored) == 0 {
		return nil
	}

	names := make(map[int32]string, len(scored))
	args := make([]any, 0, len(scored))
	placeholders := make([]string, 0, len(scored))
	for _, s := range scored {
		args = append(args, int64(s.TagID))
		placeholders = append(placeholders, "?")
	}

	stmt := `SELECT id, name FROM tags WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	if rows, err := d.Engine.Corpus.DB.QueryContext(ctx, stmt, args...); err != nil {
		// Ids and scores are still worth showing. A missing name is not a
		// reason to drop a real result.
		d.log().Warn("neighbour names unavailable", "tag", id, "err", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var tid int64
			var name string
			if err := rows.Scan(&tid, &name); err == nil {
				names[int32(tid)] = name
			}
		}
	}

	out := make([]Neighbour, 0, len(scored))
	for _, s := range scored {
		out = append(out, Neighbour{
			ID:    int64(s.TagID),
			Name:  names[s.TagID],
			PMI:   s.PMI,
			Count: int64(s.Count),
		})
	}
	// A neighbour with no name sorts last, so a partial name lookup cannot
	// quietly promote a blank row to the top of the list.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Name == "") != (out[j].Name == "") {
			return out[j].Name == ""
		}
		return out[i].PMI > out[j].PMI
	})
	return out
}

// -- plumbing ---------------------------------------------------------------

func (d Deps) base(title, heading string) Base {
	b := Base{Title: title, Heading: heading, Version: d.Version}
	if d.Lite {
		b.Mode = "lite"
	} else {
		b.Mode = "full"
	}
	if d.Engine != nil && d.Engine.Corpus != nil && d.Engine.Corpus.DB != nil {
		if n, err := d.Engine.Corpus.CountWorks(context.Background()); err == nil {
			b.CorpusWorks = n
		}
	}
	return b
}

// page renders a template into a buffer, then writes it.
//
// The buffer is the point: a template that fails halfway has written no
// bytes, so the response is a clean error page rather than 200 with a
// document that stops mid-sentence.
func (d Deps) page(w http.ResponseWriter, name string, data any, status int) {
	var buf bytes.Buffer
	if err := Render(&buf, name, data); err != nil {
		d.log().Error("template failed", "template", name, "err", err)
		d.fail(w, nil, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (d Deps) notFound(w http.ResponseWriter, r *http.Request) {
	page := NotFoundPage{Base: d.base("Not found", "Not found"), Path: r.URL.Path}
	var buf bytes.Buffer
	if err := Render(&buf, "notfound.html", page); err != nil {
		// The 404 page itself failed. Fall back to text, because a broken
		// error page must not become a 500 loop.
		http.Error(w, "404 not found: "+r.URL.Path, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(buf.Bytes())
}

func (d Deps) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	if status >= 500 {
		d.log().Error("page failed", "status", status, "err", err)
	}
	path := "/"
	if r != nil {
		path = r.URL.Path
	}
	page := ErrorPage{
		Base:    d.base("Error", "Something went wrong"),
		Path:    path,
		Message: err.Error(),
	}
	var buf bytes.Buffer
	if rerr := Render(&buf, "error.html", page); rerr != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// Static serves the embedded CSS and JS.
//
// The content type is set by hand rather than sniffed by extension: these
// two files are the only ones served, and an explicit table cannot be
// wrong in the way a MIME database can.
func Static() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// Only reachable if the embed directive is broken, which is a
		// compile-time mistake, not a runtime condition.
		panic("web: embedded assets missing: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "style.css" {
			// Exactly one file is reachable. A request for anything else
			// is a probe or a typo and gets a plain 404 rather than a
			// directory listing of the embedded filesystem.
			//
			// There is no JavaScript to serve: the frontend is one
			// stylesheet and plain links, so the whole thing works with
			// scripting off.
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		data, err := fs.ReadFile(sub, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}

// likeEscape escapes the LIKE wildcards in a user's search term.
//
// Without this, searching "100%" returns every tag, and "a_b" matches
// "axb". It is a correctness bug dressed as a feature, and nobody reports
// it because the result looks plausible.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
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

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
