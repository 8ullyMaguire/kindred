package tagseason

// Which tags belong to which month (#80).
//
// AO3 records an update date per work in its own display format ("02 Jan 2006"), and 112,895 of
// the mirror's 112,937 works have one. That is enough to ask a question the archive has never
// been asked: is a tag's use concentrated in a calendar month, consistently across YEARS?
//
// Two things make this more than a count:
//
//   - LIFT, not volume. December has more works than June for reasons that have nothing to do
//     with Christmas, so a tag's raw monthly count mostly measures how much the archive was
//     posted that month. Every number here is a share of that month's works divided by the tag's
//     share of the whole archive.
//   - CONSISTENCY across years. "Christmas" spiking in December 2023 and nowhere else could be
//     one author's series. A tag is seasonal only if its peak month is the same in most years it
//     appears in, which no single-year spike can fake.
//
// This is DETECTION. #44's calendar TERM -- using the model to move a ranking -- is not built
// here and cannot be validated offline: every evaluation runs at one instant, so a term that
// depends on "today's month" has no holdout to be measured on. See the handoff.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Opts bounds what counts as seasonal.
type Opts struct {
	// MinSupport is how many dated works must carry the tag at all. Below a few hundred, a
	// month's share is one author's posting habit.
	MinSupport int
	// MinMonthWorks is the fewest works in the peak month for the lift to mean anything.
	MinMonthWorks int
	// MinLift is how much more often the tag appears in its peak month than in an average one.
	MinLift float64
	// MinYearShare is the fraction of the tag's years in which the peak month is the SAME month.
	MinYearShare float64
	// MinYearWorks is how many works of the tag a year needs to vote. A year with one work is
	// not evidence about a month.
	MinYearWorks int
	// MinYears is how many years must have voted at all. WITHOUT THIS a tag that exists only in
	// one year is "100% consistent": measured on the live archive, "project hail mary (2026)"
	// came back as a MAY tag with 1/1 years agreeing, because every work carrying it was posted
	// in one month of one year. That is a tag going VIRAL, not a seasonal one, and the two look
	// identical to a per-year test.
	MinYears int
	// KeepPerMonth caps the output.
	KeepPerMonth int
}

// DefaultOpts is what `kindred seasons` runs with.
func DefaultOpts() Opts {
	return Opts{
		// Measured on the live archive: at 300 support only "christmas" survives; at 80 the
		// calendar appears (christmas, christmas smut, christmas fluff, valentine's day,
		// halloween) and so does the noise the consistency floor is for (a show's characters
		// spiking in the month its episodes aired, 2 of 3 years). 0.75 keeps the calendar and
		// drops the airing schedule.
		MinSupport:    80,
		MinMonthWorks: 20,
		MinLift:       1.5,
		MinYearShare:  0.75,
		MinYearWorks:  3,
		MinYears:      3,
		KeepPerMonth:  25,
	}
}

// Season is one tag's claim on one month.
type Season struct {
	TagID int64
	// Month is 1..12.
	Month int
	// Lift is the tag's share of the month's works over its share of the archive.
	Lift float64
	// Works is how many works carry the tag in that month, and Support how many in total.
	Works   int
	Support int
	// YearShare is the share of the tag's qualifying years whose peak month is Month.
	YearShare float64
	// Years is how many years voted.
	Years int
}

// Name is filled by a caller that has the tag names.
func (s Season) Label(name string) string {
	if name == "" {
		return fmt.Sprintf("tag %d", s.TagID)
	}
	return name
}

// MonthNames is the calendar, so no caller writes it twice.
var MonthNames = [...]string{"", "January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December"}

// ParseMonth accepts "12", "dec" or "december" (case-insensitive) and returns 1..12.
//
// A caller typing `--month december` is the obvious use, and refusing it because the flag is an
// int would be a worse interface than this function.
func ParseMonth(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("no month given")
	}
	for i := 1; i <= 12; i++ {
		name := strings.ToLower(MonthNames[i])
		if s == name || (len(s) >= 3 && strings.HasPrefix(name, s)) {
			return i, nil
		}
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n >= 1 && n <= 12 {
		return n, nil
	}
	return 0, fmt.Errorf("%q is not a month", s)
}

// Counts is the raw pass: per tag, works per month, and works per year-month.
//
// Year-month rather than year matters: the consistency test asks whether the tag's peak month is
// the same in each YEAR, so the month counts have to be separable by year.
type Counts struct {
	// ByMonth[tag][month] = works
	ByMonth map[int64][13]int
	// ByYear[tag][year][month] = works
	ByYear map[int64]map[int][13]int
	// MonthTotals[month] = dated works in that month, across all tags
	MonthTotals [13]int
	// Total is the number of dated works.
	Total int
	// Dated is how many works had a parseable date at all.
	Dated int
	// Undated is how many did not, so the report can say what it left out.
	Undated int
}

