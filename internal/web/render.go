package web

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/profile"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// Base is embedded in every page. It is how the shared header, footer and
// <head> reach every template without a second parse or a second pass.
type Base struct {
	Title       string
	Heading     string
	Version     string
	Mode        string
	CorpusWorks int
	// Query is on Base, not on SearchPage, because the layout prefills the
	// search box on every page -- and a page struct that lacked it would
	// fail at execution with "can't evaluate field Query", which reads as
	// a template bug rather than a struct one.
	Query string

	// IndexBuiltAt is when the co-occurrence index was built, RFC3339, or
	// empty if unknown.
	//
	// The ingest command has always written this into the store's meta table
	// and nothing has ever read it, so the mirror's age -- the single fact
	// that most changes what a recommendation is worth -- was invisible to
	// every reader. A stale index does not error; it answers confidently
	// from last month's data.
	IndexBuiltAt string
	// CorpusBuiltAt is the mirror's own timestamp, which is a DIFFERENT fact
	// from the index build: an index built today from a mirror written in
	// March is three months out of date however fresh the index is.
	CorpusBuiltAt string
	// IndexAgeDays is how stale the data is, in whole days. It is the
	// footer prose's precision: "built 3 days ago".
	//
	// Separate from the timestamps so the template does no date arithmetic.
	// Negative when the recorded time is in the future, which is a clock
	// problem rather than a data problem and is rendered as such.
	IndexAgeDays int
	// AgeSeconds is the same age in seconds, for the response headers.
	//
	// It exists because the day count is too coarse for a header: a mirror
	// ingested five minutes ago has IndexAgeDays == 0, and a header derived
	// from it reads "0s", claiming the index was rebuilt this second. That
	// was not a theoretical worry -- it is what the live header said, five
	// minutes after a 5m26s ingest.
	AgeSeconds int64
	// AgeKnown is false when no build time is recorded at all, which is a
	// THIRD state distinct from "built now" and "built long ago". A page
	// must not render "0 days old" for a mirror of unknown vintage -- that
	// is a fresh-looking claim made from no data.
	AgeKnown bool
	// IndexAgeAbsDays is |IndexAgeDays|, so a build time in the future can
	// be rendered as a magnitude rather than as a negative number of days.
	//
	// The alternative is a `negate` template helper used in exactly one
	// place, which would be a function on the template namespace forever to
	// avoid one subtraction in Go.
	IndexAgeAbsDays int
	// AgeInFuture is true when the recorded build time is later than now --
	// a clock problem, not a data problem, and worth naming as one.
	AgeInFuture bool
}

// SearchPage is one query's results, in two lists. It is also the home page:
// there is no separate one, because a recommender with nothing to seed from
// has nothing to say, so the search box IS the home page.
//
// Tags and works are separate rather than merged. A tag match and a title
// match are not comparable scores, so a single ranked list would be ordered
// arbitrarily while claiming to be the best matches. Two honest lists, each
// labelled, beat one dishonest ordering.
//
// Works exist here because the page used to search tags ONLY: a work in the
// mirror could not be found by its title, and a work the reader can name is
// the one they most want to seed a recommendation from.
type SearchPage struct {
	Base
	Results []TagHit
	Total   int
	Limited bool

	// Works, matching title OR author.
	Works []WorkHit
	// WorkTotal is the number of works RETURNED, which is not the number
	// that exist. With a LIMIT of 50 a query matching 900 works says 50.
	WorkTotal   int
	WorkLimited bool

	// TagError and WorkError are set when one half of the search failed.
	// They are separate so a failing tag search does not hide working work
	// results, and neither failure is rendered as an empty result: "your
	// query found nothing" and "the search failed" are different facts and a
	// reader acting on the first would be wrong about the second.
	TagError  string
	WorkError string

	// N is the work-list limit this request asked for (?n=), and NextN is
	// N doubled -- the "show more" target. They live here because the
	// truncation message has to LINK to a bigger list, and a link built
	// from a hardcoded number would stop being true the moment the reader
	// changed the limit.
	N     int
	NextN int

	// Exact is a work found by AO3 ID or work URL parsed out of the query,
	// rendered above the fuzzy results. A reader pasting
	// "archiveofourown.org/works/12345" wants THAT work, not fifty title
	// matches, and burying it under them is a search that answered a
	// different question.
	Exact *WorkHit

	// ExactMissing is the work id a URL-shaped query named that this
	// mirror does not hold. Zero when the query named no work. Set only
	// for URL-shaped queries: a bare number that matches nothing is a
	// search term, not a broken link, and claiming otherwise would be a
	// confident answer to a question that was never asked.
	ExactMissing int64
}

