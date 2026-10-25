package harvest

// "Did the harvest store what the page says?"
//
// The harvest writes 20 works per request from a LISTING page, which is the only way 22,600 works
// is affordable -- and which means every stored row is a claim about a page nobody fetched. A
// report that says "22,614 saved" is a count of writes, not of correct writes. This compares a
// sample against the work page itself and separates two different things that both look like an
// error:
//
//   - a PARSE mismatch: the page says one thing and the blurb-derived row says another. This is
//     the harvest's bug, and it is systematic -- one bad regex is 22,600 wrong rows.
//   - a DRIFT: kudos, hits and bookmarks legitimately grow between the crawl and the check. A
//     comparison that calls that an error produces a report that is red every time and therefore
//     read by nobody.
//
// Only a zero-where-the-page-has-a-number counts as a parse failure for a counter: a stored 4,000
// against a live 4,050 is drift; a stored 0 against a live 4,050 is a column that never got filled.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

// StoredWork is a row as the corpus holds it.
type StoredWork struct {
	ID         int64
	Title      string
	Authors    string
	WordCount  int64
	Kudos      int64
	Bookmarks  int64
	Rating     string
	Complete   bool
	UpdateDate string
	Language   string
	Chapters   string
}

// QAResult is the comparison, by field.
type QAResult struct {
	Sampled     int
	Compared    int
	FetchFailed int
	ParseFailed int
	// Mismatch counts fields where the stored value disagrees with the page. Drift counts
	// counters that moved (an observation, not a defect).
	Mismatch map[string]int
	Drift    map[string]int
	// Examples names up to a handful of concrete disagreements, because a count does not tell
	// anyone which regex is wrong.
	Examples []string
}

// OK reports whether the sample found no systematic disagreement.
func (r *QAResult) OK() bool { return len(r.Mismatch) == 0 && r.Compared > 0 }

// QA compares each stored row against its work page.
func QA(ctx context.Context, f Fetcher, base string, rows []StoredWork) (*QAResult, error) {
	res := &QAResult{Mismatch: map[string]int{}, Drift: map[string]int{}}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Sampled++
		rawURL := fmt.Sprintf("%s/works/%d", base, row.ID)
		body, err := f.Fetch(ctx, rawURL)
		if err != nil {
			res.FetchFailed++
			continue
		}
		w, err := crawl.ParseWork(body, rawURL)
		if err != nil {
			res.ParseFailed++
			res.Examples = append(res.Examples, fmt.Sprintf("%d: page did not parse: %v", row.ID, err))
			continue
		}
		res.Compared++

		// note counts by FIELD, and puts the values in the example: a report keyed by the whole
		// sentence cannot be counted ("title (stored ...)" is a different key for every work), and
		// a report of counts alone does not say which regex is wrong.
		note := func(field, detail string) {
			res.Mismatch[field]++
			if len(res.Examples) < 10 {
				res.Examples = append(res.Examples, fmt.Sprintf("%d: %s — %s", row.ID, field, detail))
			}
		}
		if !sameText(row.Title, w.Title) {
			note("title", fmt.Sprintf("stored %q, page %q", trunc(row.Title, 40), trunc(w.Title, 40)))
		}
		if !sameText(row.Authors, w.Authors) {
			note("authors", fmt.Sprintf("stored %q, page %q", trunc(row.Authors, 30), trunc(w.Authors, 30)))
		}
		if row.WordCount != w.WordCount {
			// A word count can change when an author edits, but not by a factor: a zero or a
			// wildly different number is a parse failure, a small change is drift.
			if row.WordCount == 0 || factorOff(row.WordCount, w.WordCount) > 2 {
				note("word_count", fmt.Sprintf("stored %d, page %d", row.WordCount, w.WordCount))
			} else {
				res.Drift["word_count"]++
			}
		}
		if !sameText(row.Rating, w.Rating) {
			note("rating", fmt.Sprintf("stored %q, page %q", row.Rating, w.Rating))
		}
		if row.Complete != w.Complete {
			note("complete", fmt.Sprintf("stored %v, page %v", row.Complete, w.Complete))
		}
		if row.Language != "" && w.Language != "" && !sameText(row.Language, w.Language) {
			note("language", fmt.Sprintf("stored %q, page %q", row.Language, w.Language))
		}
		// Counters: zero is a defect, movement is life.
		if row.Kudos == 0 && w.Kudos > 0 {
			note("kudos", fmt.Sprintf("stored 0, page %d", w.Kudos))
		} else if row.Kudos != w.Kudos {
			res.Drift["kudos"]++
		}
		if row.Bookmarks != 0 && w.Bookmarks != nil && row.Bookmarks != *w.Bookmarks {
			res.Drift["bookmarks"]++
		}
		if row.UpdateDate != "" && w.UpdateDate != "" && !sameDate(row.UpdateDate, w.UpdateDate) {
			note("update_date", fmt.Sprintf("stored %q, page %q", row.UpdateDate, w.UpdateDate))
		}
	}
	return res, nil
}

// sameText compares two display strings the way a reader would: case and spacing are not the
// question, the words are.
func sameText(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// sameDate accepts the two spellings the corpus and the page use for the same day.
func sameDate(a, b string) bool {
	x, okx := parseAnyDate(a)
	y, oky := parseAnyDate(b)
	if okx && oky {
		return x.Equal(y)
	}
	return sameText(a, b)
}

// parseAnyDate reads the spellings AO3 and this corpus use for a day.
func parseAnyDate(s string) (t time.Time, ok bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "2 Jan 2006", "02 Jan 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func factorOff(a, b int64) float64 {
	if a <= 0 || b <= 0 {
		if a == b {
			return 1
		}
		return 1e9
	}
	f := float64(a) / float64(b)
	if f < 1 {
		f = 1 / f
	}
	return f
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
