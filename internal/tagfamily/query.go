package tagfamily

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// DefaultRecentDays is the recency window when a caller names none.
const DefaultRecentDays = 365

// DefaultLimit is the number of rows returned when a caller names none.
const DefaultLimit = 50

// QueryOptions configures a family-intersection query.
type QueryOptions struct {
	// UniverseTags are the fandom spellings a work must carry at least
	// one of. This is the franchise axis.
	UniverseTags []string
	// ConceptTags are the concept spellings a work must carry at least
	// one of. This is the idea axis.
	ConceptTags []string
	// RecentDays keeps only works updated within the window. Zero means
	// DefaultRecentDays.
	RecentDays int
	// Now pins the clock so a test can pin recency.
	Now time.Time
	// Limit caps the rows returned.
	Limit int
}

// Item is one matching work, with the evidence for why it matched.
type Item struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Authors  string `json:"authors"`
	Kudos    int64  `json:"kudos"`
	Words    int64  `json:"words"`
	Rating   string `json:"rating"`
	Complete bool   `json:"complete"`
	// UpdateDate is the mirror's raw "DD Mon YYYY" string, kept verbatim
	// because that is what the mirror stores and what a reader can check.
	UpdateDate string `json:"update_date"`
	// AgeDays is the parsed age in days, or -1 when the date could not
	// be parsed.
	AgeDays int `json:"age_days"`
	// MatchedTags are the concept tags the work carries.
	MatchedTags []string `json:"matched_tags,omitempty"`
	// OtherFandom is a DIFFERENT fandom the work carries, when it has
	// one. It is the general "this story crosses universes" fact, and it
	// is empty for a work that stays inside one universe.
	OtherFandom string `json:"other_fandom,omitempty"`
	// Score is the ranking score, exposed so the order can be checked.
	Score float64 `json:"score"`
}

// Meta reports the pool sizes so an empty list is explainable.
type Meta struct {
	UniverseWorks int `json:"universe_works"`
	ConceptWorks  int `json:"concept_works"`
	Matched       int `json:"matched"`
	Returned      int `json:"returned"`
	RecentDays    int `json:"recent_days"`
}

// Result is a query result plus the counts that explain it.
type Result struct {
	Items []Item `json:"items"`
	Meta  Meta   `json:"meta"`
}

// Query answers: which works carry a tag from BOTH families, were updated
// within the window, and rank highest.
//
// The score is log10(kudos+1)/sqrt(ageDays+1): log because a work with
// 10,000 kudos is not ten times better than one with 1,000; the age term
// because a reader asking for "recent" wants the live ones near the top
// rather than buried under a 2019 classic. This is the same scorer the
// bespoke worm-isekai command used, moved here unchanged.
func Query(ctx context.Context, db *sql.DB, opts QueryOptions) (Result, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.RecentDays <= 0 {
		opts.RecentDays = DefaultRecentDays
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	// The Shape is built ONCE per query, not once per work: LoadShape
	// scans the tags table and takes about 2.3 seconds against the live
	// mirror. otherFandom uses it for every row.
	sh, err := LoadShape(ctx, db)
	if err != nil {
		return Result{}, err
	}
	universe, err := workIDsForTags(ctx, db, opts.UniverseTags)
	if err != nil {
		return Result{}, err
	}
	concept, err := workIDsForTags(ctx, db, opts.ConceptTags)
	if err != nil {
		return Result{}, err
	}
	var res Result
	res.Meta.UniverseWorks = len(universe)
	res.Meta.ConceptWorks = len(concept)
	res.Meta.RecentDays = opts.RecentDays

	ids := make([]int64, 0, len(universe))
	for id := range universe {
		if concept[id] {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		it, ok, err := work(ctx, db, id, opts.Now)
		if err != nil {
			return Result{}, err
		}
		if !ok || it.AgeDays < 0 || it.AgeDays > opts.RecentDays {
			continue
		}
		it.MatchedTags = matchedTags(ctx, db, id, opts.ConceptTags)
		it.OtherFandom = otherFandom(ctx, db, sh, id, opts.UniverseTags)
		items = append(items, it)
	}
	res.Meta.Matched = len(items)
	for i := range items {
		items[i].Score = score(items[i])
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].ID < items[j].ID
	})
	if len(items) > opts.Limit {
		items = items[:opts.Limit]
	}
	res.Items = items
	res.Meta.Returned = len(items)
	return res, nil
}