// TagHit is one row of a tag search.
type TagHit struct {
	ID   int64
	Name string
}

// WorkPage is a single work, with the tags it carries and what the
// recommender makes of it.
type WorkPage struct {
	Base
	Work    corpus.Entity
	Tags    []corpus.TagPair
	Similar []rank.Candidate
	// RecommendErr is set when the recommendation half failed while the
	// work half succeeded. Rendering the work without the recommendations
	// is strictly better than a 500, but it must be visible: a page that
	// silently drops half its content is a lie about what the recommender
	// can do.
	RecommendErr string

	// N is the recommendation count this page asked for (?n=, default 10)
	// and NextN is N doubled, for the "more like this" raise link.
	N     int
	NextN int
}

// TagPage is a tag, the works carrying it, and the tags that tend to
// travel with it.
type TagPage struct {
	Base
	Tag     TagInfo
	Works   []WorkHit
	Total   int
	Limited bool
	// Neighbours are graph.ScoredNeighbour, which carries ids and scores
	// but NOT names: the CSR drops its name table after building, to give
	// 25 MiB back. The names are fetched from the corpus here, which is
	// the only place they still exist at request time.
	Neighbours []Neighbour

	// Sort is the order the works are listed in, as one of "kudos",
	// "recent", "words". It is on the page so the control can render the
	// CURRENT selection -- a reader who asked for "recent" and got kudos
	// back can otherwise only guess why.
	Sort string
	// SortOptions drives the control. A template cannot loop over a map
	// with a stable order, so the options are a slice.
	SortOptions []SortOption
	// Blocked says whether this reader has blocked the tag, so the control
	// can offer the reverse action and the page can say what blocking did.
	Blocked bool

	// Words is the raw word-count filter as the reader typed it, echoed
	// back into the control so a reload keeps it.
	Words string
	// WordsBound is the parsed integer, used by the query.
	WordsBound int
	// WordsErr is set when the bound could not be parsed. It renders as a
	// message and the list falls back to unfiltered, because silently
	// showing an unfiltered list under a filter the reader set is the
	// failure mode this whole change exists to remove.
	WordsErr string

	// Complete is "true", "false" or "" -- the completion filter as the
	// reader spelled it, echoed so the control shows the current selection.
	Complete string
	// Rating and Lang likewise, for the rating and language controls.
	Rating string
	Lang   string
	// RatingOptions drives the rating control. tagRatingOptions is the
	// measured set; a template cannot range over a map with a stable order.
	RatingOptions []SortOption
	// UnfilteredTagCount is the tag's real size, unaffected by any filter.
	// Separate from Tag.WorkCount, which is the FILTERED count because the
	// heading above the list must describe the set the page contains.
	//
	// They differ exactly when a filter is active, and the empty-result
	// message needs the unfiltered one -- otherwise it reports "the tag has 0
	// works" at the reader, which is the opposite of what happened.
	UnfilteredTagCount int

	// FilterErr is set when complete/rating/lang could not be understood.
	// Separate from WordsErr because it is a different control and a reader
	// needs to know WHICH one was ignored.
	FilterErr string

	// NextN is N doubled, the "show more" target for the truncation note.
	// N rides along so the sort/filter form can carry the reader's chosen
	// count instead of resetting it to 20 on every submit.
	N     int
	NextN int

	// Blend is true when the list is the taste blend (sort=taste): works
	// carrying the tag, boosted, mixed with works readers of this tag
	// bookmarked. Rows know it individually via WorkHit.ShowScore, but the
	// page-level note and heading need it too.
	Blend bool

	// TasteNote explains what the blend actually did -- how many of each
	// kind went in. An unexplained mixed list is a list whose ordering
	// cannot be argued with, which is the one thing this site promises not
	// to ship.
	TasteNote string

	// TasteErr records a failed blend attempt. The page still lists the
	// works carrying the tag, because a recommender that could not
	// recommend must not take the exact matches with it.
	TasteErr string
}

