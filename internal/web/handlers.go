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
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/profile"
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

	// CorpusQuery runs the corpus-analysis modes (fandom ranking, underrated,
	// tag neighbours) that have no seeds.
	//
	// It is an interface rather than the concrete type so a page test can
	// supply a canned result. The corpus queries are pure functions of the DB,
	// so faking them loses nothing worth testing.
	CorpusQuery CorpusQuerier

	// Profiles is the writable store of taste profiles.
	//
	// It is deliberately NOT the corpus handle: the corpus is opened read-only
	// (the mirror is 1.7 GB and shared with the API), and writing a profile
	// into a read-only database fails with "attempt to write a readonly
	// database" -- an error that names the symptom rather than the design
	// error. Two handles, two jobs, stated here rather than discovered at the
	// call site that breaks.
	Profiles ProfileStore
}

// CorpusQuerier is the corpus-analysis surface the pages need.
//
// These are the Runner's own methods, so *corpusquery.Runner satisfies the
// interface without a wrapper. Options (not a bespoke Request struct) because
// that is what the Runner takes, and an adapter layer whose only job is to
// rename a struct is a layer that can disagree with it.
type CorpusQuerier interface {
	FandomRanking(ctx context.Context, seedTagIDs []int32, opts corpusquery.Options) (corpusquery.Result, error)
	Underrated(ctx context.Context, opts corpusquery.Options) (corpusquery.Result, error)
	TagNeighbours(ctx context.Context, tag string, opts corpusquery.Options) (corpusquery.Result, error)
}

// ProfileStore is the read/write profile surface the pages need.
//
// Save/Load/List are the profile.Store methods. Put and Apply are NOT: the
// real package builds a profile from works (BuildFromWorks) and folds feedback
// in memory (Apply returns the new profile for the caller to inspect and
// save). So the page layer keeps that shape -- it inspects, then saves --
// rather than inventing a Store method that would save without showing anyone
// what changed.
type ProfileStore interface {
	Save(ctx context.Context, p *profile.Profile) error
	Load(ctx context.Context, name string) (*profile.Profile, error)
	List(ctx context.Context) ([]string, error)
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
	// The judge is the one POST a page accepts. It is a write, but it is
	// the completion of a form the arena itself rendered, and a form
	// cannot POST to a JSON API and land the user back on a page. It
	// records exactly the same thing /api/v1/arena/compare records.
	if r.Method == http.MethodPost && r.URL.Path == "/arena/judge" {
		d.postJudge(w, r)
		return
	}

	// Blocking is the same shape: a one-button form the /block page
	// rendered. A page POST is completed by the browser, not typed by
	// hand, so this does not widen the set of URLs that accept writes --
	// the guard below still rejects a POST to any other page URL.
	if r.Method == http.MethodPost && r.URL.Path == "/block" {
		d.postBlock(w, r)
		return
	}

	// Only GET and HEAD otherwise render a page. A POST to a page URL is
	// not a page, and answering it with HTML answers a different question
	// than the one asked. Writes go to /arena/judge or the API under
	// /api/v1/.
	// POST is allowed on exactly two pages: /profiles and /profile, which
	// build a taste profile and record a rating. Those are the only mutable
	// things a reader can do here, and hiding both behind curl would make the
	// profile feature unreachable from the site it is a profile OF.
	//
	// Everything else stays read-only, and the gate below says which: a 405
	// that names the methods that do work beats a 405 that says "writes go to
	// the API" when half of them no longer do.
	mutable := r.URL.Path == "/profiles" || r.URL.Path == "/profile"

	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPost:
		if !mutable {
			d.fail(w, r, http.StatusMethodNotAllowed,
				fmt.Errorf("%s is not allowed on %s; only /profiles and /profile accept a POST",
					r.Method, r.URL.Path))
			return
		}
	default:
		d.fail(w, r, http.StatusMethodNotAllowed,
			fmt.Errorf("%s is not a page method; use GET, or POST for /profiles and /profile", r.Method))
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
	case p == "/fandoms":
		d.renderFandoms(w, r)
	case p == "/underrated":
		d.renderUnderrated(w, r)
	case p == "/neighbours":
		d.renderNeighbours(w, r)
	case p == "/profiles":
		d.renderProfiles(w, r)
	case p == "/profile":
		d.renderProfile(w, r)
	case p == "/arena":
		d.renderArena(w, r)
	case p == "/leaderboard":
		d.renderLeaderboard(w, r)
	case p == "/my-ranking":
		d.renderMyRanking(w, r)
	case p == "/block":
		d.renderBlock(w, r)
	case strings.HasPrefix(p, "/rank/"):
		d.renderRank(w, r, strings.TrimPrefix(p, "/rank/"))
	default:
		d.notFound(w, r)
	}
}

// -- pages ------------------------------------------------------------------
// -- corpus analysis --------------------------------------------------------

