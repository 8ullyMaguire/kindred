package corpus

// A tag's shelf in numbers (#54).
//
// The tag page could say how many works carry a tag and list them. It could not
// say what the tag's shelf IS like: how long those works are, how many kudos a
// typical one gets, which single work readers saved most, and whether the tag is
// growing or fading.
//
// # Why medians and not averages
//
// AO3's distributions are extremely skewed: one 400k-word epic in a tag with
// mostly 2k-word fics moves the mean by tens of thousands of words, and a reader
// told "average length 41,000 words" will open the tag expecting something that
// does not exist. The median is the work a reader is actually likely to meet, so
// that is what the page prints — and the mean is kept beside it, labelled, so the
// skew is visible rather than hidden. Measured on the live mirror, tag 29:
// median 30,603 words against a mean of 76,222.
//
// # Why one pass, not five queries
//
// The first version asked SQLite five separate questions — aggregates, median
// words, median kudos, top work, trend — each a full scan of the tag's works, and
// the two medians sorted the tag's whole work list inside the query.
//
// The rewrite was prompted by a bad measurement, worth recording: the tag page
// read 4.06 s and 0.35 s had been measured for "the same page" earlier, so the
// feature looked like a 3.7 s regression. It was not. The default sort is the
// engine's taste blend, which costs 3.1 s of that 3.67 s on its own (sort=kudos
// answers in 0.54 s), and the 0.35 s reading predated that default. The shelf's
// own query is 67 ms. The five-scan version is still worth folding into one pass —
// it is five full scans of 55,810 rows against one — but it was not the cost.
//
// This reads (words, kudos, bookmarks, year) for the tag's works ONCE and computes
// everything here: two sorts in Go over 55k int64s cost milliseconds, and the scan
// happens once.
//
// # Why the trend is by UPDATE year and says so
//
// The mirror has no first-published column; `update_date` is when a work was last
// touched. A trend by update year therefore measures activity, not creation, and
// the page says "last updated in" rather than "published in" — the difference
// matters for an old fic that was edited last month.

import (
	"context"
	"fmt"
	"sort"
)

// YearCount is one bar of the trend.
type YearCount struct {
	Year  string `json:"year"`
	Works int    `json:"works"`
	// Share is this year's works as a fraction of the busiest year in the
	// window, so a template can size a bar without doing arithmetic on
	// values it cannot see.
	Share float64 `json:"share"`
}

// TopWork is the most-bookmarked work in the shelf.
type TopWork struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Bookmarks int64  `json:"bookmarks"`
}

// TagShelf is the statistical summary of everything carrying one tag.
type TagShelf struct {
	Works       int   `json:"works"`
	TotalWords  int64 `json:"total_words"`
	MedianWords int64 `json:"median_words"`
	MeanWords   int64 `json:"mean_words"`
	MedianKudos int64 `json:"median_kudos"`
	MeanKudos   int64 `json:"mean_kudos"`
	// ShrunkKudos is the tag's mean kudos pulled toward the corpus mean by
	// priorPseudoWorks pseudo-works (#39). On a tag with three works it is a
	// sentence about the corpus with a little of the tag in it; on a tag with
	// thousands it is the tag's own mean. Both facts are useful and only one of
	// them is `mean kudos`.
	ShrunkKudos float64 `json:"shrunk_kudos"`
	// PriorKudos and PriorWeight are the prior made visible: a shrunk number
	// whose prior is hidden cannot be checked or argued with.
	PriorKudos  float64 `json:"prior_kudos"`
	PriorWeight int     `json:"prior_weight"`
	// Undated is how many works carry no update date. A trend that silently
	// drops them reads as a complete history when it is not.
	Undated int     `json:"undated"`
	Top     TopWork `json:"top_work,omitempty"`
	// Years is most recent first, at most trendYears entries.
	Years []YearCount `json:"years,omitempty"`
}

// trendYears bounds the trend. Twelve is enough to show a shape and few enough
// that the numbers do not become the page.
const trendYears = 12