// SortOption is one choice in the sort control.
type SortOption struct {
	Value string
	Label string
}

// Neighbour is a ScoredNeighbour with the tag's name attached.
type Neighbour struct {
	ID    int64
	Name  string
	PMI   float64
	Count int64
}

// TagInfo is a tag as the corpus holds it.
type TagInfo struct {
	ID        int64
	Name      string
	WorkCount int64
}

// WorkHit is one row of a work list.
type WorkHit struct {
	ID        int64
	Title     string
	Author    string
	URL       string
	Summary   string
	Kudos     int64
	Hits      int64
	WordCount int64
	Language  string
	Complete  int64

	// HasTag marks a row that carries the page's tag, on the blended tag
	// list. Purely presentational: the blend already scored it, and the
	// card only needs to say which half of the blend it came from.
	HasTag bool
	// ShowScore and Score make the blend's ordering inspectable. A list
	// whose order cannot be checked against a number is a list the reader
	// has to take on faith, which is what the evidence panel exists to
	// avoid everywhere else on this site.
	ShowScore bool
	Score     float64
}

// AuthorPage is every work whose authors field matches a name.
//
// Matching is a substring search rather than a parsed identity: the mirror
// stores authors as one free-text cell, and a parser that guessed at its
// punctuation would confidently link a reader to the wrong person. The page
// says what it matched ("authors matching X") so the honesty is visible.
type AuthorPage struct {
	Base
	// Query is the name as the reader typed it (or as a byline link
	// carried it), echoed into the form and the heading.
	Query string
	Works []WorkHit
	// Total is every match, not the number shown.
	Total int64
	// Limited is true when Total exceeded N.
	Limited bool
	// N and NextN, same contract as every other list page: the limit that
	// was used, and the doubled target of the "show more" link.
	N     int
	NextN int
}

// AuthorLink is one parsed byline name and where it goes.
type AuthorLink struct {
	Name string
	URL  string
}

// RecommendPage is the multi-seed ranking, with the controls that decide
// what a ranking is FOR.
//
// The controls are on the page rather than hidden behind the URL because the
// two that matter most -- max_per_fandom and group_by -- are the difference
// between a useful list and a monocrop, and a reader who cannot see that
// control cannot use it. See MaxPerFandom's field comment for the measurement
// that makes it non-obvious.
type RecommendPage struct {
	Base
	Similar []rank.Candidate

	// Seeds echo back what was ranked, so the form can show them and so a
	// reader can see that a sixth seed was dropped rather than silently
	// ignored.
	Seeds []SeedRef

	// N is how many were requested; Returned is how many came back. They
	// differ whenever a diversity cap bites, and the page says so.
	N        int
	Returned int

	// MaxPerFandom caps how many results may come from one fandom.
	MaxPerFandom int
	// GroupBy is the diversity axis: "", "fandom", "tag" or "author".
	GroupBy string
	// SeedLimitHit records that seeds were truncated to the maximum.
	SeedLimitHit bool
	// MaxSeeds is that maximum, for the message.
	MaxSeeds int

	// Shortfall is the honest account when fewer came back than were asked
	// for. Rendered rather than hidden: a list of 12 that says nothing about
	// asking for 20 reads as a complete answer.
	ShortfallReason string
	// Tune is the active weight vector, so a reader can see WHY a result
	// ranked where it did rather than having to trust it.
	Tune []WeightRow

	// The rest of the ranking controls, echoed so the form shows the request
	// it made. Without these the filters, the pool mode and the tune name
	// were reachable ONLY by hand-editing the URL: the query parameters
	// worked, nothing on the page set them, and a capability that cannot be
	// found is not usable from the frontend.
	//
	// Filter and PoolMode are the raw request values rather than parsed
	// structs, because a select's value has to round-trip exactly as the
	// reader sent it.
	Filter engine.Filter
	// Filters is the control list the form renders. It is built from the raw
	// query rather than from Filter so a rejected value is echoed back rather
	// than silently blanked -- see recommendFilterRows.
	Filters   []FilterRow
	PoolMode  string
	PoolSize  int
	TuneName  string
	TuneNames []string // stored tunes the reader can pick, best-effort

	// HideSeen is the reader's seen-suppression toggle, echoed so the control
	// shows the state they are in rather than resetting on every render.
	HideSeen bool

	// ReturnTo is the current request path, for the mark-read form to send the
	// reader back to. It is the page's own path rather than the full query:
	// re-submitting the query would re-apply hide_seen=1, which is correct,
	// but carrying seeds is what makes the reader land on the same list.
	ReturnTo string

	// Degraded names the signals that could not run. It is on the recommend
	// page rather than only on /health because a missing signal changes the
	// RANKING, and a reader looking at a list cannot see that it was ranked
	// without collaborative filtering unless the page says so.
	Degraded []string
}