// renderFandoms answers "which fandoms would this reader enjoy".
//
// The profile is optional and its absence is a different answer, not a
// failure: without one this is the corpus's own distribution, which is worth
// knowing and worth being able to say out loud.
func (d Deps) renderFandoms(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	base := d.base("Fandoms", "Fandoms")

	if d.CorpusQuery == nil {
		d.fail(w, r, http.StatusServiceUnavailable,
			errors.New("corpus queries are not enabled on this server"))
		return
	}

	q := r.URL.Query()
	gate, err := parseBoundedInt(q.Get("min_co_works"), -1, 100000,
		corpusquery.DefaultMinCoWorksToRank)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("min_co_works: %w", err))
		return
	}
	limit, err := parseBoundedInt(q.Get("n"), 1, 500, 100)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("n: %w", err))
		return
	}

	name := q.Get("profile")
	opts := corpusquery.Options{Limit: limit, MinCoWorks: gate, ProfileName: name}

	page := FandomsPage{Base: base, Profile: name, Gate: gate}
	if avail, err := d.profileNames(ctx); err == nil {
		page.Available = avail
	}

	// Fandom ranking is seeded by tag ids, and a named profile supplies them:
	// the profile is a bag of weighted tag ids, which is exactly what this
	// query ranks fandoms against.
	var seeds []int32
	if name != "" {
		if d.Profiles == nil {
			d.fail(w, r, http.StatusServiceUnavailable,
				errors.New("profiles are not enabled on this server"))
			return
		}
		p, err := d.Profiles.Load(ctx, name)
		if err != nil {
			d.fail(w, r, http.StatusNotFound,
				fmt.Errorf("no profile %q: %w (build one at /profiles)", name, err))
			return
		}
		seeds = profileTagIDs(p, 20)
		opts.ProfileName = p.Name
	}

	res, err := d.CorpusQuery.FandomRanking(ctx, seeds, opts)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	page.Rows = res.Rows
	page.Notes = res.Notes
	page.Truncated = res.Truncated
	d.page(w, "fandoms.html", page, http.StatusOK)
}

// renderUnderrated answers "what is good and under-looked-at".
//
// The filters shrink the candidate pool before scoring, which the page states:
// filtering a previous result set is not the same set, and a reader who
// believed otherwise would be surprised by the row that vanished.
func (d Deps) renderUnderrated(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if d.CorpusQuery == nil {
		d.fail(w, r, http.StatusServiceUnavailable,
			errors.New("corpus queries are not enabled on this server"))
		return
	}

	q := r.URL.Query()
	words, err := parseBoundedInt(q.Get("min_words"), 0, 10000000, 0)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("min_words: %w", err))
		return
	}
	limit, err := parseBoundedInt(q.Get("n"), 1, 500, 100)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("n: %w", err))
		return
	}
	complete := q.Get("complete") != ""

	// The eligibility filters go INTO the query, not onto its results.
	//
	// The first draft of this handler filtered the returned rows, which is a
	// different and worse operation: ranking a wide pool and dropping half the
	// results returns the top half of the old ranking, and the works a deeper
	// query would have promoted into the gap never get considered. The query
	// narrows the pool first, so the ranking is over what is left.
	res, err := d.CorpusQuery.Underrated(ctx, corpusquery.Options{
		Limit:       limit,
		MinWords:    int64(words),
		Complete:    complete,
		ProfileName: q.Get("profile"),
	})
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	page := UnderratedPage{
		Base: d.base("Underrated", "Underrated"), Rows: res.Rows, Notes: res.Notes,
		Truncated: res.Truncated, MinWords: int64(words), Complete: complete,
	}
	d.page(w, "underrated.html", page, http.StatusOK)
}

// renderNeighbours is the tag-co-occurrence view.
func (d Deps) renderNeighbours(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	base := d.base("Tag neighbours", "Tag neighbours")

	// No tag is the untouched form, not a 404: the page has to be able to say
	// "name a tag" in its own voice.
	if tag == "" {
		if d.CorpusQuery == nil {
			d.fail(w, r, http.StatusServiceUnavailable,
				errors.New("corpus queries are not enabled on this server"))
			return
		}
		d.page(w, "neighbours.html", NeighboursPage{Base: base}, http.StatusOK)
		return
	}
	if d.CorpusQuery == nil {
		d.fail(w, r, http.StatusServiceUnavailable,
			errors.New("corpus queries are not enabled on this server"))
		return
	}

	limit, err := parseBoundedInt(r.URL.Query().Get("n"), 1, 500, 50)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("n: %w", err))
		return
	}

	res, err := d.CorpusQuery.TagNeighbours(ctx, tag, corpusquery.Options{Limit: limit})
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	page := NeighboursPage{
		Base: base, Tag: tag, Rows: res.Rows, Notes: res.Notes,
		Truncated: res.Truncated,
	}
	// The tag's own frequency is needed to read a PMI value: 0.4 over 12 works
	// and 0.4 over 12,000 are not the same claim, and without the denominator
	// the page cannot tell which one a reader is looking at.
	if total, err := d.tagWorkCount(ctx, tag); err == nil {
		page.TotalCo = total
	}
	d.page(w, "neighbours.html", page, http.StatusOK)
}

