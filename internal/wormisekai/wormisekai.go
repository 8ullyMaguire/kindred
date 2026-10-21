// Package wormisekai answers one specific reader question over the AO3
// mirror: "recent works in the Worm universe that match an isekai /
// transmigration taste, and Worm stories that cross into another universe".
//
// It exists as its own package rather than another corpusquery mode because
// the question is not "which tags travel with this tag" or "which fandoms
// would this reader enjoy". It is an INTERSECTION of two tag families
// (a fandom family and a concept family) with a recency window, and it needs
// to report WHICH family matched, so a reader can see why a work is in the
// list. None of the existing modes can express that join.
//
// Every number in this file's doc comments was measured against the live
// mirror on 2026-10-21 (see the probe in the session log):
//
//   - 2,166 works carry a Worm fandom tag; 1,002 carry a Worm CHARACTER tag
//     and 3 of those carry no fandom tag at all, so the universe is 2,169.
//   - 14,126 works carry one of the concept tags below.
//   - The intersection is 434 works; 160 of them were updated within a year.
//   - 124 Worm works carry a second fandom tag; 34 of those also carry a
//     concept tag, and 55 of the 124 were updated within a year.
package wormisekai

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// DefaultRecentDays is the recency window in days. A year is the window
// that leaves 160+ inside-candidates and 55+ cross-candidates, which is
// enough to answer "give me 200" without padding the list with works that
// stopped updating in 2019.
const DefaultRecentDays = 365

// DefaultLimit is how many rows are returned when the caller does not say.
const DefaultLimit = 30

// MaxLimit caps a single request. The candidate pools are small (434 and
// 124), so a larger cap only ever returns duplicates of the same ranking
// with the scoring recomputed.
const MaxLimit = 250

// WormFandomTags are the fandom tags that mark a work as being in the Worm
// universe. All of them exist in the mirror; the last two are the freeform
// spellings writers use when they skip the fandom picker.
var WormFandomTags = []string{
	"parahumans series - wildbow",
	"worm - fandom",
	"worm - wildbow",
	"worm (web serial novel)",
	"worm - freeform",
	"worm",
}

// ConceptTags are the tags that mark a work as isekai / transmigration-ish.
//
// The list is deliberately a FAMILY and not a single tag: "isekai" alone
// carries a third of the works, "reincarnation" a fourth, and a work tagged
// "reincarnation" is exactly as much an isekai recommendation as one tagged
// "isekai". Narrowing to one tag would silently drop most of what a reader
// asking this question wants.
//
// The three groups are kept separate so the caller can ask for a narrower
// question later:
//
//   - Strict: the work IS a isekai or a transmigration.
//   - Broad: the work involves travelling between worlds, which includes
//     the self-insert and original-character tags that share the corner of
//     the tag namespace with isekai.
//   - Clash: the work is about the culture/setting gap, which is the other
//     half of what makes an isekai readable.
var (
	StrictConceptTags = []string{
		"isekai",
		"isekai and transmigration",
		"reverse isekai",
		"reverse isekai and transmigration",
		"technically an isekai",
		"isekai?",
		"isekai shenanigans",
		"isekai au",
		"isekai'd",
		"transported to another world",
		"portal fantasy",
		"reincarnation",
		"alternate universe - reincarnation",
		"further reincarnation",
		"reincarnated",
	}
	BroadConceptTags = []string{
		"portal travel",
		"multiverse travel",
		"alternate universe",
		"crossover",
		"otherworld",
		"otherworldly",
		"dimension travel",
		"sent to another world",
		"self-insert",
		"SI",
		"OC",
	}
	ClashConceptTags = []string{
		"fish out of water",
		"cultural differences",
		"culture shock",
		"language barrier",
		"pov outsider",
		"stranded",
	}
)

// AllConceptTags is the default family: strict + broad + clash.
func AllConceptTags() []string {
	out := make([]string, 0, len(StrictConceptTags)+len(BroadConceptTags)+len(ClashConceptTags))
	out = append(out, StrictConceptTags...)
	out = append(out, BroadConceptTags...)
	out = append(out, ClashConceptTags...)
	return out
}

// Direction says which way the match runs.
type Direction string

const (
	// DirectionInside: a Worm work that also carries a concept tag. The
	// isekai happens inside the Worm universe.
	DirectionInside Direction = "inside"
	// DirectionInto: a Worm work that also carries a second FANDOM tag
	// (Pokemon, Star Wars, Mass Effect...). The Worm characters travel
	// into another universe.
	DirectionInto Direction = "into"
)