// FilterRow is one filter control on the recommend form, in the shape a
// template needs it.
//
// It is a slice of rows rather than a struct with one field per filter so the
// form renders the same way as the tag page's filters: one loop, one markup,
// and adding a filter is a row rather than an edit to three places.
type FilterRow struct {
	Name    string
	Label   string
	Kind    string // "number", "text", "select", "csv"
	Value   string
	Hint    string
	Values  []Option // for Kind == "select"
	Allow   bool     // whether the filter was accepted; a refused value says so
	Refused string   // why it was refused, when Allow is false
}

// Option is one choice in a filter select.
type Option struct {
	Value    string
	Label    string
	Selected bool
}

// WeightRow is one signal's weight, heaviest first.
type WeightRow struct {
	Name   string
	Weight float64
}

// SeedRef is a seed as the form shows it.
type SeedRef struct {
	Kind string
	ID   int64
	Raw  string
}

// FandomsPage answers "which fandoms would this reader enjoy".
//
// It is a separate page rather than a recommend variant because the question
// has no seeds in the recommender's sense: the input is a taste PROFILE, and
// the output is a distribution, not a ranking of works.
type FandomsPage struct {
	Base
	Rows      []corpusquery.Row
	Notes     []string
	Truncated bool
	// Profile is the profile the ranking was weighted by, empty for the
	// corpus distribution.
	Profile string
	// Gate is the evidence floor, shown because it changes the answer.
	Gate int
	// Available lists stored profiles so the picker is not a free-text guess.
	Available []string
}

// SurprisePage answers "show me something".
//
// Row is zero when the corpus has nothing well-tagged enough to seed from, in
// which case Notes explains why. The template renders both states from one
// struct rather than having a separate error page, because "nothing to pick
// from" is an answer about the corpus and not a failure.
type SurprisePage struct {
	Base
	Row   corpusquery.Row
	Seed  string
	Notes []string
}

// UnderratedPage answers "what is good and under-looked-at".
type UnderratedPage struct {
	Base
	Rows      []corpusquery.Row
	Notes     []string
	Truncated bool
	// Filters echoes what was applied, so a zero-row result is explicable.
	MinWords int64
	Complete bool
}

// NeighboursPage is the tag-co-occurrence view, separated from the tag page
// because a tag page is about WORKS and this is about TAGS.
type NeighboursPage struct {
	Base
	Tag       string
	Rows      []corpusquery.Row
	Notes     []string
	Truncated bool
	// TotalCo is the tag's corpus frequency, needed to read a PMI value: a
	// PMI of 0.4 over 12 works and one over 12,000 are not the same claim.
	TotalCo int
}

// ProfilesPage lists the stored taste profiles and offers the build form.
type ProfilesPage struct {
	Base
	Profiles []ProfileRow
	// Works is the comma-separated seed list for the build form.
	Works string
	// Built names the profile just built, for the confirmation.
	Built string
	// Err is a build failure worth showing on the page rather than as a
	// bare 500: "work 999 not in this corpus" is actionable and a stack of
	// SQL is not.
	Err string
}

// ProfileRow is one profile in the list.
type ProfileRow struct {
	Name   string
	Source string
	Works  int
	Tags   int
}

