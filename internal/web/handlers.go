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
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/profile"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
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

	// SurpriseMinTags lowers /surprise's eligibility gate. 0 means the
	// production value (corpusquery's own default of 10).
	//
	// It exists for the e2e suite, whose fixture corpus gives every work four or
	// five tags -- below ten, so /surprise would be permanently empty there and
	// the browser tests could only exercise the empty state. Lowering it lets
	// them exercise the pick, the seed link and the redirect, which are the parts
	// that can actually break.
	//
	// It is a field on Deps rather than a constant in the handler because the
	// threshold is a property of the CORPUS, not of the page: the page should not
	// decide what counts as well-tagged, and a server on the real mirror wants
	// the real number.
	SurpriseMinTags int
}

// CorpusQuerier is the corpus-analysis surface the pages need.
//
// These are the Runner's own methods, so *corpusquery.Runner satisfies the
// interface without a wrapper. Options (not a bespoke Request struct) because
// that is what the Runner takes, and an adapter layer whose only job is to
// rename a struct is a layer that can disagree with it.
// SurpriseMinTags lowers /surprise's eligibility gate. 0 means the production
// value (corpusquery's own default), which is what a deployed server wants.
//
// It exists for the e2e suite, whose fixture corpus gives every work four or five
// tags -- below the real threshold of ten, so /surprise would be permanently empty
// there and the browser tests could only exercise the empty state. Lowering the
// gate lets them exercise the pick, the seed link and the redirect, which are the
// parts that can actually break.
//
// The gate is a field on Deps rather than a constant in the handler because it is
// a property of the CORPUS, not of the page: the page should not decide what
// counts as well-tagged, and `kindred` on a 1.7 GB mirror wants the real number.
type SurpriseMinTags int

type CorpusQuerier interface {
	FandomRanking(ctx context.Context, seedTagIDs []int32, opts corpusquery.Options) (corpusquery.Result, error)
	Underrated(ctx context.Context, opts corpusquery.Options) (corpusquery.Result, error)
	TagNeighbours(ctx context.Context, tag string, opts corpusquery.Options) (corpusquery.Result, error)
	Surprise(ctx context.Context, opts corpusquery.Options) (corpusquery.Result, error)
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
	// The staleness headers go HERE, on the outermost handler, and nowhere
	// else in the tree.
	//
	// This layer is the only one that sees every response the server
	// produces: guardAPI sends /api/, /healthz and /stats to `api` and
	// everything else to `pages`, so middleware inside either branch covers
	// only part of the surface. An earlier version put this in the API
	// package instead, and a test asserting the header on /, /search,
	// /work/1 and /arena failed on all four while every /api/ route passed
	// -- the signature of middleware wired to the wrong layer. It was then
	// mutation-checked as redundant here and deleted.
	return withFreshnessHeaders(d, d.guardAPI(api, pages))
}

// withFreshnessHeaders stamps the index age onto HTML responses.
//
// SPEC §3.2.2 says EVERY response. An API client reads the header; a reader
// in a browser does not, which is why the same fact is also rendered in the
// footer. Two mechanisms for one requirement, because they reach different
// readers -- and the footer version is the one a human actually sees.
func withFreshnessHeaders(d Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := d.base("", "")

		// SPEC §3.2.3: "Server: kindred plus a JSON unofficial: true on
		// every document, and a human-visible line on the root page."
		//
		// The label half was never set: `grep -rn 'Header().Set("Server'`
		// found nothing. Go's net/http omits the Server header entirely by
		// default, so a client could not tell what it was talking to from
		// the response at all. Set before the handler runs, because the
		// header map seals at WriteHeader.
		w.Header().Set("Server", "kindred")
		// An X-Powered-By-style disclosure is the opposite of what this
		// project is for, so it is explicitly cleared rather than merely
		// left alone.
		w.Header().Set("X-Powered-By", "")

		w.Header().Set("X-Kindred-Index-Age", ageHeaderString(b))
		w.Header().Set("X-Kindred-Index-Age-Seconds", ageSeconds(b))
		w.Header().Set("X-Kindred-Index-Version", versionFor(b))
		next.ServeHTTP(w, r)
	})
}

// formatAge renders the age as a duration, or "unknown".
//
// "unknown" rather than "0s" for the same reason as the API header: a zero
// age claims the index was built just now, and that is the one claim a client
// would be most wrong to accept. -1 is the sentinel for it, and both header
// writers below turn that sentinel into the literal string "unknown" rather
// than into a negative number of seconds.
func formatAge(b Base) time.Duration {
	if !b.AgeKnown {
		return unknownAgeSentinel
	}
	if b.AgeInFuture {
		return -time.Duration(b.IndexAgeAbsDays) * 24 * time.Hour
	}
	return time.Duration(b.IndexAgeDays) * 24 * time.Hour
}

// unknownAgeSentinel marks "no build stamp recorded". Distinct from any real
// age, including zero.
const unknownAgeSentinel = time.Duration(-1)

// ageHeaderString is the human/machine spelling used in the header value.
func ageHeaderString(b Base) string {
	d := formatAge(b)
	if d == unknownAgeSentinel {
		return "unknown"
	}
	// NOT `time.Duration(b.IndexAgeDays) * 24 * time.Hour`. That is the age
	// in DAYS, and the index was built minutes ago: with IndexAgeDays == 0
	// (a sub-24h build) it produced "0s", which claims the mirror was
	// rebuilt this second. Verified live -- the header read `0s` five
	// minutes after a 5m26s ingest. The whole-day floor is for the footer's
	// prose, not for a header with second resolution.
	//
	// AgeSeconds exists precisely so this function has a real precision to
	// report; IndexAgeDays stays as the coarse value the footer prose reads.
	return (time.Duration(b.AgeSeconds) * time.Second).String()
}

// versionFor names the build, from an explicit version if the store has one
// and from the build date otherwise.
func versionFor(b Base) string {
	if b.IndexBuiltAt == "" {
		return "unknown"
	}
	if t, err := parseBuildTime(b.IndexBuiltAt); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	return "unknown"
}