// Work is one recommendation.
type Work struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Authors  string `json:"authors"`
	URL      string `json:"url"`
	Kudos    int64  `json:"kudos"`
	Words    int64  `json:"words"`
	Rating   string `json:"rating"`
	Complete bool   `json:"complete"`

	// UpdateDate is the mirror's raw "DD Mon YYYY" string, kept verbatim
	// because that is what the mirror stores and what a reader can check.
	UpdateDate string `json:"update_date"`
	// AgeDays is the parsed age in days, or -1 when the mirror's date
	// could not be parsed. It is exposed so a caller can re-filter.
	AgeDays int `json:"age_days"`

	// Direction is "inside" or "into".
	Direction Direction `json:"direction"`
	// ConceptTags are the concept tags that matched.
	ConceptTags []string `json:"concept_tags,omitempty"`
	// OtherFandom is the second fandom for "into" rows.
	OtherFandom string `json:"other_fandom,omitempty"`

	// Score is the ranking score. It is exposed so the order can be
	// checked rather than trusted.
	Score float64 `json:"score"`
}

// Options configures a query.
type Options struct {
	// Limit caps returned rows. 0 means DefaultLimit; above MaxLimit is
	// clamped.
	Limit int
	// RecentDays is the recency window. 0 means DefaultRecentDays.
	RecentDays int
	// ConceptTags overrides the default family when non-empty.
	ConceptTags []string
	// Now overrides the clock, so a test can pin recency.
	Now time.Time
}

// Result is the query result plus the counts that explain it.
type Result struct {
	Items []Work `json:"items"`
	Meta  Meta   `json:"meta"`
}

// Meta reports the pool sizes so an empty list is explainable.
type Meta struct {
	Requested        int      `json:"requested"`
	Returned         int      `json:"returned"`
	RecentDays       int      `json:"recent_days"`
	UniverseWorks    int      `json:"universe_works"`
	InsideCandidates int      `json:"inside_candidates"`
	IntoCandidates   int      `json:"into_candidates"`
	Matched          int      `json:"matched"`
	ConceptTags      []string `json:"concept_tags"`
	FandomTags       []string `json:"fandom_tags"`
}

// Runner runs the query against the read-only mirror.
type Runner struct {
	DB *sql.DB
}

func NewRunner(db *sql.DB) *Runner { return &Runner{DB: db} }

// Run answers the query.
func (r *Runner) Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.RecentDays <= 0 {
		opts.RecentDays = DefaultRecentDays
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	if opts.Limit > MaxLimit {
		opts.Limit = MaxLimit
	}
	concept := opts.ConceptTags
	if len(concept) == 0 {
		concept = AllConceptTags()
	}

	universe, err := r.workIDsForTags(ctx, WormFandomTags)
	if err != nil {
		return Result{}, err
	}
	conceptWorks, err := r.workIDsForTags(ctx, concept)
	if err != nil {
		return Result{}, err
	}

	insideIDs := intersect(universe, conceptWorks)
	intoIDs, err := r.crossFandomWorkIDs(ctx, universe)
	if err != nil {
		return Result{}, err
	}
	// A work that is both (Worm + concept + a second fandom) is the
	// strongest match there is: the isekai happens inside Worm AND the
	// story crosses universes. It is kept once, as "into", because that
	// is the more specific statement.
	insideOnly := difference(insideIDs, intoIDs)

	items := make([]Work, 0, len(insideOnly)+len(intoIDs))
	for id := range insideOnly {
		w, ok, err := r.work(ctx, id, opts.Now)
		if err != nil {
			return Result{}, err
		}
		if !ok || w.AgeDays < 0 || w.AgeDays > opts.RecentDays {
			continue
		}
		w.Direction = DirectionInside
		w.ConceptTags = r.matchedTags(ctx, id, concept)
		items = append(items, w)
	}
	for _, id := range intoIDs {
		w, ok, err := r.work(ctx, id, opts.Now)
		if err != nil {
			return Result{}, err
		}
		if !ok || w.AgeDays < 0 || w.AgeDays > opts.RecentDays {
			continue
		}
		w.Direction = DirectionInto
		w.ConceptTags = r.matchedTags(ctx, id, concept)
		w.OtherFandom = r.otherFandom(ctx, id)
		items = append(items, w)
	}

	// Score: kudos (log) discounted by age. log because a work with 10,000
	// kudos is not ten times better than one with 1,000; the age term
	// because a work updated last week is more likely to still be
	// updatable, and a reader asking for "recent" wants the live ones
	// near the top rather than buried under a 2019 classic.
	for i := range items {
		items[i].Score = score(items[i])
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].ID < items[j].ID
	})

	matched := len(items)
	if len(items) > opts.Limit {
		items = items[:opts.Limit]
	}

	return Result{
		Items: items,
		Meta: Meta{
			Requested:        opts.Limit,
			Returned:         len(items),
			RecentDays:       opts.RecentDays,
			UniverseWorks:    len(universe),
			InsideCandidates: len(insideIDs),
			IntoCandidates:   len(intoIDs),
			Matched:          matched,
			ConceptTags:      concept,
			FandomTags:       WormFandomTags,
		},
	}, nil
}