// ProfilePage is one profile's weights, with the rate form on it.
type ProfilePage struct {
	Base
	Profile profile.Profile
	// Tags are the profile's tags with names resolved, heaviest first.
	Tags []ProfileTagRow
	// Err is a rate failure, shown inline.
	Err string
	// Rated is the work id just rated, for the confirmation.
	Rated int64
	// RatedTags are the weights that moved, so a reader can see the effect
	// of one click rather than inferring it.
	RatedTags []ProfileTagRow
}

// ProfileTagRow is one tag weight with its name.
type ProfileTagRow struct {
	ID     int32
	Name   string
	Weight float64
}

// NotFoundPage is what a browser gets for a URL kindred does not serve.
// The API's own 404 stays JSON: a JSON client parsing an HTML error page
// gets a parse error instead of a message, and a person looking at a page
// gets JSON instead of a sentence.
type NotFoundPage struct {
	Base
	Path string
}

// ErrorPage is a failure a person can read.
type ErrorPage struct {
	Base
	Path    string
	Message string
}

// -- template plumbing ------------------------------------------------------

// pages is one template set PER PAGE.
//
// The obvious implementation -- one set, ParseFS over assets/*.html --
// is wrong in a way that produces 200 with an empty body and no error.
// Every page file defines {{define "content"}}, and Go's template package
// resolves a duplicate name to the LAST one parsed, so exactly one
// "content" survives. Executing "search.html" then finds a template with
// that name and no body, because the name belongs to the FILE, not to
// anything it defined. The result is one byte of whitespace per page.
//
// Each page therefore gets its own set, built from the shared fragments
// plus that one page's file. The cost is parsing five small templates at
// startup instead of one, which is nothing next to the 25 MiB the CSR
// gives back.
var pages = map[string]*template.Template{}

func init() {
	for _, page := range []string{
		"layout.html", "search.html", "work.html", "tag.html",
		"recommend.html", "notfound.html", "error.html",
		"arena.html", "leaderboard.html", "rank.html", "myranking.html",
		"block.html", "fandoms.html", "underrated.html", "neighbours.html",
		"surprise.html",
		"profiles.html", "profile.html",
		"author.html",
	} {
		pages[page] = template.Must(template.New(page).
			Funcs(funcMap).
			ParseFS(assets,
				"assets/layout.html",
				"assets/"+page,
			))
	}
}

// funcMap is the only place templates are given new vocabulary.
//
// dict exists because Go's {{template}} takes exactly one argument, and
// a row of four stat cells wants four values in one place.
// statRow is one labelled figure on a work page. It is a slice rather
// than a map because Go's {{range}} over a map yields values in sorted
// KEY order, which would print the words count under the label "Hits".
// The failure is silent and the page still looks plausible.
type statRow struct {
	Key   string
	Label string
}

// workStats is the order the labels appear in, most interesting first.
var workStats = []statRow{
	{"word_count", "Words"},
	{"hits", "Hits"},
	{"kudos", "Kudos"},
	{"bookmarks", "Bookmarks"},
	{"complete", "Complete"},
}

var funcMap = template.FuncMap{
	"dict": func(values ...any) (map[string]any, error) {
		if len(values)%2 != 0 {
			return nil, fmt.Errorf("dict needs an even number of arguments, got %d", len(values))
		}
		m := make(map[string]any, len(values)/2)
		for i := 0; i < len(values); i += 2 {
			k, ok := values[i].(string)
			if !ok {
				return nil, fmt.Errorf("dict keys must be strings, got %T", values[i])
			}
			m[k] = values[i+1]
		}
		return m, nil
	},
	"stat":        stat,
	"commas":      commas,
	"shorten":     shorten,
	"pct":         percent,
	"taghits":     func(c rank.Candidate) []TagHit { return CandidateTags(c) },
	"workstats":   func() []statRow { return workStats },
	"authorlinks": authorLinks,

	// add is 1-based indexing for "the # column", because a table that starts
	// at 0 reads as an off-by-one even when it is not.
	"add": func(a, b int) int { return a + b },

	// urlquery percent-escapes a value for a query-string parameter.
	//
	// It exists because fandom names contain spaces, slashes and ampersands
	// ("harry potter - all media types", "a/b", "x & y"), and an unescaped
	// href renders as a link that goes somewhere else or nowhere. html/template
	// escapes for HTML, not for a URI component.
	"urlquery": url.QueryEscape,

	// int64 converts a template-visible int to int64 for commas(), which takes
	// int64. Without it the call site needs a cast in every row.
	"int64": func(v int) int64 { return int64(v) },
}