// -- profiles ---------------------------------------------------------------

// renderProfiles lists stored profiles and builds new ones.
func (d Deps) renderProfiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := ProfilesPage{Base: d.base("Taste profiles", "Taste profiles")}

	names, err := d.profileNames(ctx)
	if err != nil {
		// A missing profile store is a configuration fact, not a fault in the
		// request, and the page still renders its empty list so the reader is
		// not met with a bare 500.
		d.log().Warn("profile list unavailable", "err", err)
	} else {
		page.Profiles, _ = d.profileRows(ctx, names)
	}

	if r.Method == http.MethodPost {
		if err := d.buildProfile(w, r, &page); err != nil {
			return
		}
		d.page(w, "profiles.html", page, http.StatusOK)
		return
	}
	d.page(w, "profiles.html", page, http.StatusOK)
}

// buildProfile handles the POST on /profiles.
//
// Errors are returned on the PAGE, not as a status: "work 999999 is not in
// this corpus" is something a reader can fix, and a 500 that says it is
// something they can only report.
func (d Deps) buildProfile(w http.ResponseWriter, r *http.Request, page *ProfilesPage) error {
	ctx := r.Context()
	if d.Profiles == nil || d.Engine == nil {
		page.Err = "profiles are not enabled on this server"
		return nil
	}
	if err := r.ParseForm(); err != nil {
		page.Err = fmt.Sprintf("could not read the form: %v", err)
		return nil
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	worksRaw := strings.TrimSpace(r.PostFormValue("works"))
	if name == "" {
		page.Err = "a profile needs a name"
		return nil
	}
	page.Works = worksRaw

	ids, err := parseWorkList(worksRaw)
	if err != nil {
		page.Err = err.Error()
		return nil
	}
	if len(ids) == 0 {
		page.Err = "list at least one work id, comma separated"
		return nil
	}

	p, err := profile.BuildFromWorks(ctx, d.Engine.Corpus.DB, ids, name)
	if err != nil {
		page.Err = fmt.Sprintf("could not build the profile: %v", err)
		return nil
	}
	if len(p.Tags) == 0 {
		page.Err = fmt.Sprintf(
			"works %s carry no tags in this corpus, so the profile would be empty",
			joinInts(ids))
		return nil
	}
	if err := d.Profiles.Save(ctx, p); err != nil {
		page.Err = fmt.Sprintf("could not save the profile: %v", err)
		return nil
	}

	page.Built = p.Name
	page.Works = ""
	names, err := d.profileNames(ctx)
	if err == nil {
		page.Profiles, _ = d.profileRows(ctx, names)
	}
	d.log().Info("profile built from web", "profile", p.Name, "works", len(ids), "tags", len(p.Tags))
	return nil
}

// renderProfile shows one profile's weights and takes a rating.
func (d Deps) renderProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if r.Method == http.MethodPost && name == "" {
		name = strings.TrimSpace(r.PostFormValue("name"))
	}
	// Heading is set here rather than left to the caller: every other page sets
	// both, and a page whose only heading is the <title> is a page with no
	// <h1> at all. Six pages shipped that way (an <h1> is the document's main
	// heading for a screen reader, and its absence is invisible in a rendered
	// screenshot).
	base := d.base("Taste profile", "Taste profile")

	page := ProfilePage{Base: base}
	if p, ok := d.loadProfile(&page, ctx, name); ok {
		page.Profile = *p
		page.Tags = d.profileTagRows(ctx, p)
	}

	if r.Method == http.MethodPost {
		d.rateWork(r, &page, name)
		// Reload, so the table shows the effect of the rating that was just
		// recorded rather than the state before it.
		if p, ok := d.loadProfile(&page, ctx, name); ok {
			page.Profile = *p
			page.Tags = d.profileTagRows(ctx, p)
		}
	}

	if page.Profile.Name != "" {
		base.Title = "Taste profile: " + page.Profile.Name
		d.page(w, "profile.html", page, http.StatusOK)
		return
	}

	// No name, or a name that does not load.
	//
	// 404 for "named but missing" and 200 for "untouched form", because they
	// are different facts and a reader needs to tell them apart. The first
	// draft of this was a map lookup keyed on `page.Err == ""` mapping true to
	// StatusNotFound, which returned 404 for a blank form and 200 for a
	// profile that does not exist -- exactly inverted, and the Playwright test
	// for it passed while both were wrong.
	status := http.StatusOK
	if name != "" && page.Err != "" {
		status = http.StatusNotFound
	}
	d.page(w, "profile.html", page, status)
}

