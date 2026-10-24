package corpusquery

// TagTrend: is a tag rising or falling, measured against the corpus's own growth
// (#31).
//
// The comparison the row names -- "90-day vs corpus average" -- is the only honest
// one available. A tag with more works this quarter than last could be a rising trope
// or a corpus that grew; dividing by the corpus's own recent share cancels the second
// explanation and leaves the first.
//
// THE ANCHOR IS THE MIRROR'S NEWEST WORK, NOT TODAY. This mirror's newest update_date
// is 2025-10-31. A window measured back from the wall clock would contain nothing, and
// every tag in the archive would report a collapse that is really a stale snapshot --
// one wrong number per tag, all pointing the same way, which reads as a finding. The
// anchor is named in the result's notes so a reader can see how old the mirror is.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// tagTrendDays is the window the row names. A constant because a second window is a
// different question, not a tuning knob, and any caller that wants one should say so
// with a field rather than a magic number.
const tagTrendDays = 90

// dayExpr normalises works.update_date to YYYY-MM-DD.
//
// THIS MIRROR STORES TWO DATE SHAPES: 112,893 rows read "05 Sep 2007" and two read
// "2006-01-02". Comparing either shape as a raw string is a silent wrong answer --
// "31 Oct 2025" >= "2025-08-02" is TRUE lexically because '3' > '2', so a window
// comparison against a display date would put a decade-old work inside it. The
// expression makes both shapes comparable, and the tests carry a display-format corpus
// because the first version of this code parsed only ISO and passed its own tests.
//
// Rows with an empty date normalise to NULL, which no comparison matches: an undated
// work is neither in the window nor outside it, and counting it as outside would make
// the corpus's growth look slower than it is.
const dayExpr = `CASE
		WHEN w.update_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]' THEN w.update_date
		WHEN w.update_date GLOB '[0-9][0-9] [A-Z][a-z][a-z] [0-9][0-9][0-9][0-9]' THEN
			substr(w.update_date, 8, 4) || '-' ||
			printf('%02d',
				(instr('JanFebMarAprMayJunJulAugSepOctNovDec',
					substr(w.update_date, 4, 3)) - 1) / 3 + 1) || '-' ||
			substr(w.update_date, 1, 2)
	END`

// tagTrendMinWindowWorks is the fewest works a window must hold before a lift is
// computed. Below it the corpus's growth is a handful of rows and every ratio is noise
// wearing two decimal places.
const tagTrendMinWindowWorks = 200

// tagTrendRowLimit is how many rising pairs a default query returns.
const tagTrendRowLimit = 25