// stat reads one number out of a work's Stats map.
//
// The map is the whole reason this function exists. A key that is absent
// from a Go map returns the zero value, so a naive {{.Stats.bookmarks}}
// prints "0" for a bookmark count nobody has recorded -- and a page that
// says "0" is making a claim. The corpus has NULL in several of these
// columns, so the honest rendering is a dash.
//
// It returns (string, bool) because that is what reads well -- but
// html/template only accepts a second return of type error, and a bool
// there is a PANIC AT INIT rather than a compile error. So the exported
// shape is a single string, and the template tests it against the empty
// string to decide whether to draw the row. Missing and zero are
// therefore never confused: missing yields the dash, zero yields "0".
func stat(m map[string]float64, key string) string {
	if m == nil {
		return dash
	}
	v, ok := m[key]
	if !ok {
		return dash
	}
	switch key {
	case "bookmarks", "kudos", "hits", "word_count":
		return commas(int64(v))
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// dash is what an absent number renders as. A page that says "0" is
// making a claim about data it does not have.
const dash = "—"

// commas renders an integer with thousands separators, because 2384712
// is much harder to read at a glance than 2,384,712.
// commas renders an integer with thousands separators.
//
// It takes any integer type rather than int64, which is a response to a real
// bug: corpusquery.Row.Works is an `int` and NeighboursPage.TotalCo was too,
// so `{{commas .TotalCo}}` was a 500 ("expected int64; got int") on every
// /neighbours request with a tag. html/template resolves that at RENDER time,
// not parse time, so `go build`, `go vet` and every Go test passed while the
// page was broken in the browser.
//
// A template helper taking a single concrete numeric width means every new
// field is a potential 500 discovered by a user. `any` plus a type switch
// makes the mismatch impossible to express.
func commas(n any) string {
	var s string
	switch v := n.(type) {
	case int:
		s = strconv.FormatInt(int64(v), 10)
	case int32:
		s = strconv.FormatInt(int64(v), 10)
	case int64:
		s = strconv.FormatInt(v, 10)
	case uint:
		s = strconv.FormatUint(uint64(v), 10)
	case uint32:
		s = strconv.FormatUint(uint64(v), 10)
	case uint64:
		s = strconv.FormatUint(v, 10)
	case float64:
		s = strconv.FormatInt(int64(v), 10)
	default:
		// Visible, not silent: a struct field that never reaches this branch
		// is the one to fix, and the reader should see which it was.
		return fmt.Sprintf("%v", n)
	}
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// shorten trims a summary to a whole number of sentences, for a list page
// where the full text is one click away. Cutting mid-word mid-sentence
// reads as broken rather than as abbreviated.
//
// It has to skip over the dots that are NOT sentence endings, because a
// summary that opens "Dr. Smith has 3.5k words. Then more." used to render as
// "Dr." -- four pages showed a reader a title instead of a sentence. A period
// ends a sentence only when:
//
//   - it is followed by whitespace or the end of the string (so "3.5" and
//     "example.com" are left alone), AND
//   - the thing before it is not a single capital letter (so "A." and "J. R. R."
//     are initials, not one-character sentences), AND
//   - the token before it is not a known title abbreviation.
//
// Iterating over runes rather than bytes matters: "Café naïve." puts a
// two-byte rune before the period, and a byte-indexed slice can land inside it.
// trim(), below, guards the same hazard and says so; this did not, which is how
// the two ended up inconsistent.
func shorten(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if s == "" || max <= 0 {
		return s
	}
	r := []rune(s)
	count := 0
	for i := 0; i < len(r); i++ {
		if r[i] != '.' || !sentenceEndsAt(r, i) {
			continue
		}
		count++
		if count >= max {
			return strings.TrimRight(string(r[:i+1]), " ")
		}
	}
	return s
}

// titleAbbreviations are the tokens whose trailing period does not end a
// sentence. The list is deliberately short: it covers the forms that actually
// appear in AO3 summaries, and a token not on it is treated as a real sentence
// ending, which truncates rather than over-keeps. Erring that way loses a
// little text; erring the other way loses the reader the sentence.
var titleAbbreviations = map[string]bool{
	"dr": true, "mr": true, "mrs": true, "ms": true, "prof": true,
	"st": true, "sr": true, "jr": true, "rev": true, "hon": true,
	"capt": true, "sgt": true, "lt": true, "col": true, "gen": true,
	"inc": true, "ltd": true, "co": true, "vs": true, "etc": true,
	"approx": true, "dept": true, "est": true, "fig": true, "no": true,
	"vol": true, "pp": true,
}

// sentenceEndsAt reports whether the period at index i terminates a sentence.
func sentenceEndsAt(r []rune, i int) bool {
	// Must be followed by whitespace or end of string. This alone rejects "3.5"
	// and "example.com"; without it every decimal and every URL truncates.
	if i+1 < len(r) && !unicode.IsSpace(r[i+1]) {
		return false
	}
	// Walk back over the token that precedes the period. The case is preserved
	// here and lowered only for the abbreviation lookup below: lowercasing
	// first and then asking unicode.IsUpper would always be false, which is how
	// this check shipped dead the first time.
	j := i - 1
	for j >= 0 && !unicode.IsSpace(r[j]) {
		j--
	}
	if j+1 >= i {
		return false
	}
	token := string(r[j+1 : i])
	// A lone capital letter is an initial: "A." in "A. B. C. wrote this."
	// Checked on the original case, not the lowered token.
	if isSingleRune(token) && unicode.IsUpper([]rune(token)[0]) {
		return false
	}
	return !titleAbbreviations[strings.ToLower(token)]
}

// isSingleRune reports whether s is exactly one rune (so []rune(s) has len 1).
func isSingleRune(s string) bool { return len([]rune(s)) == 1 }

// percent formats a 0..1 ratio.
func percent(v float64) string {
	return strconv.FormatFloat(v*100, 'f', 1, 64) + "%"
}

// render executes a named template into a buffer.
//
// Buffering first and writing second is not a micro-optimisation: it
// means a template that fails halfway through produces a 500 rather than
// 200 with a page that stops in the middle of a sentence.
func render(w *bytes.Buffer, name string, data any) error {
	t, ok := pages[name]
	if !ok {
		return fmt.Errorf("no such page template %q", name)
	}
	// Execute the "layout" wrapper, not the file name: layout.html's body
	// is entirely {{define "layout"}}, so executing the file name would
	// render nothing for the same reason the pages were empty before.
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		return fmt.Errorf("render %s: %w", name, err)
	}
	return nil
}

// Render writes one named page. The caller supplies the ResponseWriter
// because the content type is set before the first write, and a template
// that errors after headers are flushed cannot change it.
func Render(w *bytes.Buffer, name string, data any) error { return render(w, name, data) }

// CandidateTags splits a candidate's tag names into the display form. The
// recommender returns tag names already, so there is no lookup here: one
// fewer query per row, and the names cannot disagree with the graph.
func CandidateTags(c rank.Candidate) []TagHit {
	out := make([]TagHit, 0, len(c.TagNames))
	for i, n := range c.TagNames {
		h := TagHit{Name: n}
		if i < len(c.TagIDs) {
			h.ID = int64(c.TagIDs[i])
		}
		out = append(out, h)
	}
	return out
}

// authorLinks splits a works.authors cell into per-name profile links.
//
// The split is deliberately conservative: on "," and on " - " (the two
// separators the corpus actually contains), and nothing else. A name with a
// hyphen inside it ("X-Files fan") must survive as one name, and guessing at
// more punctuation would turn real names into fragments that then match
// someone else's works. Every fragment becomes a substring search on the
// author page, so even a mis-split finds its own works and nothing that
// contains it.
func authorLinks(s string) []AuthorLink {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// Normalise the explicit separator first so one pass over commas
	// catches both shapes.
	s = strings.ReplaceAll(s, " - ", ", ")
	var out []AuthorLink
	for _, part := range strings.Split(s, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		out = append(out, AuthorLink{
			Name: name,
			URL:  "/author?q=" + url.QueryEscape(name),
		})
		if len(out) >= 8 {
			break // a byline longer than this is a wall of names, not links
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
