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
// skew is visible rather than hidden.
//
// # Why the trend is by UPDATE year and says so
//
// The mirror has no first-published column; `update_date` is when a work was last
// touched. A trend by update year therefore measures activity, not creation, and
// the page says "last updated in" rather than "published in" — the difference
// matters for an old fic that was edited last month.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// chronologicalYear is the four-digit year of an update_date in either shape the
// corpus holds: AO3's "17 Sep 2008" and ISO "2008-09-17".
//
// It deliberately does NOT reimplement the month table: a year is the last four
// characters of one form and the first four of the other, and duplicating the
// month CASE here would be a second place for the two orderings to disagree.
const chronologicalYear = `
CASE
	WHEN w.update_date IS NULL OR w.update_date = '' THEN ''
	WHEN instr(w.update_date, ' ') > 0 THEN substr(w.update_date, -4)
	ELSE substr(w.update_date, 1, 4)
END`

// TagShelf computes the summary for one tag.
//
// Every query starts from `SELECT DISTINCT work_id FROM work_tags WHERE tag_id`,
// not from a plain join. `work_tags` is keyed by (work_id, tag_id, tag_type), so
// a work can carry the same tag under two types — the live mirror's duplicate-name
// shape and the fixture reproduce it — and a plain join counts that work twice,
// inflating every number on the page by one per duplicate. Measured on the
// fixture: 43 works where there are 42.
//
// Every query is keyed on work_tags.tag_id, which is indexed; the aggregates are
// three full passes over the tag's works and are the reason the tag page reports
// its own timing.
func (a *AO3) TagShelf(ctx context.Context, tagID int64) (TagShelf, error) {
	var s TagShelf

	// One pass for the scalars. `COUNT(w.word_count)` rather than COUNT(*) so a
	// NULL length does not count as a zero-length work in the mean.
	err := a.DB.QueryRowContext(ctx, `
		WITH t AS (SELECT DISTINCT work_id FROM work_tags WHERE tag_id = ?)
		SELECT COUNT(*),
		       COALESCE(SUM(w.word_count),0),
		       CAST(COALESCE(AVG(w.word_count),0) AS INTEGER),
		       CAST(COALESCE(AVG(COALESCE(w.kudos,0)),0) AS INTEGER),
		       COALESCE(SUM(CASE WHEN w.update_date IS NULL OR w.update_date = '' THEN 1 ELSE 0 END),0)
		FROM t JOIN works w ON w.id = t.work_id`, tagID).
		Scan(&s.Works, &s.TotalWords, &s.MeanWords, &s.MeanKudos, &s.Undated)
	if err != nil {
		return s, fmt.Errorf("tag %d shelf scalars: %w", tagID, err)
	}
	if s.Works == 0 {
		return s, nil
	}

	// Medians. SQLite has no median aggregate, so this is the middle row by
	// value — the standard LIMIT 1 OFFSET n/2 form, which is exact for an odd
	// count and the upper of the two middles for an even one. Saying "median"
	// about the upper middle is a half-work difference nobody reads as a lie.
	median := func(expr string) (int64, error) {
		var v sql.NullInt64
		q := `WITH t AS (SELECT DISTINCT work_id FROM work_tags WHERE tag_id = ?)
		      SELECT ` + expr + ` FROM t JOIN works w ON w.id = t.work_id
		      ORDER BY ` + expr + ` LIMIT 1 OFFSET ?`
		if err := a.DB.QueryRowContext(ctx, q, tagID, s.Works/2).Scan(&v); err != nil {
			return 0, err
		}
		if !v.Valid {
			return 0, nil
		}
		return v.Int64, nil
	}
	if s.MedianWords, err = median("COALESCE(w.word_count,0)"); err != nil {
		return s, fmt.Errorf("tag %d median words: %w", tagID, err)
	}
	if s.MedianKudos, err = median("COALESCE(w.kudos,0)"); err != nil {
		return s, fmt.Errorf("tag %d median kudos: %w", tagID, err)
	}

	// The single most-saved work. A NULL bookmarks column (the corpus has them)
	// must not win the ordering.
	//
	// The `IS NOT NULL` filter is DEFENSIVE and, measured on SQLite, redundant:
	// NULL sorts as smaller than every value, so `ORDER BY bookmarks DESC` puts
	// NULLs last by itself, and the mutant deleting the filter survives every
	// test — because it changes no row. It stays because the claim "most saved"
	// must not depend on a collation detail, and the test pins the OUTCOME (the
	// maximum wins) rather than the clause.
	var top TopWork
	var topBM sql.NullInt64
	err = a.DB.QueryRowContext(ctx, `
		WITH t AS (SELECT DISTINCT work_id FROM work_tags WHERE tag_id = ?)
		SELECT w.id, w.title, w.bookmarks
		FROM t JOIN works w ON w.id = t.work_id
		WHERE w.bookmarks IS NOT NULL
		ORDER BY w.bookmarks DESC, w.id LIMIT 1`, tagID).
		Scan(&top.ID, &top.Title, &topBM)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A tag whose works carry no bookmark counts at all: no claim.
	case err != nil:
		return s, fmt.Errorf("tag %d top work: %w", tagID, err)
	default:
		top.Bookmarks = topBM.Int64
		s.Top = top
	}

	// The trend, most recent year first.
	rows, err := a.DB.QueryContext(ctx, `
		WITH t AS (SELECT DISTINCT work_id FROM work_tags WHERE tag_id = ?)
		SELECT `+chronologicalYear+` AS yr, COUNT(*)
		FROM t JOIN works w ON w.id = t.work_id
		WHERE `+chronologicalYear+` <> ''
		GROUP BY yr ORDER BY yr DESC LIMIT ?`, tagID, trendYears)
	if err != nil {
		return s, fmt.Errorf("tag %d trend: %w", tagID, err)
	}
	defer rows.Close()
	var busiest int
	for rows.Next() {
		var y YearCount
		if err := rows.Scan(&y.Year, &y.Works); err != nil {
			return s, err
		}
		if y.Works > busiest {
			busiest = y.Works
		}
		s.Years = append(s.Years, y)
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	if busiest > 0 {
		for i := range s.Years {
			s.Years[i].Share = float64(s.Years[i].Works) / float64(busiest)
		}
	}
	return s, nil
}