// ageSeconds converts a duration to whole seconds for the machine-readable
// header. "unknown" is 0 seconds -- which is NOT the same claim as a measured
// zero, and is why both spellings are sent.
func ageSeconds(b Base) string {
	if !b.AgeKnown {
		return "0"
	}
	return strconv.FormatInt(int64(b.AgeSeconds), 10)
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

	// Marking a work read or unread. Same shape as blocking: a one-button
	// form the result list renders, so a page POST is completed by the
	// browser and this does not widen the set of URLs that accept writes.
	//
	// Without it, "I have read this" could only be recorded by hand-writing
	// a POST, and the read-exclusion half of the seen list would be
	// unreachable in practice while the shown half worked fine — a feature
	// that exists on the API and not on the page.
	if r.Method == http.MethodPost && r.URL.Path == "/seen" {
		d.postSeen(w, r)
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
	case p == "/author":
		d.renderAuthor(w, r)
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
	case p == "/surprise":
		d.renderSurprise(w, r)
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

// renderSurprise answers "show me something" -- one random work that is tagged
// enough to seed a ranking from.
//
// It exists because every other ranking page starts from a seed the reader
// chose, and choosing one requires knowing what is in the mirror. That is the
// part a first-time reader does not have, so the recommender was unreachable
// from its own front door.
//
// The pick is a redirect to /recommend with the seed filled in, rather than a
// result rendered here. Two reasons: the recommendation engine already renders
// its own explainability, and reusing it means this page cannot drift from what
// /recommend shows for the same seed.
func (d Deps) renderSurprise(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if d.CorpusQuery == nil {
		d.fail(w, r, http.StatusServiceUnavailable,
			errors.New("corpus queries are not enabled on this server"))
		return
	}

	// No limit is taken from the query string. The window is an implementation
	// detail of the selection, and letting a reader set it would mean a URL
	// could ask for a walk so long it looks like a hang.
	opts := corpusquery.Options{}
	if d.SurpriseMinTags > 0 {
		opts.MinTags = d.SurpriseMinTags
	}
	res, err := d.CorpusQuery.Surprise(ctx, opts)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	if len(res.Rows) == 0 {
		// An empty result is an ANSWER here, not a failure: the corpus may
		// simply have nothing well-tagged in it yet. Render the form with the
		// query's own explanation rather than a bare error page.
		d.page(w, "surprise.html", SurprisePage{
			Base:  d.base("Surprise", "Corpus"),
			Notes: res.Notes,
		}, http.StatusOK)
		return
	}

	row := res.Rows[0]
	seed := fmt.Sprintf("%s:%d", corpus.AO3Kind, row.WorkID)

	// Redirect when the reader asked for a redirect (?go=1), which is what the
	// button on the recommend form links to. Without ?go=1 the page renders
	// the pick and explains it, so the choice is inspectable before it is used.
	if r.URL.Query().Get("go") != "" {
		http.Redirect(w, r, "/recommend?seed="+url.QueryEscape(seed), http.StatusFound)
		return
	}

	d.page(w, "surprise.html", SurprisePage{
		Base:  d.base("Surprise", "Corpus"),
		Row:   row,
		Seed:  seed,
		Notes: res.Notes,
	}, http.StatusOK)
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
	page.N = clampInt(atoiDefault(r.URL.Query().Get("n"), 50), 1, 200)
	page.NextN = page.N * 2

	if q != "" {
		// An AO3 work id or work URL in the box is an ADDRESS, not a
		// search term: resolve it first and render it above whatever the
		// fuzzy match finds. A URL-shaped query that names an id this
		// mirror does not hold also says so -- that is a reader holding a
		// link, and "nothing matched" would hide a fact they need.
		if id, fromURL := searchWorkID(q); id > 0 {
			if hit, err := d.workHitByID(ctx, id); err != nil {
				page.WorkError = err.Error()
			} else if hit != nil {
				page.Exact = hit
			} else if fromURL {
				page.ExactMissing = id
			}
		}
		d.searchTags(ctx, q, &page)
		d.searchWorks(ctx, q, &page)
	}
	d.page(w, "search.html", page, http.StatusOK)
}

// searchWorkID reports the AO3 work id a query names, if any, and whether
// the query was URL-shaped.
//
// URL-shaped (contains "/works/123") matters because it changes what a MISS
// means: a pasted link naming a work this mirror lacks is worth saying
// outright, while a bare number that matches nothing is just a search term
// that happened to be digits ("1984" is not a broken link).
func searchWorkID(q string) (id int64, fromURL bool) {
	q = strings.TrimSpace(q)
	if strings.Contains(q, "/works/") {
		if m := ao3WorkPathRe.FindStringSubmatch(q); m != nil {
			if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				return v, true
			}
		}
		return 0, false
	}
	if bareDigitsRe.MatchString(q) {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil {
			return v, false
		}
	}
	return 0, false
}

var (
	// ao3WorkPathRe finds "/works/12345" in any URL or path-like string.
	// Deliberately host-agnostic: fixtures and mirrors other than
	// archiveofourown.org carry the same path shape, and rejecting a
	// correct link over a hostname would be clever in the wrong direction.
	ao3WorkPathRe = regexp.MustCompile(`/works/(\d{1,12})`)
	// bareDigitsRe is a query that is nothing but a work id.
	bareDigitsRe = regexp.MustCompile(`^\d{1,12}$`)
)

// workHitByID fetches one work by id for the exact-match block.
// Returns (nil, nil) when the mirror does not hold the id.
func (d Deps) workHitByID(ctx context.Context, id int64) (*WorkHit, error) {
	const stmt = `SELECT ` + workHitCols + ` FROM works w WHERE w.id = ?`
	row := d.Engine.Corpus.DB.QueryRowContext(ctx, stmt, id)
	h, err := scanWorkHit(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// workHitCols is the ten-column work shape every list on this site reads.
// One constant, one scanner: a column added in one place and forgotten in
// another is how two pages disagree about the same work.
const workHitCols = `w.id, w.title, w.authors, w.url, w.summary,
	w.kudos, w.hits, w.word_count, w.language, w.complete`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// scanWorkHit reads the workHitCols shape. Every column is NULLable in the
// real corpus, so all of them scan through sql.Null*: discovering the
// nullable ones one 500 at a time is how the first version of the tag page
// lost a whole list to one work with no author recorded.
func scanWorkHit(row rowScanner) (WorkHit, error) {
	var h WorkHit
	var author, url, summary, language sql.NullString
	var kudos, hits, words, complete sql.NullInt64
	if err := row.Scan(&h.ID, &h.Title, &author, &url, &summary,
		&kudos, &hits, &words, &language, &complete); err != nil {
		return h, err
	}
	h.Author, h.URL, h.Summary, h.Language =
		author.String, url.String, summary.String, language.String
	h.Kudos, h.Hits, h.WordCount, h.Complete =
		kudos.Int64, hits.Int64, words.Int64, complete.Int64
	return h, nil
}

// searchTokens splits a query into the terms the work half ANDs together.
//
// "snape harry" finds works whose title-or-author carries BOTH tokens, so a
// reader can name a fic by its title and its author in one box -- the
// single most natural way to type it, and one the whole-string LIKE could
// never match. Capped at eight terms: past that the AND-chain matches
// nothing anyway, and an unbounded clause list is a query-shaped hang.
func searchTokens(q string) []string {
	fields := strings.Fields(q)
	if len(fields) > 8 {
		fields = fields[:8]
	}
	return fields
}

// searchTags fills the tag half of the results.
func (d Deps) searchTags(ctx context.Context, q string, page *SearchPage) {
	// The tag limit rides along with ?n= when the reader raised it, so one
	// parameter widens both halves of the page instead of only the one
	// that happens to be longer.
	limit := 100
	if page.N > limit {
		limit = page.N
	}
	const stmt = `SELECT id, name FROM tags WHERE name LIKE ? ESCAPE '\' ORDER BY name LIMIT ?`
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, stmt, "%"+likeEscape(q)+"%", limit)
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
	page.Limited = len(page.Results) == limit
}

// searchWorks fills the work half of the results, matching the title.
//
// LIKE '%q%' on works is a full table scan: 112,935 rows on the real mirror.
//
// MEASURED on thinkcentre against the 1.7 GB mirror, not estimated:
//
//	title LIKE '%dark%' OR authors LIKE '%dark%', ORDER BY kudos LIMIT 50
//	  -> 0.06 s, 6.2 MB peak RSS
//	  -> 0.06 s for a query matching a third of the corpus ('%the%')
//	the tag query that has always shipped
//	  -> 0.07 s, 5.4 MB peak RSS
//
// So the works search costs the same as the tag search it sits beside, and
// the worst case is not worse than the ordinary case: the LIMIT is pushed
// into SQL, so SQLite stops after 50 rows even when the predicate matches
// 40,000. A first run took 3.5 s and the next took 0.06 s -- page cache, not
// a warm-up curve worth engineering around.
//
// An index cannot serve a leading-wildcard LIKE, and a trigram table would
// be a new index over 112,935 rows to save 60 ms on a request a reader
// waits for once. Not worth it at this size; re-measure at 10x the corpus.
//
// Title AND author are both searched because an AO3 reader knows a fic by
// its author at least as often as by its title, and `authors` holds a
// comma-separated list, so a substring match there finds a person by name.
// Both columns are non-NULL for all 112,935 rows of the real mirror
// (measured), but they are still scanned into sql.Null* because the shape of
// the corpus is not a promise a reader of this code can make about every
// mirror.
func (d Deps) searchWorks(ctx context.Context, q string, page *SearchPage) {
	tokens := searchTokens(q)
	if len(tokens) == 0 {
		return
	}

	// Every token must appear in the title OR the author, so a two-word
	// "title author" query matches the work whose title carries one word
	// and byline the other -- the whole-string LIKE could only ever match
	// one contiguous string, which is why that combination never worked.
	var args []any
	var clauses []string
	for _, t := range tokens {
		clauses = append(clauses, `(w.title LIKE ? ESCAPE '\' OR w.authors LIKE ? ESCAPE '\')`)
		like := "%" + likeEscape(t) + "%"
		args = append(args, like, like)
	}

	stmt := `SELECT ` + workHitCols + `
		FROM works w
		WHERE ` + strings.Join(clauses, " AND ")

	// The exact match already rendered above must not repeat below it.
	if page.Exact != nil {
		stmt += ` AND w.id <> ?`
		args = append(args, page.Exact.ID)
	}

	// Ranking: an exact title beats a phrase match beats scattered
	// tokens, and kudos orders within each band. Without the bands every
	// result is "as good as any other with the same kudos", which is
	// what an unordered-looking kudos list reads as no matter how right
	// it is.
	stmt += ` ORDER BY (CASE
			WHEN w.title = ? THEN 0
			WHEN w.title LIKE ? ESCAPE '\' OR w.authors LIKE ? ESCAPE '\' THEN 1
			ELSE 2 END),
		w.kudos DESC, w.id
		LIMIT ?`
	phrase := "%" + likeEscape(q) + "%"
	args = append(args, q, phrase, phrase, page.N)

	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, stmt, args...)
	if err != nil {
		page.WorkError = err.Error()
		return
	}
	defer rows.Close()
	for rows.Next() {
		h, err := scanWorkHit(rows)
		if err != nil {
			page.WorkError = err.Error()
			return
		}
		page.Works = append(page.Works, h)
	}
	if err := rows.Err(); err != nil {
		page.WorkError = err.Error()
		return
	}
	page.WorkTotal = len(page.Works)
	page.WorkLimited = len(page.Works) == page.N
}

// renderAuthor lists every work whose byline matches a name.
//
// This is the page the byline links point at: an author on this site is a
// person you can visit, not a string you retype into the search box. The
// match is a substring on the stored byline cell -- the mirror has no
// author table, and building an identity out of punctuation heuristics
// would invent relationships the corpus never asserted. What the page shows
// is exactly what it did: "authors matching X".
func (d Deps) renderAuthor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	title := "Authors"
	if q != "" {
		title = "Works by " + q
	}
	page := AuthorPage{Base: d.base(title, "Search"), Query: q}
	// Same contract as every list: ?n= picks the count, 20 by default so
	// the page is useful before any parameter is known to exist.
	page.N = clampInt(atoiDefault(r.URL.Query().Get("n"), 20), 1, 500)
	page.NextN = page.N * 2

	if q != "" {
		needle := "%" + likeEscape(q) + "%"
		const countStmt = `SELECT COUNT(*) FROM works WHERE authors LIKE ? ESCAPE '\'`
		if err := d.Engine.Corpus.DB.QueryRowContext(ctx, countStmt, needle).
			Scan(&page.Total); err != nil {
			// One data source, one failure mode: say it and stop. The
			// search page halves its errors because it has two halves to
			// save; this page has nothing left to show if the count is
			// gone.
			d.fail(w, r, http.StatusServiceUnavailable,
				fmt.Errorf("counting works matching %q: %w", q, err))
			return
		}
		const stmt = `SELECT ` + workHitCols + `
			FROM works w
			WHERE w.authors LIKE ? ESCAPE '\'
			ORDER BY w.kudos DESC, w.id
			LIMIT ?`
		rows, err := d.Engine.Corpus.DB.QueryContext(ctx, stmt, needle, page.N)
		if err != nil {
			d.fail(w, r, http.StatusServiceUnavailable,
				fmt.Errorf("reading works matching %q: %w", q, err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			h, err := scanWorkHit(rows)
			if err != nil {
				d.fail(w, r, http.StatusServiceUnavailable,
					fmt.Errorf("reading works matching %q: %w", q, err))
				return
			}
			page.Works = append(page.Works, h)
		}
		if err := rows.Err(); err != nil {
			d.fail(w, r, http.StatusServiceUnavailable,
				fmt.Errorf("reading works matching %q: %w", q, err))
			return
		}
		page.Limited = page.Total > int64(len(page.Works))
	}
	d.page(w, "author.html", page, http.StatusOK)
}

// queueAutoCrawl asks serve's background crawler for one work.
//
// Purely advisory: the page being rendered does not wait for it, does not
// depend on it, and must not fail because of it. The network request belongs
// to the worker goroutine and the crawl delay; all this does on the hot path
// is one INSERT. workURL empty means "build it from the id", which is the
// 404 case -- the mirror has no URL to offer for a work it does not have.
func (d Deps) queueAutoCrawl(ctx context.Context, workID int64, workURL, source string) {
	if d.Engine == nil || d.Engine.Store == nil {
		return
	}
	if workURL == "" {
		workURL = fmt.Sprintf("%s/works/%d", crawl.BaseURL, workID)
	}
	if err := d.Engine.Store.EnqueueCrawl(ctx, workURL, source); err != nil {
		d.log().Debug("auto-crawl enqueue failed", "work", workID, "source", source, "err", err)
	}
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
			// The reader's request IS the input: this mirror lacks the
			// work, so queue it for the background crawler. The page still
			// 404s -- growth happens off this path, at the crawl delay --
			// but the next reader of this URL finds it here.
			d.queueAutoCrawl(ctx, id, "", "reader-requested")
			d.notFound(w, r)
			return
		}
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	// A work the mirror holds but never finished storing: viewed metadata
	// is the other input. Same off-path rule -- enqueue and serve.
	if strings.TrimSpace(ent.Summary) == "" {
		d.queueAutoCrawl(ctx, id, ent.URL, "viewed-incomplete")
	}

	page := WorkPage{Base: d.base(ent.Title, ent.Title), Work: ent}

	// How many "more like this" the reader wants. Default 10 (the page's
	// historical size), ?n= raises it: the list was hard-capped at 10 with
	// no way to ask for more, which is exactly the limitation ?n= exists
	// to lift everywhere else on this site.
	page.N = clampInt(atoiDefault(r.URL.Query().Get("n"), 10), 1, 200)
	page.NextN = page.N * 2

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
		N:       page.N,
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

	// ?n= chooses how many works. 20 is the floor the spec promises; 200 is
	// the ceiling, because past that the page is a scroll and the engine's
	// blended half grows with it.
	n := clampInt(atoiDefault(r.URL.Query().Get("n"), 20), 1, 200)
	page := TagPage{Base: d.base(name, name), Tag: TagInfo{ID: id, Name: name}}
	page.N = n
	page.NextN = n * 2

	// Whether this reader has already blocked the tag, so the control offers
	// the reverse action. Best-effort: no store or no session means "not
	// blocked", because the page still has to render its works, and a missing
	// block list cannot be reported by a page that is trying to render.
	//
	// The full map is kept, not just this tag's bit: the taste blend below
	// drops works carrying ANY blocked tag from the recommender half, so a
	// block set on /block holds everywhere on this page instead of only in
	// the pools the engine happened to read.
	blockedAll := map[int64]bool{}
	if d.Engine != nil && d.Engine.Store != nil {
		if _, ownerKey, err := d.arenaSession(ctx, w, r); err == nil {
			if b, berr := d.Engine.Store.BlockedTagIDs(ctx, ownerKey); berr == nil {
				blockedAll = b
				page.Blocked = b[id]
			}
		}
	}

	// Sort and filter, from the query string.
	//
	// The order was hardcoded to kudos, which is defensible and useless:
	// the three questions a reader brings to a tag -- "what is popular",
	// "what is recent", "what can I read in one sitting" -- need three
	// different answers and got one. The values are whitelisted into a
	// constant rather than interpolated, because the alternative is string
	// concatenation into ORDER BY, which is SQL injection with a different
	// costume.
	//
	// "taste" is the default and the point: readers of this tag appreciated
	// these works even where the works do not carry the tag themselves, with
	// the exact matches boosted among them. The three exact orders stay as
	// choices because the blend answers a different question than "show me
	// this tag".
	page.Sort = tagSort(r.URL.Query().Get("sort"))
	page.SortOptions = []SortOption{
		{Value: "taste", Label: "taste match (default)"},
		{Value: "kudos", Label: "most kudos"},
		{Value: "recent", Label: "most recently updated"},
		{Value: "words", Label: "longest"},
	}
	page.RatingOptions = tagRatingOptions

	// complete / rating / lang, which the API has always honoured and the page
	// silently ignored. Verified against the live instance before this change:
	// `/api/v1/ao3/works?complete=true` applied the filter and reported it in
	// `filters_applied`, while `/tag/29?complete=true` returned the unfiltered
	// list with no comment. A reader who learned the filters from the API
	// documentation could not use them in a browser, and nothing said so.
	//
	// Each is parsed here rather than concatenated into SQL, so a bad value is
	// a stated reason instead of a silently absent filter -- the same rule the
	// length bound already follows.
	extra, extraArgs := tagFilters(r.URL.Query(), &page)

	// The length bound is parsed here rather than in SQL so a typo is a
	// stated reason and not a silently ignored parameter -- the same rule
	// the works list follows.
	wordsFilter := strings.TrimSpace(r.URL.Query().Get("words"))
	wordsClause := ""
	if wordsFilter != "" {
		bound, under, ok := parseWordBoundDir(wordsFilter)
		if ok {
			page.Words = wordsFilter
			// Strict on both sides. AO3's own filter treats the bound as
			// exclusive, and a reader asking for "under 20,000 words" who
			// gets a 20,000-word fic did not get what they asked for.
			//
			// word_count > 0 as well, because the mirror has 42 works of
			// 112,935 with a recorded count of exactly 0 -- AO3 published no
			// count for them. Those are not short fics; they are fics of
			// unknown length, and they satisfy a "under 5,000 words" filter.
			// Measured: 0.04% of the corpus, so this will never be visible
			// in a list, but it is wrong in the direction that matters --
			// the filter is there to protect a reader's time, and an
			// unknown-length fic protects nothing. No NULLs exist in the
			// column, but word_count IS NOT NULL is stated anyway so the
			// clause survives a mirror that does have them.
			if under {
				wordsClause = " AND w.word_count IS NOT NULL" +
					" AND w.word_count > 0 AND w.word_count < ?"
			} else {
				wordsClause = " AND w.word_count IS NOT NULL" +
					" AND w.word_count > 0 AND w.word_count > ?"
			}
			page.WordsBound = bound
		} else {
			page.WordsErr = fmt.Sprintf(
				"%q is not a word-count bound; use under:10000 or over:50000",
				wordsFilter)
		}
	}

	// The heading needs the real total, which is not the length of the
	// page. Conflating them is how a page ends up claiming a tag with
	// 3,000 works has twenty.
	//
	// The total counts the FILTERED set, not every work on the tag. The
	// first version counted all of them, so "3,000 works" sat above a list
	// of 20 short ones filtered to under 5,000 words -- a heading that
	// describes a set the page does not contain.
	// ONE clause string and ONE arg list, shared by the count and the list.
	// They were separate before the length filter and keeping them in step by
	// hand is how "of 55,808" ends up above a filtered list of twenty.
	// COUNT(DISTINCT) for the same reason as the list's DISTINCT: a work
	// attached to this tag under two types is one work, not two. Using COUNT(*)
	// here made the heading claim 29 works over a list of 28 distinct ones.
	totalQuery := `SELECT COUNT(DISTINCT wt.work_id) FROM work_tags wt
	               JOIN works w ON w.id = wt.work_id
	               WHERE wt.tag_id = ?` + extra
	totalArgs := append([]any{id}, extraArgs...)
	if wordsClause != "" {
		totalQuery += wordsClause
		totalArgs = append(totalArgs, page.WordsBound)
	}
	var total int64
	if err := d.Engine.Corpus.DB.QueryRowContext(ctx,
		totalQuery, totalArgs...).Scan(&total); err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	// The unfiltered size, for the empty-result message only. Skipped when the
	// filtered count is already the unfiltered one (no filters set), since the
	// two are then the same query.
	var unfilteredTotal int64
	if extra != "" || wordsClause != "" {
		// COUNT(DISTINCT work_id), for the same reason as the filtered count:
		// work_tags is keyed on (work_id, tag_id, tag_type), so a tag name
		// attached under two types is one work with two rows. COUNT(*) here
		// said the tag had 29 works while the list below showed 28 distinct
		// ones -- which is the same class of contradiction as the "0 works"
		// message, one layer further out.
		if err := d.Engine.Corpus.DB.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT work_id) FROM work_tags WHERE tag_id = ?`, id).
			Scan(&unfilteredTotal); err != nil {
			d.fail(w, r, http.StatusInternalServerError, err)
			return
		}
	}
	// Tag.WorkCount is the FILTERED count, because the heading above the list
	// has to describe the set the page actually contains -- "3,000 works" over a
	// filtered list of twenty is a heading that lies.
	page.Tag.WorkCount = total
	page.Total = int(total)

	// UnfilteredTagCount is the tag's real size, for the one message that needs
	// it: "No work on this tag matches ... The tag has N works in total, so the
	// filters are what excluded them."
	//
	// Reusing Tag.WorkCount there said "The tag has 0 works in total" the
	// moment any filter matched nothing, which tells the reader their filter
	// emptied the tag rather than that the filter excluded everything. Caught
	// by a Playwright assertion on the message text, not by a Go test -- the Go
	// test for this branch asserted the filters were NAMED and never checked
	// the number beside them.
	page.UnfilteredTagCount = int(unfilteredTotal)
	page.Limited = total > int64(n)

	// authors, url, summary and language are all NULLable in the corpus.
	// Scanning them into *string fails the whole page on a row that is
	// perfectly ordinary, which is what happened the first time: one
	// work with no author recorded took out every tag page that listed it.
	// DISTINCT, and this is load-bearing.
	//
	// work_tags is keyed on (work_id, tag_id, tag_type), so the SAME tag name
	// attached to a work under two types -- "dark" as both a fandom and a
	// freeform, which the fixture deliberately produces -- yields two rows for
	// one work. Without DISTINCT the page lists work 1 twice, the count says 29
	// for 28 distinct works, and `complete=true` and `complete=false` union to
	// 28 rather than 29.
	//
	// Found by a Playwright assertion that the two completion states partition
	// the tag. It failed by exactly one, which is what a duplicated join row
	// looks like from the outside.
	listQuery := `
		SELECT DISTINCT w.id, w.title, w.authors, w.url, w.summary,
		       w.kudos, w.hits, w.word_count, w.language, w.complete
		FROM work_tags wt JOIN works w ON w.id = wt.work_id
		WHERE wt.tag_id = ?` + extra + wordsClause + `
		ORDER BY ` + tagOrder(page.Sort) + `, w.id LIMIT ?`
	listArgs := append([]any{id}, extraArgs...)
	if wordsClause != "" {
		listArgs = append(listArgs, page.WordsBound)
	}
	listArgs = append(listArgs, n)
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, listQuery, listArgs...)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		h, err := scanWorkHit(rows)
		if err != nil {
			d.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		page.Works = append(page.Works, h)
	}
	if err := rows.Err(); err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	// The taste blend. sort=taste (the default) merges the works carrying
	// this tag -- boosted -- with works readers of this tag appreciated
	// even when they carry no such tag. Any explicit sort skips it and
	// lists the exact matches alone, which is what those choices mean.
	if page.Sort == tasteSort && d.Engine != nil {
		d.blendTagTaste(ctx, id, n, extra, extraArgs, wordsClause, blockedAll, &page)
	}

	if d.Engine.Graph != nil {
		page.Neighbours = d.neighbours(ctx, id)
	}
	d.page(w, "tag.html", page, http.StatusOK)
}

// tasteSort is the blended default; see tagSort.
const tasteSort = "taste"

// blendTagTaste merges the tag's exact matches with what readers of the tag
// appreciated elsewhere, and explains itself in page.TasteNote.
//
// The recipe, in full, because an ordering nobody can audit is the one thing
// this site promises not to ship:
//
//  1. Seeds: the tag's five most-kudoed works, filtered the same way as the
//     visible list, so a work the reader filtered or blocked out never
//     speaks for the tag.
//  2. Candidates: the recommender's reader-overlap neighbours of those
//     seeds, minus anything carrying the tag (those arrive via the exact
//     half), minus blocked tags, then run through the SAME SQL filters as
//     the exact half -- the filters are the reader's, they apply to every
//     half of the list.
//  3. Scores: an untagged candidate keeps its similarity; a tagged work
//     gets its similarity PLUS a boost equal to the mean similarity of the
//     surviving pool. Mean rather than max so a niche exact match floats
//     above weak suggestions without welding every exact match to the top,
//     where the list would just be the old exact list with a new heading.
//  4. Merge, order by score (tagged wins ties, then kudos), cut to n.
//
// Failure keeps the exact list: a recommender that could not recommend must
// not take the works the reader explicitly asked about with it.
func (d Deps) blendTagTaste(
	ctx context.Context,
	id int64,
	n int,
	extra string,
	extraArgs []any,
	wordsClause string,
	blockedAll map[int64]bool,
	page *TagPage,
) {
	// 1. Seeds.
	seedQuery := `SELECT DISTINCT w.id FROM work_tags wt
		JOIN works w ON w.id = wt.work_id
		WHERE wt.tag_id = ?` + extra + wordsClause + `
		ORDER BY w.kudos DESC, w.id LIMIT 5`
	seedArgs := append([]any{id}, extraArgs...)
	if wordsClause != "" {
		seedArgs = append(seedArgs, page.WordsBound)
	}
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, seedQuery, seedArgs...)
	if err != nil {
		page.TasteErr = "seed lookup failed (" + err.Error() + "); showing the works carrying this tag"
		return
	}
	var seeds []engine.Seed
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			page.TasteErr = "seed lookup failed (" + err.Error() + "); showing the works carrying this tag"
			return
		}
		seeds = append(seeds, engine.Seed{Kind: corpus.AO3Kind, ID: sid})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		page.TasteErr = "seed lookup failed (" + err.Error() + "); showing the works carrying this tag"
		return
	}
	if len(seeds) == 0 {
		// A tag whose exact list is empty has nothing to blend; the
		// empty-result message below it already says why.
		return
	}

	// 2. Candidates. Over-fetch: the filter pass and the tagged-exclusion
	// remove some, and a pool that trims to fewer than n on a big tag
	// would undercut the list it exists to fill.
	cfN := clampInt(n*3, 30, 150)
	res, err := d.Engine.Recommend(ctx, engine.Request{
		Seeds:         seeds,
		Kind:          corpus.AO3Kind,
		N:             cfN,
		Exclude:       true,
		BlockedTagIDs: blockedAll,
	})
	if err != nil {
		page.TasteErr = "the taste blend failed (" + err.Error() + "); showing the works carrying this tag"
		return
	}
	if len(res.Items) == 0 {
		page.TasteErr = "no reader-signal for this tag yet; showing the works carrying this tag"
		return
	}

	// Score map from the recommender's own ordering.
	score := make(map[int64]float64, len(res.Items))
	ids := make([]any, 0, len(res.Items))
	seen := make(map[int64]bool, len(res.Items))
	placeholders := make([]string, 0, len(res.Items))
	for _, it := range res.Items {
		score[it.ID] = it.Score
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		placeholders = append(placeholders, "?")
		ids = append(ids, it.ID)
	}
	if len(ids) == 0 {
		page.TasteErr = "no reader-signal for this tag yet; showing the works carrying this tag"
		return
	}

	// The same filters as the exact half, plus the exclusion of everything
	// already listed there.
	cfQuery := `SELECT DISTINCT ` + workHitCols + ` FROM works w
		WHERE w.id IN (` + strings.Join(placeholders, ",") + `)
		AND NOT EXISTS (SELECT 1 FROM work_tags wt2
			WHERE wt2.work_id = w.id AND wt2.tag_id = ?)` +
		extra + wordsClause
	cfArgs := append(append([]any{}, ids...), id)
	cfArgs = append(cfArgs, extraArgs...)
	if wordsClause != "" {
		cfArgs = append(cfArgs, page.WordsBound)
	}
	cfRows, err := d.Engine.Corpus.DB.QueryContext(ctx, cfQuery, cfArgs...)
	if err != nil {
		page.TasteErr = "the taste blend failed (" + err.Error() + "); showing the works carrying this tag"
		return
	}
	var pool []WorkHit
	for cfRows.Next() {
		h, err := scanWorkHit(cfRows)
		if err != nil {
			cfRows.Close()
			page.TasteErr = "the taste blend failed (" + err.Error() + "); showing the works carrying this tag"
			return
		}
		pool = append(pool, h)
	}
	cfRows.Close()
	if err := cfRows.Err(); err != nil {
		page.TasteErr = "the taste blend failed (" + err.Error() + "); showing the works carrying this tag"
		return
	}
	if len(pool) == 0 {
		page.TasteErr = "no untagged neighbours of this tag survived the filters; showing the works carrying this tag"
		return
	}

	// 3. Boost. Mean over the surviving pool only: the boost should describe
	// the works actually entering the list, not candidates the filters
	// threw away.
	var sum float64
	for _, h := range pool {
		sum += score[h.ID]
	}
	boost := sum / float64(len(pool))
	if boost == 0 {
		// Non-zero similarity everywhere or nowhere; with all-zero scores
		// the merge would degrade to "exact matches first, then noise
		// ordered by nothing", which is the old page with extra steps.
		page.TasteErr = "the recommender returned no usable similarity for this tag; showing the works carrying this tag"
		return
	}

	for i := range page.Works {
		page.Works[i].HasTag = true
		page.Works[i].ShowScore = true
		page.Works[i].Score = score[page.Works[i].ID] + boost
	}
	for _, h := range pool {
		h.ShowScore = true
		h.Score = score[h.ID]
		page.Works = append(page.Works, h)
	}

	// 4. Merge and order. Stable, so equal scores keep kudos order instead
	// of shuffling, and the tagged half keeps its position within a tie.
	sort.SliceStable(page.Works, func(i, j int) bool {
		a, b := page.Works[i], page.Works[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.HasTag != b.HasTag {
			return a.HasTag // an exact match beats an untagged work at equal score
		}
		return a.Kudos > b.Kudos
	})
	if len(page.Works) > n {
		page.Works = page.Works[:n]
	}
	page.Blend = true
	taggedShown := taggedCount(page)
	page.TasteNote = fmt.Sprintf(
		"Taste match: %d works readers of this tag appreciated (score shown), "+
			"blended with %d works carrying this tag, each boosted by the mean similarity %.3f.",
		len(page.Works)-taggedShown, taggedShown, boost)
}

// taggedCount counts the rows that carry the page's tag, for the taste note.
func taggedCount(page *TagPage) int {
	n := 0
	for _, w := range page.Works {
		if w.HasTag {
			n++
		}
	}
	return n
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

	// Tune selection. The page used to DISPLAY the active weights and never
	// let a reader choose one, so `?tune=` was unreachable from the browser
	// and `kindred tune` was the only way to use a stored tune at all — a
	// capability that exists, works, and cannot be found. An unknown name is
	// refused by loadTune inside the engine rather than silently ignored,
	// which matters more here than in the API: a typo'd tune in a form
	// should say so, not quietly rank by the default.
	tuneName := strings.TrimSpace(q.Get("tune"))

	// Pool mode: whether co-bookmarked works may enter the pool at all.
	poolMode, err := engine.ParsePoolMode(q.Get("pool_mode"))
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, err)
		return
	}

	// Pool size, bounded like the API's ?pool= so a reader cannot ask for an
	// unbounded ranking from a form field.
	poolSize := 0
	if v := q.Get("pool"); v != "" {
		poolSize, err = parseBoundedInt(v, 0, 5000, 0)
		if err != nil {
			d.fail(w, r, http.StatusBadRequest,
				fmt.Errorf("pool: %w (candidates ranked per request, 0-5000)", err))
			return
		}
	}

	// Filters. Parsed by the SAME function the API uses, so the page and the
	// JSON API cannot drift on what a filter means — and so a value the
	// parser does not recognise is a 400 naming it rather than a silently
	// ignored control.
	filter, err := engine.ParseFilter(q)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, err)
		return
	}

	// The reader's explicitly blocked tags. Resolved from their arena
	// session, so blocking a tag on /block changes what /recommend returns
	// — which is the whole point of having a block list. Before this, the
	// block list was stored, displayed, and reversible, and never consulted
	// during ranking: a reader could block a tag, see the same
	// recommendations, and conclude the feature was broken.
	//
	// A session is minted on demand rather than requiring one, because
	// /recommend is reachable without ever visiting /block and forcing a
	// cookie there would be a side effect of merely ranking. A store failure
	// is not fatal: the page still ranks, and says so in the degraded list,
	// because refusing to recommend because a block list could not be read
	// would be a worse failure than recommending something blocked.
	var blocked map[int64]bool
	if d.Engine.Store != nil {
		_, ownerKey, serr := d.arenaSession(ctx, w, r)
		if serr != nil {
			d.log().Warn("could not resolve the reader's block list; ranking without it",
				"err", serr)
		} else if b, berr := d.Engine.Store.BlockedTagIDs(ctx, ownerKey); berr != nil {
			d.log().Warn("could not read blocked tags; ranking without them", "err", berr)
		} else if len(b) > 0 {
			blocked = b
		}
	}

	// Works this reader has already been shown or has read.
	//
	// Two separate decisions, because they are two separate complaints.
	// `hide_seen` is about the RECOMMENDER repeating itself and expires: a
	// work unseen for a month is worth showing again. Marked-read works are
	// excluded unconditionally, because the reader said they read it and that
	// does not expire.
	seen, hideSeen := d.seenExclusions(ctx, w, r, q)

	req := engine.Request{
		Seeds:   seeds,
		Kind:    corpus.AO3Kind,
		N:       n,
		Tune:    tuneName,
		Exclude: true,
		// The cap and its axis are passed through rather than applied to the
		// result list here. Post-filtering a top-N list is a different and much
		// worse algorithm: if the top 20 are all Harry Potter and the cap is 3,
		// filtering the 20 returns 3 when the corpus had 200 eligible works
		// outside that fandom. The cap has to shape the query.
		MaxPerGroup:   capN,
		GroupBy:       groupBy,
		Filter:        filter,
		PoolMode:      poolMode,
		PoolSize:      poolSize,
		BlockedTagIDs: blocked,
		SeenIDs:       seen,
	}

	res, err := d.Engine.Recommend(ctx, req)
	if err != nil {
		// A seed kind this mirror does not carry is the reader's mistake, and
		// gets a 400 naming the kind rather than a 500: "ao3_book:1" is a typo,
		// not a server fault.
		if errors.Is(err, engine.ErrUnsupportedKind) ||
			errors.Is(err, engine.ErrUnknownTune) {
			// Both are the reader naming something that does not exist, and
			// both errors carry the correction. A 500 for a typo'd tune
			// would report a server fault for a client mistake.
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
		Filter:          filter,
		PoolMode:        poolMode.String(),
		PoolSize:        poolSize,
		TuneName:        tuneName,
		TuneNames:       d.tuneNames(ctx),
		Filters:         recommendFilterRows(q, filter),
		Degraded:        res.Meta.Degraded,
	}
	// Record what was shown, AFTER the ranking. Doing it before would mean
	// the page's own results are on the exclusion list by the time the reader
	// refreshes, so a refresh returns an empty page — the feature working so
	// well it hides everything.
	//
	// Only recorded when the reader ASKED for seen-exclusion. Otherwise every
	// visit silently builds a history the reader never opted into, and then
	// `hide_seen` starts suppressing works they have never actually seen.
	d.recordShown(ctx, w, r, res.Items, hideSeen)

	page.Tune = weightRows(res.Tune)
	page.HideSeen = hideSeen
	// Only the path and query, and only if they came from this site: the mark
	// read form POSTs this back, and an attacker-supplied absolute URL here
	// would make "mark read" a redirect off-site. safeReturnTo re-checks it,
	// but building it from the request rather than from a constant is what
	// keeps the two in step.
	page.ReturnTo = safeReturnTo(r.URL.RequestURI())
	d.page(w, "recommend.html", page, http.StatusOK)
}

// seenExclusions resolves which works this reader should not be shown again.
//
// It returns the exclusion set and whether seen-tracking is on. The boolean is
// passed back to the caller rather than inferred from the set's size,
// because "the reader has seen nothing" and "the reader is not tracking" both
// produce an empty set and must not be confused: the second one must not
// start recording.
//
// Read works are always excluded. Shown-but-not-read works are excluded only
// while the reader has asked for it, and only inside the window — see
// store.SeenIDs for why the window exists.
func (d Deps) seenExclusions(ctx context.Context, w http.ResponseWriter, r *http.Request, q url.Values) (map[string]bool, bool) {
	if d.Engine == nil || d.Engine.Store == nil {
		return nil, false
	}
	hideSeen := q.Get("hide_seen") == "1" || q.Get("hide_seen") == "true"
	_, ownerKey, err := d.arenaSession(ctx, w, r)
	if err != nil {
		return nil, false
	}
	// A zero window excludes only the read set. That is the right default
	// for someone who has never ticked the box: "do not re-recommend what I
	// have read" is a reasonable thing to honour, and "do not show me
	// anything twice" is not.
	window := time.Duration(0)
	if hideSeen {
		window = defaultSeenWindow
	}
	seen, err := d.Engine.Store.SeenIDs(ctx, ownerKey, window)
	if err != nil {
		d.log().Warn("could not read the reader's seen list; ranking without it",
			"err", err)
		return nil, false
	}
	return seen, hideSeen
}

// defaultSeenWindow is how long a merely-shown work stays suppressed.
//
// Seven days is a guess, and it is labelled as one. It is long enough that a
// reader working through a list is not offered the same works the next day,
// and short enough that a work they skipped on Monday is back by the
// following week. It is a parameter rather than a constant so a reader
// wanting a different tolerance can have one.
const defaultSeenWindow = 7 * 24 * time.Hour

// recordShown notes the works this response listed.
//
// It is deliberately best-effort and deliberately silent: a reader cannot
// act on "I could not save that you saw this", and failing the whole page
// because a history row failed to write would be a much worse outcome than a
// slightly incomplete history.
func (d Deps) recordShown(ctx context.Context, w http.ResponseWriter, r *http.Request, items []rank.Candidate, tracking bool) {
	if !tracking || d.Engine == nil || d.Engine.Store == nil || len(items) == 0 {
		return
	}
	_, ownerKey, err := d.arenaSession(ctx, w, r)
	if err != nil {
		return
	}
	ids := make([]string, 0, len(items))
	for _, c := range items {
		ids = append(ids, corpus.AO3Kind+":"+strconv.FormatInt(c.ID, 10))
	}
	if err := d.Engine.Store.MarkShown(ctx, ownerKey, ids); err != nil {
		d.log().Warn("could not record shown works", "err", err)
	}
}

// maxSeeds is the seed ceiling. Five describes a taste; twenty averages it
// into mush, which is a worse answer than refusing to answer.
const maxSeeds = 5

// recommendFilterRows builds the ranking filter controls, echoing the values
// the reader sent.
//
// The values come back out of the RAW QUERY rather than out of the parsed
// Filter, and that is deliberate: a number box holding "abc" must still show
// "abc" so the reader can see what they typed. Reconstructing the value from
// the parsed filter would silently blank it, which reads as the control
// having rejected the input without saying why.
func recommendFilterRows(q url.Values, f engine.Filter) []FilterRow {
	csv := func(vs []string) string { return strings.Join(vs, ", ") }
	sel := func(cur string, opts ...Option) []Option {
		for i := range opts {
			opts[i].Selected = opts[i].Value == cur
		}
		return opts
	}
	// The raw `complete` value is echoed when it parses, and the first legal
	// option is selected when it does not.
	complete := strings.ToLower(strings.TrimSpace(q.Get("complete")))
	switch {
	case complete == "":
	case complete == "any", complete == "all":
		complete = "any"
	case complete == "complete", complete == "only", complete == "completed",
		complete == "true", complete == "1", complete == "yes":
		complete = "complete"
	default:
		complete = "in-progress"
	}

	return []FilterRow{
		{
			Name: "min_words", Label: "At least", Kind: "number",
			Value: q.Get("min_words"),
			Hint:  "words; 0 for no floor",
		},
		{
			Name: "max_words", Label: "At most", Kind: "number",
			Value: q.Get("max_words"),
			Hint:  "words; 0 for no ceiling",
		},
		{
			Name: "min_kudos", Label: "At least", Kind: "number",
			Value: q.Get("min_kudos"),
			Hint:  "kudos",
		},
		{
			Name: "complete", Label: "Completion", Kind: "select",
			Value: complete,
			Values: sel(complete,
				Option{Value: "any", Label: "any"},
				Option{Value: "complete", Label: "finished only"},
				Option{Value: "in-progress", Label: "in progress only"}),
		},
		{
			Name: "rating", Label: "Rating", Kind: "csv",
			Value: csv(f.Ratings),
			Hint:  "comma separated, e.g. General Audiences, Explicit",
		},
		{
			Name: "lang", Label: "Language", Kind: "csv",
			Value: csv(f.Languages),
			Hint:  "comma separated, e.g. English",
		},
	}
}

// tuneNames lists the stored tunes a reader can pick.
//
// Best-effort by design: a reader with no store, or a store that cannot be
// read, gets a select with only the "default" option rather than an error
// page. The page still ranks. A tune picker that can break the recommend page
// is a worse trade than a tune picker that occasionally offers nothing.
func (d Deps) tuneNames(ctx context.Context) []string {
	out := []string{"default"}
	if d.Engine == nil || d.Engine.Store == nil {
		return out
	}
	names, err := d.Engine.Store.TuneNames(ctx)
	if err != nil || len(names) == 0 {
		return out
	}
	for _, n := range names {
		if n != "" && n != "default" {
			out = append(out, n)
		}
	}
	return out
}

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
		elapsed := time.Since(t)
		b.AgeSeconds = int64(elapsed.Seconds())
		b.IndexAgeDays = int(elapsed.Hours() / 24)
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

// tagRatingOptions is the rating control's choices.
//
// The VALUES are what the reader types and what AO3 uses -- single letters --
// and the LABELS are the names the mirror actually stores. Sending the letter
// is deliberate: `ao3RatingNames` in internal/api translates it, and that
// mapping is measured against the real corpus rather than guessed.
//
// The full names are also accepted on input (`?rating=General%20Audiences`),
// because a client that reads `rating` out of the API and sends it back must
// get the same works. Both spellings are what readers type, and a control
// that only accepts one of them is a control that fails silently.
var tagRatingOptions = []SortOption{
	{Value: "G", Label: "General Audiences"},
	{Value: "T", Label: "Teen And Up Audiences"},
	{Value: "M", Label: "Mature"},
	{Value: "E", Label: "Explicit"},
}

// tagFilters parses complete / rating / lang into a SQL clause and its
// arguments, and echoes the accepted values back onto the page so the controls
// can render the current selection.
//
// Returns ("", nil) when none is set, which is the common case and must not
// add a clause -- an empty predicate would change the query plan for every
// unfiltered request.
//
// Every value is bound as a parameter. None of these three is a string
// interpolated into SQL, and the one that could be (rating) is a fixed set of
// clauses built from a hardcoded comparison, never from the input.
func tagFilters(q url.Values, page *TagPage) (string, []any) {
	var clause strings.Builder
	var args []any

	// complete. Only "true" and "false" are accepted; anything else is an
	// error rather than a no-op, because "complete=1" or "complete=yes" read
	// as a filter that did nothing.
	switch v := strings.ToLower(strings.TrimSpace(q.Get("complete"))); v {
	case "":
	case "true", "1", "yes", "only":
		page.Complete = "true"
		clause.WriteString(" AND w.complete = 1")
	case "false", "0", "no":
		page.Complete = "false"
		clause.WriteString(" AND w.complete = 0")
	default:
		page.FilterErr = fmt.Sprintf(
			"%q is not a completion filter; use complete=true or complete=false", v)
		return "", nil
	}

	if v := strings.TrimSpace(q.Get("rating")); v != "" {
		// Upper-cased and matched against the mirror's own spelling. The
		// letters are translated by the API's own table, so this page and
		// `/api/v1/ao3/works` cannot disagree about what "E" means.
		//
		// `UPPER(w.rating) = ?` rather than a LIKE: the column is a short
		// controlled vocabulary, and a full scan of 112,935 rows with a
		// function on the column is not worth it for a page that is usually
		// read without this filter set.
		page.Rating = v
		clause.WriteString(" AND UPPER(w.rating) = ?")
		args = append(args, strings.ToUpper(ratingToStored(v)))
	}

	if v := strings.TrimSpace(q.Get("lang")); v != "" {
		page.Lang = v
		clause.WriteString(" AND UPPER(w.language) = ?")
		args = append(args, strings.ToUpper(v))
	}

	return clause.String(), args
}

// ratingToStored maps an AO3 rating letter to the name the mirror stores, or
// returns the value unchanged when it is already a full name.
//
// Measured on the real mirror, all 112,935 rows included: Explicit 42,968,
// Teen And Up Audiences 27,562, Mature 25,190, Not Rated 8,796, General
// Audiences 8,419. The letters alone match nothing, which is why `rating=G`
// returned zero works until this mapping existed.
//
// "Not Rated" is deliberately absent from the control: it is a real value in
// the column and `?rating=Not+Rated` works, but it is not a rating a reader
// picks from a list of content ratings, so offering it would be a category
// error.
func ratingToStored(v string) string {
	// ONLY the four AO3 letters.
	//
	// The first version also accepted P, S, D and Z, which I took from other
	// systems' age ratings. That is wrong and not merely incomplete: those
	// letters mean different things elsewhere, so a reader asking for them
	// would have been silently served works they did not ask for.
	//
	//	 P  UK 12+/15+ and PEGI 12        -> not "General"
	//	 S  Spain/Portugal 12+             -> not "Teen"
	//	 D  Germany 16+, or US TV-MA       -> not "Mature"
	//	 Z  Explicit in some storefronts   -> not "Mature"
	//
	// `?rating=Z` returned two Mature works instead of nothing, which is the
	// worst failure mode a filter can have: it answers the wrong question
	// confidently. A letter this project does not recognise now matches
	// nothing, which is the honest answer.
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "G":
		return "General Audiences"
	case "T":
		return "Teen And Up Audiences"
	case "M":
		return "Mature"
	case "E":
		return "Explicit"
	default:
		// Already a full name, or something the mirror does not contain --
		// which then matches nothing, honestly, instead of being coerced.
		return v
	}
}

// tagSort is the tag page's sort order, as a closed set.
//
// A string, not an ORDER BY fragment, because the value comes from a query
// parameter: interpolating one is SQL injection with a different costume.
// An unrecognised value falls back to kudos rather than erroring, and the
// page renders the control showing kudos selected -- so the reader can see
// what they are getting instead of guessing why the order is not what they
// asked for.
func tagSort(s string) string {
	switch s {
	case "date", "recent":
		return "recent"
	case "words", "length":
		return "words"
	case "kudos", "popular":
		return "kudos"
	case "taste":
		return "taste"
	case "":
		// The default is the taste blend: works readers of this tag
		// appreciated, with the works carrying the tag boosted among
		// them. A reader who wants the plain exact list picks a sort.
		return "taste"
	default:
		return "kudos"
	}
}

// tagOrder maps a tagSort value to its ORDER BY expression.
//
// kudos is NULL for some rows in the real corpus, so every arm ends with
// `w.id` as a tiebreak: without it, rows equal on the sort key come back in
// whatever order SQLite happens to produce, and the same page renders
// differently on two requests. That instability was measured on the tag
// page's neighbours list before it was fixed there.
func tagOrder(sortKey string) string {
	switch sortKey {
	case "recent":
		// update_date is TEXT and nullable, so a NULL date sorts last with
		// an explicit guard rather than first by accident.
		return "CASE WHEN w.update_date IS NULL OR w.update_date = '' THEN 1 ELSE 0 END, w.update_date DESC"
	case "words":
		return "w.word_count DESC, w.id"
	default:
		// "taste" and everything unrecognised order the TAGGED half by
		// kudos. The blend's final order is computed in Go from the
		// similarity scores; this is only the input ordering.
		return "w.kudos DESC, w.id"
	}
}

// parseWordBoundUI reads a word-count bound in the AO3 spelling the works
// list uses: under:N, over:N, and the <N / >N forms.
//
// Returns ok=false for anything else so the caller can SAY the value was not
// understood, rather than dropping it -- the same rule as the API filter,
// and for the same reason: a silently ignored control looks like a working
// one.
func parseWordBoundUI(s string) (int, bool) {
	n, _, ok := parseWordBoundDir(s)
	return n, ok
}

// parseWordBoundDir is parseWordBoundUI plus the DIRECTION.
//
// The direction is part of the parse rather than something the caller
// re-derives. It was: the handler checked `strings.HasPrefix(s, "under")`
// to pick the SQL comparison, which is correct for "under:2000" and
// INVERTED for "<2000" -- the same bound, the opposite filter. Caught by
// the test arm `?words=<2000 listed [3], want [1]`: it returned the single
// work that is NOT under 2,000 words.
//
// Returning the direction means the two spellings cannot disagree about
// what they mean, and it puts the accepted forms in one place.
func parseWordBoundDir(s string) (n int, under bool, ok bool) {
	raw := strings.TrimSpace(s)
	lower := strings.ToLower(raw)
	var num string
	switch {
	case strings.HasPrefix(lower, "under:"):
		num, under = raw[len("under:"):], true
	case strings.HasPrefix(lower, "over:"):
		num = raw[len("over:"):]
	case strings.HasPrefix(raw, "<"):
		num, under = raw[1:], true
	case strings.HasPrefix(raw, ">"):
		num = raw[1:]
	default:
		return 0, false, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(num))
	if err != nil || v < 0 {
		return 0, false, false
	}
	return v, under, true
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
