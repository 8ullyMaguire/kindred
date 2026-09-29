package web

import (
	"bytes"
	"fmt"
	"html/template"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
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
}

// SearchPage is the root page and the result of a tag search. There is no
// separate home page: a recommender with nothing to seed from has nothing
// to say, so the search box IS the home page.
type SearchPage struct {
	Base
	Results []TagHit
	Total   int
	Limited bool
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
	"stat":      stat,
	"commas":    commas,
	"shorten":   shorten,
	"pct":       percent,
	"taghits":   func(c rank.Candidate) []TagHit { return CandidateTags(c) },
	"workstats": func() []statRow { return workStats },
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
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
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
func shorten(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if s == "" || max <= 0 {
		return s
	}
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			count++
			if count >= max {
				return s[:i+1]
			}
		}
	}
	return s
}

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