// loadProfile fetches a profile, writing a reader-usable error into the page.
func (d Deps) loadProfile(page *ProfilePage, ctx context.Context, name string) (*profile.Profile, bool) {
	if name == "" {
		return nil, false
	}
	if d.Profiles == nil {
		page.Err = "profiles are not enabled on this server"
		return nil, false
	}
	p, err := d.Profiles.Load(ctx, name)
	if err != nil {
		page.Err = fmt.Sprintf("no profile %q: %v", name, err)
		return nil, false
	}
	return p, true
}

// rateWork records a rating against a profile.
func (d Deps) rateWork(r *http.Request, page *ProfilePage, name string) {
	ctx := r.Context()
	if name == "" {
		page.Err = "a rating needs a profile name"
		return
	}
	if d.Profiles == nil {
		page.Err = "profiles are not enabled on this server"
		return
	}
	if err := r.ParseForm(); err != nil {
		page.Err = fmt.Sprintf("could not read the form: %v", err)
		return
	}

	workID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("work")), 10, 64)
	if err != nil || workID <= 0 {
		page.Err = fmt.Sprintf("%q is not a work id", r.PostFormValue("work"))
		return
	}
	liked, err := parseRating(r.PostFormValue("rating"))
	if err != nil {
		page.Err = err.Error()
		return
	}

	p, err := d.Profiles.Load(ctx, name)
	if err != nil {
		page.Err = fmt.Sprintf("no profile %q: %v", name, err)
		return
	}

	// The work's tags are the ones that move. Without them a rating would
	// touch nothing, which is the same accepted-and-ignored shape as the
	// --profile-from-user bug: a form that records a click and changes no
	// weight.
	tagIDs, err := d.workTagIDs(ctx, workID)
	if err != nil {
		page.Err = fmt.Sprintf("work %d: %v", workID, err)
		return
	}
	if len(tagIDs) == 0 {
		page.Err = fmt.Sprintf("work %d carries no tags in this corpus, so a rating "+
			"of it would move no weights", workID)
		return
	}

	before := cloneWeights(p.Tags)
	updated := profile.Apply(p, profile.Feedback{
		WorkID: workID, Liked: liked, TagIDs: tagIDs,
		At: time.Now().UTC().Format(time.RFC3339),
	})
	if err := d.Profiles.Save(ctx, updated); err != nil {
		page.Err = fmt.Sprintf("could not save the rating: %v", err)
		return
	}

	page.Rated = workID
	page.RatedTags = d.movedTags(ctx, before, updated.Tags)
	d.log().Info("work rated from web", "profile", name, "work", workID, "liked", liked,
		"tags", len(tagIDs))
}

// parseRating maps the form's rating to a liked flag.
//
// Only two outcomes are representable. A "neutral" rating that did not move
// anything would be indistinguishable from a rating that was never submitted,
// so the form's neutral option is spelled out as making no change rather than
// as a value the code pretends to store.
func parseRating(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "like", "love", "1", "true", "yes":
		return true, nil
	case "dislike", "0", "false", "no":
		return false, nil
	case "neutral", "":
		return false, errors.New("neutral records nothing; pick like or dislike")
	default:
		return false, fmt.Errorf("%q is not a rating; use like or dislike", s)
	}
}

// -- profile helpers --------------------------------------------------------

// profileNames lists stored profile names, or nil if there is no store.
func (d Deps) profileNames(ctx context.Context) ([]string, error) {
	if d.Profiles == nil {
		return nil, errors.New("profiles are not enabled")
	}
	return d.Profiles.List(ctx)
}