// LoadCounts reads the corpus once.
//
// The query is DISTINCT and ORDERED by work, because work_tags has (work, tag, TYPE) as its key
// and a tag can carry two types on one work: without the de-duplication a tag's December count
// doubles and its lift with it. Ordering by work lets the loop count each WORK once for the
// month denominators by watching the id change, instead of holding a 112k-entry map.
func LoadCounts(ctx context.Context, corpus *sql.DB) (*Counts, error) {
	if corpus == nil {
		return nil, fmt.Errorf("tagseason: a corpus database is required")
	}
	c := &Counts{
		ByMonth: map[int64][13]int{},
		ByYear:  map[int64]map[int][13]int{},
	}
	var totalWorks int
	if err := corpus.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&totalWorks); err != nil {
		return nil, fmt.Errorf("tagseason: count works: %w", err)
	}

	rows, err := corpus.QueryContext(ctx, `
		SELECT DISTINCT wt.work_id, wt.tag_id, w.update_date
		  FROM work_tags wt JOIN works w ON w.id = wt.work_id
		 WHERE w.update_date IS NOT NULL AND w.update_date <> ''
		 ORDER BY wt.work_id, wt.tag_id`)
	if err != nil {
		return nil, fmt.Errorf("tagseason: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var curWork int64 = -1
	curMonth := 0
	for rows.Next() {
		var workID, tagID int64
		var date string
		if err := rows.Scan(&workID, &tagID, &date); err != nil {
			return nil, fmt.Errorf("tagseason: scan: %w", err)
		}
		year, month, ok := parseAO3Date(date)
		if !ok {
			c.Undated++
			continue
		}
		if workID != curWork {
			curWork, curMonth = workID, month
			c.Total++
			c.MonthTotals[month]++
		}
		byM := c.ByMonth[tagID]
		byM[curMonth]++
		c.ByMonth[tagID] = byM
		if c.ByYear[tagID] == nil {
			c.ByYear[tagID] = map[int][13]int{}
		}
		byY := c.ByYear[tagID][year]
		byY[curMonth]++
		c.ByYear[tagID][year] = byY
		c.Dated++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tagseason: rows: %w", err)
	}
	// Works with no tag at all are neither dated nor undated for this model: the denominator is
	// the works that could carry a tag, and a work with none contributes to no tag's share.
	if c.Total+c.Undated < totalWorks {
		c.Undated += totalWorks - c.Total - c.Undated
	}
	return c, nil
}

// parseAO3Date reads "02 Jan 2006".
func parseAO3Date(s string) (year, month int, ok bool) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) < 3 {
		return 0, 0, false
	}
	mon := strings.ToLower(fields[1])
	for i := 1; i <= 12; i++ {
		if strings.HasPrefix(strings.ToLower(MonthNames[i]), mon) {
			month = i
			break
		}
	}
	if month == 0 {
		return 0, 0, false
	}
	y := 0
	for _, r := range fields[2] {
		if r < '0' || r > '9' {
			return 0, 0, false
		}
		y = y*10 + int(r-'0')
	}
	if y < 1900 || y > 2200 {
		return 0, 0, false
	}
	return y, month, true
}

// Build turns counts into the seasonal claims.
//
// The month totals come from the works the caller counted, not from a second query: the
// denominator has to be the same population as the numerator, or a tag can look seasonal because
// the two queries filtered differently.
func Build(c *Counts, o Opts) []Season {
	if c == nil || c.Total == 0 {
		// Totals are filled by the caller after LoadCounts; see FillTotals.
		return nil
	}
	var out []Season
	for tag, byMonth := range c.ByMonth {
		support := 0
		for m := 1; m <= 12; m++ {
			support += byMonth[m]
		}
		if support < o.MinSupport {
			continue
		}
		bestM, bestLift := 0, 0.0
		for m := 1; m <= 12; m++ {
			if byMonth[m] < o.MinMonthWorks {
				continue
			}
			monthShare := float64(byMonth[m]) / float64(c.MonthTotals[m])
			tagShare := float64(support) / float64(c.Total)
			lift := monthShare / tagShare
			if lift > bestLift {
				bestLift, bestM = lift, m
			}
		}
		if bestM == 0 || bestLift < o.MinLift {
			continue
		}
		share, years := consistency(c.ByYear[tag], bestM, o.MinYearWorks)
		if years < o.MinYears || share < o.MinYearShare {
			continue
		}
		out = append(out, Season{
			TagID: tag, Month: bestM, Lift: bestLift, Works: byMonth[bestM],
			Support: support, YearShare: share, Years: years,
		})
	}
	// Strongest first, then by tag id: a deterministic order, because two runs must agree.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Lift != out[b].Lift {
			return out[a].Lift > out[b].Lift
		}
		return out[a].TagID < out[b].TagID
	})
	return out
}

// consistency is the share of the tag's qualifying years whose peak month is `month`.
func consistency(byYear map[int][13]int, month, minWorks int) (share float64, years int) {
	if len(byYear) == 0 {
		return 0, 0
	}
	same := 0
	for _, months := range byYear {
		total := 0
		peak, peakN := 0, 0
		for m := 1; m <= 12; m++ {
			total += months[m]
			if months[m] > peakN {
				peak, peakN = m, months[m]
			}
		}
		if total < minWorks || peak == 0 {
			continue
		}
		years++
		if peak == month {
			same++
		}
	}
	if years == 0 {
		return 0, 0
	}
	return float64(same) / float64(years), years
}

// Keep takes the best `n` per month.
func Keep(all []Season, n int) map[int][]Season {
	out := map[int][]Season{}
	if n <= 0 {
		return out
	}
	for _, s := range all {
		// The input is sorted by lift, so appending preserves that.
		if len(out[s.Month]) < n {
			out[s.Month] = append(out[s.Month], s)
		}
	}
	return out
}