// TagTrend compares a tag's co-occurrences in the mirror's last 90 days with the same
// pairs over the whole mirror.
//
// Rows are the tags that travel WITH the seed, ranked by how much more often that
// pairing happens recently than the corpus's own growth explains. A row's Lift is
//
//	(co-occurrences in the window / co-occurrences overall) / (works in the window / works overall)
//
// so 1.0 means "grew exactly as fast as the corpus", 2.0 means "twice as fast" and 0.5
// means "half as fast". Works is the window's count and CoWorks the lifetime count, so
// every row can be checked by hand.
func (r *Runner) TagTrend(ctx context.Context, tag string, opts Options) (Result, error) {
	res := Result{Mode: string(ModeTagTrend)}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return res, fmt.Errorf("tag-trend: a tag is required")
	}
	if opts.Limit <= 0 {
		opts.Limit = tagTrendRowLimit
	}
	if opts.MinCoWorks == 0 {
		// The same gate the other runners use: a pairing seen twice is an anecdote,
		// and a lift computed from two works is the corpus's growth read through noise.
		opts.MinCoWorks = DefaultMinCoWorksToRank
	}

	// The anchor and the corpus's own counts, in one query.
	var anchor sql.NullString
	if err := r.DB.QueryRowContext(ctx, `SELECT MAX(`+dayExpr+`) FROM works w
		WHERE `+dayExpr+` IS NOT NULL`).Scan(&anchor); err != nil {
		return res, fmt.Errorf("tag-trend: anchor: %w", err)
	}
	if !anchor.Valid || anchor.String == "" {
		res.Notes = append(res.Notes, "this mirror carries no update dates, so no window can be measured")
		return res, nil
	}
	anchorDay, err := time.Parse("2006-01-02", anchor.String)
	if err != nil {
		return res, fmt.Errorf("tag-trend: anchor %q is not a date: %w", anchor.String, err)
	}

	// The floor scales with the corpus, because a fixture corpus of forty works cannot
	// hold two hundred: a window must hold at least a tenth of the archive, capped at
	// the absolute floor, and never fewer than five.
	var totalAll, totalRecent int
	if err := r.DB.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN `+dayExpr+` IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM works w`).Scan(&totalAll); err != nil {
		return res, fmt.Errorf("tag-trend: corpus size: %w", err)
	}
	floor := tagTrendMinWindowWorks
	if tenth := totalAll / 10; tenth < floor {
		floor = tenth
	}
	if floor < 5 {
		floor = 5
	}

	// The window widens until it holds enough works to measure. This mirror's newest
	// date is a single crawled row from 2026-10-15 while the body of the archive ends in
	// 2025, so a strict 90-day window held TWO works out of 112,895 and every pair's
	// lift came out 0.00 -- a ranking of zeros that reads as "nothing is rising" and is
	// really "nothing was measured". Widening is said out loud; going below the floor is
	// refused rather than guessed.
	var startDay string
	usedDays := 0
	for _, days := range []int{tagTrendDays, 180, 365, 730, 1460, 3650} {
		candidate := anchorDay.AddDate(0, 0, -days).Format("2006-01-02")
		if err := r.DB.QueryRowContext(ctx, `SELECT
				COALESCE(SUM(CASE WHEN `+dayExpr+` >= ? THEN 1 ELSE 0 END), 0)
			FROM works w`, candidate).Scan(&totalRecent); err != nil {
			return res, fmt.Errorf("tag-trend: corpus counts: %w", err)
		}
		startDay, usedDays = candidate, days
		if totalRecent >= floor {
			break
		}
	}
	res.Notes = append(res.Notes, fmt.Sprintf(
		"window %s..%s (%d days, %d of %d works) anchored on the mirror's newest work, "+
			"not today", startDay, anchor.String, usedDays, totalRecent, totalAll))
	if usedDays != tagTrendDays {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"the window was widened from %d to %d days: %d of %d works fall inside the "+
				"shorter one, which cannot measure a trend", tagTrendDays, usedDays,
			totalRecent, totalAll))
	}
	if totalRecent < floor || totalAll == 0 {
		// Naming this instead of returning zero lifts: a window with nothing in it is
		// a fact about the snapshot, and every row would be a division by zero dressed
		// as a ranking.
		res.Notes = append(res.Notes, fmt.Sprintf(
			"only %d of %d works fall inside even a %d-day window, which is too few to "+
				"measure a trend against", totalRecent, totalAll, usedDays))
		return res, nil
	}

	var id, tagWorks int64
	err = r.DB.QueryRowContext(ctx,
		`SELECT t.id, COUNT(wt.work_id) FROM tags t
		 LEFT JOIN work_tags wt ON wt.tag_id = t.id
		 WHERE t.name = ? GROUP BY t.id`, tag).Scan(&id, &tagWorks)
	if err == sql.ErrNoRows {
		res.Notes = append(res.Notes, fmt.Sprintf("no tag named %q in this mirror", tag))
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("tag-trend: resolve %q: %w", tag, err)
	}
	if id == 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("no tag named %q in this mirror", tag))
		return res, nil
	}

	// Co-occurrences of the seed with every other tag, split by window. The seed's own
	// row is excluded: a tag always co-occurs with itself, and its "trend" is just the
	// corpus's.
	rows, err := r.DB.QueryContext(ctx, `
		SELECT t.name,
		       COUNT(*) AS all_works,
		       COALESCE(SUM(CASE WHEN `+dayExpr+` >= ? THEN 1 ELSE 0 END), 0) AS recent_works
		FROM work_tags a
		JOIN work_tags b ON b.work_id = a.work_id AND b.tag_id <> a.tag_id
		JOIN tags t ON t.id = b.tag_id
		JOIN works w ON w.id = a.work_id
		WHERE a.tag_id = ?
		GROUP BY b.tag_id
		HAVING all_works >= ?
		ORDER BY all_works DESC`, startDay, id, opts.MinCoWorks)
	if err != nil {
		return res, fmt.Errorf("tag-trend: co-occurrences: %w", err)
	}
	defer rows.Close()

	corpusGrowth := float64(totalRecent) / float64(totalAll)
	for rows.Next() {
		var name string
		var all, recent int
		if err := rows.Scan(&name, &all, &recent); err != nil {
			return res, fmt.Errorf("tag-trend: scan: %w", err)
		}
		if all == 0 {
			continue
		}
		// The window's co-occurrence share over the lifetime share, divided by the
		// corpus's own growth. Both sides use the same pair count, so a tag's
		// popularity cancels and only the CHANGE survives.
		pairGrowth := float64(recent) / float64(all)
		lift := 0.0
		if pairGrowth > 0 {
			lift = pairGrowth / corpusGrowth
		}
		res.Rows = append(res.Rows, Row{
			Key:     name,
			Label:   name,
			Score:   lift,
			Lift:    lift,
			Works:   recent,
			CoWorks: all,
			Note: fmt.Sprintf("%d of %d works in the window (corpus grew %.2fx)",
				recent, all, corpusGrowth),
		})
	}
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("tag-trend: rows: %w", err)
	}

	// Ranked by lift, but ties broken by the lifetime count so a pair with 40 works is
	// not shuffled behind one with 3 by float noise.
	sort.SliceStable(res.Rows, func(i, j int) bool {
		if res.Rows[i].Lift != res.Rows[j].Lift {
			return res.Rows[i].Lift > res.Rows[j].Lift
		}
		return res.Rows[i].CoWorks > res.Rows[j].CoWorks
	})
	if len(res.Rows) > opts.Limit {
		res.Rows = res.Rows[:opts.Limit]
		res.Truncated = true
	}
	res.Notes = append(res.Notes, fmt.Sprintf(
		"lift = the pair's growth in the window divided by the corpus's %.2fx growth; "+
			"1.0 is \"as fast as the corpus\"", corpusGrowth))
	return res, nil
}