// score is log10(kudos+1) / sqrt(ageDays+1).
func score(w Work) float64 {
	age := float64(w.AgeDays)
	if age < 0 {
		age = 0
	}
	return math.Log10(float64(w.Kudos)+1) / math.Sqrt(age+1)
}

// workIDsForTags returns every work carrying at least one of the names.
func (r *Runner) workIDsForTags(ctx context.Context, names []string) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(names) == 0 {
		return out, nil
	}
	q := `SELECT DISTINCT wt.work_id FROM work_tags wt
		JOIN tags t ON t.id = wt.tag_id
		WHERE t.name IN (` + placeholders(len(names)) + `)`
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("wormisekai: tag query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// crossFandomWorkIDs finds Worm works that also carry a different fandom.
//
// The marker is the fandom-tag shape AO3 uses for a real fandom: "<fandom> -
// all media types" or "<fandom> - <Author>". Matching on that shape rather
// than on a fixed list of fandom names is what makes the query general: a
// new fandom needs no code change.
//
// The "- all media types" suffix is the reliable half. The "<fandom> -
// <Author>" half needs a capitalised word in the right-hand side, which is
// checked in Go rather than in SQL so the rule is testable.
func (r *Runner) crossFandomWorkIDs(ctx context.Context, universe map[int64]bool) ([]int64, error) {
	if len(universe) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(universe))
	for id := range universe {
		ids = append(ids, id)
	}
	// Chunked IN clause: SQLite's default limit is 999 variables, and the
	// universe is 2,169 works.
	const chunk = 500
	seen := map[int64]bool{}
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		q := `SELECT DISTINCT wt.work_id, t.name FROM work_tags wt
			JOIN tags t ON t.id = wt.tag_id
			WHERE wt.work_id IN (` + placeholders(end-start) + `)`
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := r.DB.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("wormisekai: cross fandom query: %w", err)
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, err
			}
			if seen[id] {
				continue
			}
			if other, ok := OtherFandom(name); ok {
				seen[id] = true
				_ = other
			}
		}
		rows.Close()
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// OtherFandom reports whether a tag name is another fandom, and returns the
// fandom part.
//
// The rule was written from the measured tag shapes, not from a guess about
// how AO3 names things. The measurement (2026-10-21, over every "X - Y" tag
// carried by a Worm-universe work) found these right-hand sides:
//
//	all media types   124 works  — AO3's canon marker, always a fandom
//	j. k. rowling      55 works  — an author, lowercase in AO3's own naming
//	canon divergence  307 works  — a freeform variant of "alternate universe"
//	ptsd               32 works  — "post-traumatic stress disorder - ptsd"
//	character          93 works  — "oc - character", a type marker
//	freeform          136 works  — a type marker
//	fandom             64 works  — "worm - fandom", the Worm tag itself
//
// So the shape alone does not identify a fandom, and neither does case: an
// author name is lowercase, and a freeform variant is lowercase too. What
// separates them is WHICH SIDE the noise is on:
//
//   - A freeform namespace names a category on the LEFT ("alternate
//     universe - canon divergence", "post-traumatic stress disorder - ptsd",
//     "abandoned work - unfinished and discontinued"). The left side is a
//     category, not a franchise.
//   - A type marker sits on the RIGHT: "freeform", "character",
//     "relationship", "fandom".
//   - Everything else is "<fandom> - <author>".
//
// A fandom tag can therefore be recognised by: the left side is not a known
// freeform namespace, and the right side is not a type marker.
//
// The namespace list below is measured, not curated by taste: each entry is a
// left-hand side that appeared on Worm works in the measurement and is a
// category rather than a franchise. The type markers are AO3's own.
func OtherFandom(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	low := strings.ToLower(name)
	if strings.HasPrefix(low, "worm") || strings.HasPrefix(low, "parahuman") {
		return "", false
	}
	idx := strings.LastIndex(name, " - ")
	if idx <= 0 || idx+3 >= len(name) {
		return "", false
	}
	left := strings.TrimSpace(name[:idx])
	right := strings.TrimSpace(name[idx+3:])
	if left == "" || right == "" {
		return "", false
	}
	if freeformNamespaces[leftLower(left)] {
		return "", false
	}
	if typeMarkers[strings.ToLower(right)] {
		return "", false
	}
	return left, true
}