// score is log10(kudos+1) / sqrt(ageDays+1).
func score(it Item) float64 {
	age := float64(it.AgeDays)
	if age < 0 {
		age = 0
	}
	return math.Log10(float64(it.Kudos)+1) / math.Sqrt(age+1)
}

// ageDays parses the mirror's "DD Mon YYYY" date.
//
// The date is TEXT, so it cannot be compared as a string: measured, the
// lexicographic maximum ("31 Oct 2025") is a year older than the
// chronological maximum ("26 Jun 2026"). Every recency decision here is
// made in Go for that reason.
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

// workIDsForTags returns every work carrying at least one of the names.
func workIDsForTags(ctx context.Context, db *sql.DB, names []string) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(names) == 0 {
		return out, nil
	}
	q := `SELECT DISTINCT wt.work_id FROM work_tags wt
		JOIN tags t ON t.id = wt.tag_id
		WHERE t.name COLLATE NOCASE IN (` + placeholders(len(names)) + `)`
	args := make([]any, 0, len(names))
	for _, n := range names {
		args = append(args, n)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("tagfamily: tag query: %w", err)
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

// work fetches one work row.
func work(ctx context.Context, db *sql.DB, id int64, now time.Time) (Item, bool, error) {
	const q = `SELECT id, title, url, authors, kudos, word_count, rating, complete,
		COALESCE(update_date,'') FROM works WHERE id = ?`
	var it Item
	var complete int64
	err := db.QueryRowContext(ctx, q, id).Scan(&it.ID, &it.Title, &it.URL, &it.Authors,
		&it.Kudos, &it.Words, &it.Rating, &complete, &it.UpdateDate)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("tagfamily: work %d: %w", id, err)
	}
	it.Complete = complete == 1
	it.AgeDays = ageDays(it.UpdateDate, now)
	return it, true, nil
}

// matchedTags returns the concept tags a work carries.
func matchedTags(ctx context.Context, db *sql.DB, id int64, tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	q := `SELECT t.name FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id = ? AND t.name COLLATE NOCASE IN (` + placeholders(len(tags)) + `)
		ORDER BY t.name`
	args := make([]any, 0, len(tags)+1)
	args = append(args, id)
	for _, n := range tags {
		args = append(args, n)
	}
	rows, err := db.QueryContext(ctx, q, args...)
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

// otherFandom returns a DIFFERENT fandom the work carries, if any.
//
// The rule is the corpus-derived Shape, not a list of freeform
// namespaces. The bespoke implementation carried a hand-measured list of
// 13 namespaces gathered from Worm works, which would be wrong for any
// other franchise; the shape rule was measured at precision 1.000 against
// AO3's own typed markup.
//
// The universe's own spellings are skipped, so a work is not reported as
// crossing into itself.
func otherFandom(ctx context.Context, db *sql.DB, sh *Shape, id int64, universeTags []string) string {
	own := map[string]bool{}
	for _, t := range universeTags {
		own[strings.ToLower(t)] = true
	}
	q := `SELECT t.name FROM work_tags wt JOIN tags t ON t.id = wt.tag_id WHERE wt.work_id = ?`
	rows, err := db.QueryContext(ctx, q, id)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return ""
		}
		if own[strings.ToLower(n)] {
			continue
		}
		if !sh.IsFandom(n) {
			continue
		}
		// The fandom part is the left of " - ", or the name without its
		// parenthetical medium.
		base := sh.Base(n)
		if base == "" || own[base] {
			continue
		}
		return base
	}
	return ""
}

// placeholders builds "?,?,?" for an IN clause.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