// TagShelf computes the summary for one tag in a single pass over its works.
//
// The query starts from `SELECT DISTINCT work_id FROM work_tags WHERE tag_id`.
// `work_tags` is keyed by (work_id, tag_id, tag_type), so a work can carry the
// same tag under two types — the live mirror's duplicate-name shape, reproduced by
// the fixture — and a plain join counts that work twice, inflating every number on
// the page by one per duplicate. Measured on the fixture: 43 works where there are
// 42.
func (a *AO3) TagShelf(ctx context.Context, tagID int64) (TagShelf, error) {
	var s TagShelf

	rows, err := a.DB.QueryContext(ctx, `
		WITH t AS (SELECT DISTINCT work_id FROM work_tags WHERE tag_id = ?)
		SELECT w.id, w.title, COALESCE(w.word_count,0), COALESCE(w.kudos,0),
		       w.bookmarks, COALESCE(w.update_date,'')
		FROM t JOIN works w ON w.id = t.work_id`, tagID)
	if err != nil {
		return s, fmt.Errorf("tag %d shelf: %w", tagID, err)
	}
	defer rows.Close()

	words := make([]int64, 0, 1024)
	kudos := make([]int64, 0, 1024)
	byYear := make(map[string]int, 32)
	// Only the best-so-far travels with its title, so the shelf never holds 55k
	// titles just to print one of them.
	var top TopWork
	topBM := int64(-1)
	var totalWords int64

	for rows.Next() {
		var id int64
		var title string
		var wc, kd int64
		var bm *int64
		var updated string
		if err := rows.Scan(&id, &title, &wc, &kd, &bm, &updated); err != nil {
			return s, err
		}
		words = append(words, wc)
		kudos = append(kudos, kd)
		totalWords += wc
		s.Works++
		if bm != nil && *bm > topBM {
			topBM = *bm
			top = TopWork{ID: id, Title: title, Bookmarks: *bm}
		}
		if y := updateYear(updated); y == "" {
			s.Undated++
		} else {
			byYear[y]++
		}
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	if s.Works == 0 {
		return s, nil
	}

	s.TotalWords = totalWords
	s.MeanWords = totalWords / int64(s.Works)
	s.MedianWords = medianOf(words)
	var sumKudos int64
	for _, k := range kudos {
		sumKudos += k
	}
	s.MeanKudos = sumKudos / int64(s.Works)
	s.MedianKudos = medianOf(kudos)
	// The shrunk mean. A failure to read the prior is NOT fatal: the shelf's
	// other numbers are already computed and a page about a tag must not fail
	// because the corpus mean could not be read. ShrunkKudos stays 0 and
	// PriorWeight keeps its zero value, which the template reads as "not
	// measured" rather than printing 0 as an estimate.
	if prior, perr := a.CorpusMeanKudos(ctx); perr == nil && prior > 0 {
		s.PriorKudos = prior
		s.PriorWeight = priorPseudoWorks
		s.ShrunkKudos = ShrinkKudos(float64(sumKudos), s.Works, prior)
	}
	if topBM >= 0 {
		s.Top = top
	}

	// The trend, most recent year first, capped. The busiest year inside the
	// window sets the bar scale, so a quiet recent year is visibly quiet.
	for y, n := range byYear {
		s.Years = append(s.Years, YearCount{Year: y, Works: n})
	}
	sort.Slice(s.Years, func(i, j int) bool { return s.Years[i].Year > s.Years[j].Year })
	if len(s.Years) > trendYears {
		s.Years = s.Years[:trendYears]
	}
	busiest := 0
	for _, y := range s.Years {
		if y.Works > busiest {
			busiest = y.Works
		}
	}
	if busiest > 0 {
		for i := range s.Years {
			s.Years[i].Share = float64(s.Years[i].Works) / float64(busiest)
		}
	}
	return s, nil
}

// medianOf returns the middle value, sorting in place.
//
// The upper of the two middles for an even count: SQLite has no median aggregate,
// and a half-work difference between conventions is not something a reader can act
// on. An empty slice returns 0, which the caller only prints when there are works
// at all.
func medianOf(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[len(v)/2]
}

// updateYear extracts a four-digit year from either shape the corpus holds:
// AO3's "17 Sep 2008" and ISO "2008-09-17".
//
// It deliberately does NOT reimplement the month table used by
// ChronologicalUpdateDateDesc: a year is the last four characters of one form and
// the first four of the other, and a second month CASE would be a second place for
// the two orderings to disagree.
func updateYear(s string) string {
	if s == "" {
		return ""
	}
	if len(s) >= 10 && s[4] == '-' {
		if isYearDigits(s[:4]) {
			return s[:4]
		}
		return ""
	}
	if len(s) >= 4 && isYearDigits(s[len(s)-4:]) {
		return s[len(s)-4:]
	}
	return ""
}

func isYearDigits(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