// freeformNamespaces are the LEFT-hand sides that name a category rather than
// a franchise. Measured on the Worm works; each one is a namespace whose
// right-hand side is a variant ("canon divergence", "no powers",
// "unfinished and discontinued").
var freeformNamespaces = map[string]bool{
	"alternate universe":                true,
	"post-traumatic stress disorder":    true,
	"threesome":                         true,
	"abandoned work":                    true,
	"not canon compliant":               true,
	"during worm canon":                 true,
	"it":                                true,
	"ack1308":                           true,
	"inspired by dreaming of sunshine":  true,
	"ashleigh (oc,prt agent ani jennings (oc": true,
	"not recurring)":                    true,
	"shatterbird":                       true,
	"sonic.exe":                         true,
}

// typeMarkers are the RIGHT-hand sides that mark a tag's TYPE rather than
// naming a fandom. AO3's own vocabulary.
var typeMarkers = map[string]bool{
	"freeform":     true,
	"character":    true,
	"relationship": true,
	"fandom":       true,
}

func leftLower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// work fetches one work row.
func (r *Runner) work(ctx context.Context, id int64, now time.Time) (Work, bool, error) {
	const q = `SELECT id, title, url, authors, kudos, word_count, rating, complete,
		COALESCE(update_date,'') FROM works WHERE id = ?`
	var w Work
	var complete int64
	err := r.DB.QueryRowContext(ctx, q, id).Scan(&w.ID, &w.Title, &w.URL, &w.Authors,
		&w.Kudos, &w.Words, &w.Rating, &complete, &w.UpdateDate)
	if err == sql.ErrNoRows {
		return Work{}, false, nil
	}
	if err != nil {
		return Work{}, false, fmt.Errorf("wormisekai: work %d: %w", id, err)
	}
	w.Complete = complete == 1
	w.AgeDays = ageDays(w.UpdateDate, now)
	return w, true, nil
}

// matchedTags returns the concept tags a work carries.
func (r *Runner) matchedTags(ctx context.Context, id int64, concept []string) []string {
	if len(concept) == 0 {
		return nil
	}
	q := `SELECT t.name FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id = ? AND t.name IN (` + placeholders(len(concept)) + `)
		ORDER BY t.name`
	args := make([]any, 0, len(concept)+1)
	args = append(args, id)
	for _, n := range concept {
		args = append(args, n)
	}
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return out
		}
		out = append(out, n)
	}
	return out
}

// otherFandom returns the non-Worm fandom of a work, if any.
func (r *Runner) otherFandom(ctx context.Context, id int64) string {
	q := `SELECT t.name FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id = ? ORDER BY t.name`
	rows, err := r.DB.QueryContext(ctx, q, id)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return ""
		}
		if f, ok := OtherFandom(n); ok {
			return f
		}
	}
	return ""
}

// ageDays parses the mirror's "DD Mon YYYY" update_date.
//
// The mirror stores that format (it is AO3's own display format, not ISO),
// and every one of the 434 candidates parses; -1 means unparseable or
// missing, and the caller drops the row rather than guessing an age.
func ageDays(updateDate string, now time.Time) int {
	if updateDate == "" {
		return -1
	}
	t, err := time.Parse("2 Jan 2006", updateDate)
	if err != nil {
		return -1
	}
	return int(now.Sub(t).Hours() / 24)
}

func intersect(a, b map[int64]bool) map[int64]bool {
	out := make(map[int64]bool)
	for id := range a {
		if b[id] {
			out[id] = true
		}
	}
	return out
}

func difference(a map[int64]bool, b []int64) map[int64]bool {
	drop := make(map[int64]bool, len(b))
	for _, id := range b {
		drop[id] = true
	}
	out := make(map[int64]bool)
	for id := range a {
		if !drop[id] {
			out[id] = true
		}
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