// profileRows resolves names into display rows, skipping any that fail to
// load: one unreadable profile should not empty the list.
func (d Deps) profileRows(ctx context.Context, names []string) ([]ProfileRow, error) {
	if d.Profiles == nil {
		return nil, errors.New("profiles are not enabled")
	}
	out := make([]ProfileRow, 0, len(names))
	for _, n := range names {
		p, err := d.Profiles.Load(ctx, n)
		if err != nil {
			d.log().Warn("profile unreadable, skipping in list", "profile", n, "err", err)
			continue
		}
		out = append(out, ProfileRow{
			Name: n, Source: p.Source, Works: p.Works, Tags: len(p.Tags),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// profileTagRows resolves a profile's tag ids into named rows, heaviest first.
//
// Names come from the corpus, which is the only place they still exist: the
// in-memory CSR drops its name table after building, to return 25 MiB to the
// budget. A tag id with no name is dropped rather than shown blank, because a
// blank row is indistinguishable from a rendering bug.
func (d Deps) profileTagRows(ctx context.Context, p *profile.Profile) []ProfileTagRow {
	if p == nil || d.Engine == nil {
		return nil
	}
	ids := profileTagIDs(p, 200)
	if len(ids) == 0 {
		return nil
	}
	names := d.tagNames(ctx, ids)
	if len(names) == 0 {
		return nil
	}

	out := make([]ProfileTagRow, 0, len(ids))
	for _, id := range ids {
		n, ok := names[id]
		if !ok || n == "" {
			continue
		}
		out = append(out, ProfileTagRow{ID: id, Name: n, Weight: p.Tags[id]})
	}
	return out
}

// movedTags reports the weights that changed, largest movement first, so a
// rating shows its effect instead of asking the reader to infer it.
//
// The cap is on the RETURNED rows, not on what was compared: a work can carry
// forty tags and the reader only needs to see the ones that actually moved,
// heaviest first. Comparing every tag is what makes the effect checkable.
//
// Names are resolved in one query rather than one-per-row, because a rate that
// issues forty single-row lookups turns a one-click action into a visible stall.
func (d Deps) movedTags(ctx context.Context, before, after map[int32]float64) []ProfileTagRow {
	if len(after) == 0 || d.Engine == nil {
		return nil
	}
	type move struct {
		id int32
		dv float64
	}
	var moves []move
	for id, av := range after {
		if dv := av - before[id]; dv != 0 {
			moves = append(moves, move{id: id, dv: dv})
		}
	}
	if len(moves) == 0 {
		return nil
	}
	sort.Slice(moves, func(i, j int) bool {
		// Largest magnitude first: the movement that matters is the biggest,
		// whether it is up or down. A sort by signed value would put every
		// dislike below every like, which is not what "what changed" means.
		if absf(moves[i].dv) != absf(moves[j].dv) {
			return absf(moves[i].dv) > absf(moves[j].dv)
		}
		return moves[i].id < moves[j].id
	})
	if len(moves) > 12 {
		moves = moves[:12]
	}

	ids := make([]int32, len(moves))
	for i, m := range moves {
		ids[i] = m.id
	}
	names := d.tagNames(ctx, ids)

	out := make([]ProfileTagRow, 0, len(moves))
	for _, m := range moves {
		name, ok := names[m.id]
		if !ok || name == "" {
			continue // a blank row reads as a rendering bug
		}
		out = append(out, ProfileTagRow{ID: m.id, Name: name, Weight: m.dv})
	}
	return out
}

// tagNames resolves tag ids to names in one query.
//
// It returns nil rather than an error on a bad handle: every caller uses it to
// decorate a page, and a missing name is a cosmetic loss while a 500 is a lost
// page.
func (d Deps) tagNames(ctx context.Context, ids []int32) map[int32]string {
	if d.Engine == nil || len(ids) == 0 || ctx == nil {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx,
		`SELECT id, name FROM tags WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		d.log().Warn("tag name lookup failed", "ids", len(ids), "err", err)
		return nil
	}
	defer rows.Close()

	out := make(map[int32]string, len(ids))
	for rows.Next() {
		var id int32
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return out
		}
		out[id] = name
	}
	return out
}

// profileTagIDs returns a profile's heaviest tag ids, capped at n.
//
// Sorted by weight rather than taken from the map, because Go map order is
// randomised per iteration: an unsorted list would make the same profile rank
// fandoms differently on every page load, which is indistinguishable from a
// bug and impossible to reproduce.
func profileTagIDs(p *profile.Profile, n int) []int32 {
	if p == nil || len(p.Tags) == 0 {
		return nil
	}
	type kv struct {
		id int32
		w  float64
	}
	kvs := make([]kv, 0, len(p.Tags))
	for id, w := range p.Tags {
		kvs = append(kvs, kv{id, w})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].w != kvs[j].w {
			return kvs[i].w > kvs[j].w
		}
		return kvs[i].id < kvs[j].id
	})
	if n > 0 && len(kvs) > n {
		kvs = kvs[:n]
	}
	out := make([]int32, len(kvs))
	for i, k := range kvs {
		out[i] = k.id
	}
	return out
}

// tagWorkCount counts the works carrying a tag name.
func (d Deps) tagWorkCount(ctx context.Context, name string) (int, error) {
	if d.Engine == nil {
		return 0, errors.New("no corpus")
	}
	var n int
	err := d.Engine.Corpus.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM work_tags wt JOIN tags t ON t.id = wt.tag_id WHERE t.name = ?`,
		name).Scan(&n)
	return n, err
}

// workTagIDs returns the tag ids on a work.
func (d Deps) workTagIDs(ctx context.Context, workID int64) ([]int32, error) {
	if d.Engine == nil {
		return nil, errors.New("no corpus")
	}
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx,
		`SELECT tag_id FROM work_tags WHERE work_id = ?`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int32
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// parseWorkList parses a comma-separated work id list.
//
// Spaces and repeated commas are tolerated, because a list typed by hand is
// mostly spaces and a trailing comma, and rejecting it teaches nothing.
func parseWorkList(s string) ([]int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a work id; the list is comma separated, like 111, 222", part)
		}
		if n <= 0 {
			return nil, fmt.Errorf("work id %d is not positive", n)
		}
		out = append(out, n)
	}
	return out, nil
}

// joinInts renders ids for an error message, capped.
func joinInts(ids []int64) string {
	const max = 12
	parts := make([]string, 0, len(ids))
	for i, id := range ids {
		if i == max {
			parts = append(parts, fmt.Sprintf("... and %d more", len(ids)-max))
			break
		}
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ", ")
}

// absf is a local abs for float64.
func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// cloneWeights copies a weight map so a before/after comparison is possible.
func cloneWeights(m map[int32]float64) map[int32]float64 {
	out := make(map[int32]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// renderSearch answers one query against BOTH tags and works.
//
// It used to search tags only, and the search box was labelled "Search tags"
// to match -- an honest label for an honest limitation, and a limitation a
// reader feels immediately: a work that exists in the mirror cannot be found
// by its title, and a work you can name is the one work you most want to
// seed a recommendation from.
//
// Tags and works are searched separately and reported separately rather than
// merged into one ranked list. The two have no comparable score -- a tag
// match is a substring hit, a work match is a title hit -- so any combined
// ordering would be arbitrary, and an arbitrary ordering presented as "best
// matches" is worse than two honest lists. Both sections state which one is
// empty and why.
func (d Deps) renderSearch(w http.ResponseWriter, r *http.Request, q string) {
	ctx := r.Context()
	base := d.base("kindred", "")
	base.Query = q
	page := SearchPage{Base: base}

	if q != "" {
		d.searchTags(ctx, q, &page)
		d.searchWorks(ctx, q, &page)
	}
	d.page(w, "search.html", page, http.StatusOK)
}

// searchTags fills the tag half of the results.
func (d Deps) searchTags(ctx context.Context, q string, page *SearchPage) {
	const stmt = `SELECT id, name FROM tags WHERE name LIKE ? ESCAPE '\' ORDER BY name LIMIT ?`
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, stmt, "%"+likeEscape(q)+"%", 100)
	if err != nil {
		// A tag search that fails must not take the work half down with
		// it: the work results are still worth showing, and the page says
		// what it could not do rather than pretending the query was empty.
		page.TagError = err.Error()
		return
	}
	defer rows.Close()
	for rows.Next() {
		var h TagHit
		if err := rows.Scan(&h.ID, &h.Name); err != nil {
			page.TagError = err.Error()
			return
		}
		page.Results = append(page.Results, h)
	}
	if err := rows.Err(); err != nil {
		page.TagError = err.Error()
		return
	}
	page.Total = len(page.Results)
	page.Limited = len(page.Results) == 100
}

// searchWorks fills the work half of the results, matching the title.
//
// LIKE '%q%' is a table scan. That is the right trade at this size: the
// corpus is 112,935 works, a scan is a few tens of milliseconds, and an
// index on a leading-wildcard LIKE is not something SQLite can use anyway.
// The LIMIT is what keeps the response bounded, and it is applied in SQL
// rather than by counting a full result set in Go.
//
// Title and author are both searched. AO3 readers know a fic by its author at
// least as often as by its title, and `authors` is a comma-separated column
// in this corpus, so a substring match there finds a person by name.
func (d Deps) searchWorks(ctx context.Context, q string, page *SearchPage) {
	needle := "%" + likeEscape(q) + "%"
	const stmt = `
		SELECT w.id, w.title, w.authors, w.url, w.summary,
		       w.kudos, w.hits, w.word_count, w.language, w.complete
		FROM works w
		WHERE w.title LIKE ? ESCAPE '\' OR w.authors LIKE ? ESCAPE '\'
		ORDER BY w.kudos DESC, w.id
		LIMIT ?`
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, stmt, needle, needle, 50)
	if err != nil {
		page.WorkError = err.Error()
		return
	}
	defer rows.Close()
	for rows.Next() {
		var h WorkHit
		// Every one of these is NULLable in the real corpus, and scanning
		// NULL into a string takes out the whole page. The tag handler
		// learned this one column at a time; here they are all nullable
		// up front.
		var author, url, summary, language sql.NullString
		var kudos, hits, words, complete sql.NullInt64
		if err := rows.Scan(&h.ID, &h.Title, &author, &url, &summary,
			&kudos, &hits, &words, &language, &complete); err != nil {
			page.WorkError = err.Error()
			return
		}
		h.Author, h.URL, h.Summary, h.Language =
			author.String, url.String, summary.String, language.String
		h.Kudos, h.Hits, h.WordCount, h.Complete =
			kudos.Int64, hits.Int64, words.Int64, complete.Int64
		page.Works = append(page.Works, h)
	}
	if err := rows.Err(); err != nil {
		page.WorkError = err.Error()
		return
	}
	page.WorkTotal = len(page.Works)
	page.WorkLimited = len(page.Works) == 50
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
		// BOTH sentinels mean "not found", from two layers: the corpus has
		// its own vocabulary and returns corpus.ErrNotFound. Checking only
		// store.ErrNotFound sent every missing work to the 500 branch --
		// measured by the browser suite, which found /work/999999999
		// answering 500 while /work/1 answered 200. A reader following a
		// stale link got a server fault instead of "no such work".
		if errors.Is(err, store.ErrNotFound) ||
			errors.Is(err, corpus.ErrNotFound) ||
			errors.Is(err, sql.ErrNoRows) {
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

	// No seed parameter at all is NOT a 404.
	//
	// This guard used to be here and returned notFound, on the reasoning that a
	// ranking with no seeds has no meaning. But /recommend is a linked page
	// (it is in the nav, and it is where "rank from this" on a work page points
	// with the seed already filled in), so a reader who clicks it first sees a
	// 404 for a page they were invited to. The empty form renders instead, and
	// the empty-SEED case further down does the same.
	//
	// A 404 stays correct for a path kindred does not serve; this is a path it
	// does serve.
	seeds := make([]engine.Seed, 0, len(raw))
	// The seed boxes are echoed back even for ones that fail to parse, so a
	// typo comes back in the form with the other seeds intact instead of as a
	// 400 that loses the whole request.
	echo := make([]SeedRef, 0, len(raw))

	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue // the blank "add another" slot; not a seed
		}
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
		echo = append(echo, SeedRef{Kind: kind, ID: n, Raw: s})
	}
	if len(seeds) == 0 {
		// Every slot was empty, which is what the form looks like before
		// anyone types. A 404 here would be right for a bad URL and wrong for
		// an untouched form, so the form is rendered empty instead.
		d.page(w, "recommend.html",
			RecommendPage{Base: d.base("Recommendations", "Recommendations"), MaxSeeds: maxSeeds},
			http.StatusOK)
		return
	}

	// Five is enough to describe a taste and few enough that the ranking
	// still means something. A twentieth seed averages the signal away.
	seedLimitHit := len(seeds) > maxSeeds
	if seedLimitHit {
		seeds = seeds[:maxSeeds]
		echo = echo[:maxSeeds]
	}

	q := r.URL.Query()
	capN, err := parseBoundedInt(q.Get("max_per_fandom"), 0, 100, 0)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest,
			fmt.Errorf("max_per_fandom: %w (it caps results per group, 0-100)", err))
		return
	}
	groupBy := q.Get("group_by")
	switch groupBy {
	case "", "fandom", "tag", "author":
	default:
		d.fail(w, r, http.StatusBadRequest,
			fmt.Errorf("group_by %q is not a diversity axis; use fandom, tag or author", groupBy))
		return
	}

	n := 20
	if v := q.Get("n"); v != "" {
		n, err = parseBoundedInt(v, 1, 100, 20)
		if err != nil {
			d.fail(w, r, http.StatusBadRequest, fmt.Errorf("n: %w", err))
			return
		}
	}

	req := engine.Request{
		Seeds:   seeds,
		Kind:    corpus.AO3Kind,
		N:       n,
		Exclude: true,
		// The cap and its axis are passed through rather than applied to the
		// result list here. Post-filtering a top-N list is a different and much
		// worse algorithm: if the top 20 are all Harry Potter and the cap is 3,
		// filtering the 20 returns 3 when the corpus had 200 eligible works
		// outside that fandom. The cap has to shape the query.
		MaxPerGroup: capN,
		GroupBy:     groupBy,
	}

	res, err := d.Engine.Recommend(ctx, req)
	if err != nil {
		// A seed kind this mirror does not carry is the reader's mistake, and
		// gets a 400 naming the kind rather than a 500: "ao3_book:1" is a typo,
		// not a server fault.
		if errors.Is(err, engine.ErrUnsupportedKind) {
			d.fail(w, r, http.StatusBadRequest, err)
			return
		}
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	heading := fmt.Sprintf("Recommendations from %d seeds", len(seeds))
	if len(seeds) == 1 {
		heading = "Recommendations from one seed"
	}

	page := RecommendPage{
		Base:            d.base(heading, heading),
		Similar:         res.Items,
		Seeds:           echo,
		N:               n,
		Returned:        len(res.Items),
		MaxPerFandom:    capN,
		GroupBy:         groupBy,
		SeedLimitHit:    seedLimitHit,
		MaxSeeds:        maxSeeds,
		ShortfallReason: shortfallReason(n, len(res.Items), capN, groupBy),
	}
	page.Tune = weightRows(res.Tune)
	d.page(w, "recommend.html", page, http.StatusOK)
}

// maxSeeds is the seed ceiling. Five describes a taste; twenty averages it
// into mush, which is a worse answer than refusing to answer.
const maxSeeds = 5

// shortfallReason explains a short list, or returns "" when it needs none.
//
// It exists because "showing 12 of 20" reads as a complete answer, and the
// reader cannot otherwise tell a diversity cap from an exhausted corpus. The
// distinction matters: raising the cap is the fix for one and does nothing for
// the other.
func shortfallReason(want, got, capN int, groupBy string) string {
	if got >= want {
		return ""
	}
	if capN > 0 && groupBy != "" {
		return fmt.Sprintf(
			"The cap of %d per %s removed the rest; kindred will not pad the list "+
				"back up with the works it just excluded. Raise or clear the cap for more.",
			capN, groupBy)
	}
	return fmt.Sprintf("Only %d of %d works scored above zero, which is the whole of "+
		"what this corpus supports for these seeds. Widen the seeds before expecting more.", got, want)
}

// weightRows turns the engine's weight vector into display rows, heaviest
// first, dropping zero weights because a row of 0.00 teaches nothing.
func weightRows(w map[string]float64) []WeightRow {
	var out []WeightRow
	for name, v := range w {
		if v == 0 {
			continue
		}
		out = append(out, WeightRow{Name: name, Weight: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// parseBoundedInt parses an optional integer within bounds.
//
// The bounds are a feature, not validation boilerplate: max_per_fandom=10000
// would make the ranking do more work than the corpus, and n=100000 would page
// a list nobody reads. Out of range is a 400 rather than a clamp, because a
// silently clamped cap is a cap the reader did not ask for.
func parseBoundedInt(s string, lo, hi, def int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%d is outside %d-%d", n, lo, hi)
	}
	return n, nil
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
	ctx := context.Background()
	if d.Engine != nil && d.Engine.Corpus != nil && d.Engine.Corpus.DB != nil {
		if n, err := d.Engine.Corpus.CountWorks(ctx); err == nil {
			b.CorpusWorks = n
		}
	}
	d.fillFreshness(ctx, &b)
	return b
}

// fillFreshness reads the build timestamps the ingest command already writes
// and puts the age on the page.
//
// The point is that these keys have existed in the meta table all along
// (`index_built_at`, `corpus_built_at`) and nothing has ever read them, so
// the one fact that most changes what a recommendation is worth -- how old
// the data is -- was invisible. A stale index does not fail; it answers
// confidently from old data.
//
// Three states, kept distinct because each needs different words:
//
//	no timestamp recorded -> AgeKnown false. Say the age is unknown.
//	                    -> "0 days old" is a freshness claim from no data.
//	timestamp in the future -> negative age. A clock problem, named as one.
//	otherwise -> the age in days.
//
// An unreadable or absent timestamp leaves AgeKnown false, and the page then
// says the age is not recorded. It is never fatal: a corrupt meta row must not
// take every page on the site down, and the footer is the least important
// thing any page renders.
//
// An ABSENT key is not a warning. Store.Meta returns ("", nil) for a key
// that does not exist, so treating the empty string as an unreadable
// timestamp logged a warning on every page render of a server that had
// never been ingested -- the log said "unreadable" about a value that was
// never written. Three states, and the middle one is ordinary:
//
//	key missing         -> no warning. AgeKnown stays false; the page says
//	                      the age is unknown, which is honest.
//	key present, empty  -> warning. Something wrote an empty value.
//	key present,
//	  unparseable       -> warning. Same.
func (d Deps) fillFreshness(ctx context.Context, b *Base) {
	if d.Engine == nil || d.Engine.Store == nil {
		return
	}
	if raw, err := d.Engine.Store.Meta(ctx, "index_built_at"); err != nil {
		d.log().Warn("reading index_built_at failed", "err", err)
	} else if raw != "" {
		t, perr := parseBuildTime(raw)
		if perr != nil {
			d.log().Warn("index_built_at is unreadable", "value", raw, "err", perr)
			return
		}
		b.IndexBuiltAt = raw
		b.AgeKnown = true
		b.IndexAgeDays = int(time.Since(t).Hours() / 24)
		b.IndexAgeAbsDays = b.IndexAgeDays
		if b.IndexAgeAbsDays < 0 {
			b.IndexAgeAbsDays = -b.IndexAgeAbsDays
			b.AgeInFuture = true
		}
	}
	// The mirror's own stamp is a separate fact from the index build: an
	// index built today from a mirror written in March is three months out
	// of date however fresh the index is.
	if raw, err := d.Engine.Store.Meta(ctx, "corpus_built_at"); err != nil {
		d.log().Warn("reading corpus_built_at failed", "err", err)
	} else if raw != "" {
		if _, perr := parseBuildTime(raw); perr != nil {
			d.log().Warn("corpus_built_at is unreadable", "value", raw, "err", perr)
			return
		}
		b.CorpusBuiltAt = raw
	}
}

// parseBuildTime reads a build timestamp.
//
// store.Now() writes RFC3339, but a timestamp written by an older build, by
// hand, or by a different tool is often date-only or has a space instead of
// a T. Those are accepted rather than reported as corrupt, because the
// distinction between "not parseable" and "parseable but coarse" does not
// change what the page says, and refusing to show an age you can compute is
// the worse failure.
func parseBuildTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a timestamp this understands", s)
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
