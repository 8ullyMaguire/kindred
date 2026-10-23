package web

// "Works you keep skipping" (#60).
//
// The surface exists because the impression log already knows something the
// reader does not: which works have been put in front of them three or more times
// with nothing happening. That is only useful if the reader can act on it, so
// each row carries the two actions that mean something here — "not interested"
// and "I read it" — rather than a list ending in an explanation.
//
// # Why "not interested" is negative feedback and not a hide table
//
// A hide would remove the work from one list and teach nothing. Polarity -1 goes
// through `LearnFromFeedback`, which moves this reader's weights on every tag the
// work carries — the one mechanism in the tree that can make a work score LOWER
// rather than merely absent. Whether that is enough to actually demote it is a
// measurement question, and the page says so instead of promising it.

import (
	"net/http"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// renderSkipped draws the list, and handles the two POST actions.
func (d Deps) renderSkipped(w http.ResponseWriter, r *http.Request) {
	base := d.baseOf(r, "Works you keep skipping", "Works you keep skipping")
	page := SkippedPage{Base: base, Threshold: skippedThreshold}
	if !d.hasStore() {
		page.Err = "This site has no state database, so there is no impression log to read."
		d.page(w, "skipped.html", page, http.StatusOK)
		return
	}
	_, ownerKey, err := d.arenaSession(r.Context(), w, r)
	if err != nil || ownerKey == "" {
		page.Err = "This list is per reader, and a reader session could not be established."
		d.page(w, "skipped.html", page, http.StatusOK)
		return
	}

	if r.Method == http.MethodPost {
		raw := r.PostFormValue("work_id")
		workID, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || workID <= 0 {
			page.Err = "work_id must be a positive number"
		} else {
			switch r.PostFormValue("action") {
			case "not_interested":
				// Polarity -1: the stored stance AND the tag weights, through the
				// same path the like button uses, so a reader's taste stays one
				// set of numbers rather than two adjusted separately.
				if err := d.Engine.Store.LearnFromFeedback(r.Context(), ownerKey, workID, -1); err != nil {
					page.Err = "Could not record that: " + err.Error()
				} else {
					page.Saved = "Noted. This work's tags now count against you, and it will not be " +
						"suggested again from this list."
				}
			case "read":
				if err := d.Engine.Store.MarkRead(r.Context(), ownerKey, store.WorkEntityID(workID)); err != nil {
					page.Err = "Could not record that: " + err.Error()
				} else {
					page.Saved = "Marked read."
				}
			default:
				page.Err = "action must be not_interested or read"
			}
		}
	}

	works, err := d.Engine.Store.SkippedWorks(r.Context(), ownerKey, skippedThreshold, 25)
	if err != nil {
		page.Err = "The impression log could not be read: " + err.Error()
	}
	// "Not interested" removes a work from this list by making it a negative
	// stance, so filter those out rather than showing a row whose button the
	// reader has already pressed.
	if len(works) > 0 {
		if stances, serr := d.Engine.Store.FeedbackForWorks(r.Context(), ownerKey, workIDsOf(works)); serr == nil {
			kept := works[:0]
			for _, wk := range works {
				if stances[wk.WorkID] >= 0 {
					kept = append(kept, wk)
				}
			}
			works = kept
		}
	}
	page.Works = d.decorateSkipped(r, works)
	if len(page.Works) == 0 && page.Err == "" {
		page.Empty = "Nothing yet. A work has to be shown " + strconv.Itoa(skippedThreshold) +
			" separate times with nothing happening before it counts as skipped, and this " +
			"mirror only records impressions for lists it renders itself."
	}
	d.page(w, "skipped.html", page, http.StatusOK)
}

// skippedThreshold is how many separate showings make "you keep skipping this" a
// claim rather than a coincidence.
const skippedThreshold = 3

// decorateSkipped attaches titles and URLs from the corpus, which the state
// database cannot supply.
func (d Deps) decorateSkipped(r *http.Request, rows []store.SkippedWork) []SkippedWorkView {
	out := make([]SkippedWorkView, 0, len(rows))
	labels, _ := d.workLabels(r, workIDsOf(rows))
	for _, row := range rows {
		out = append(out, SkippedWorkView{
			ID: row.WorkID, Shows: row.Shows, LastShown: row.LastShown,
			Arms: strings.Join(row.Arms, ", "), Title: labels[row.WorkID].title,
			URL: labels[row.WorkID].url,
		})
	}
	return out
}

func workIDsOf(rows []store.SkippedWork) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.WorkID)
	}
	return out
}

// There is deliberately no JSON twin of this list. The web package renders
// HTML and the API package does not import it, so an endpoint here would mean
// either a second implementation of the query or a layering exception for one
// page. /new made the same call for the same reason.

type workTitle struct{ title, url string }

// workLabels reads titles for a handful of ids. Missing ids are simply absent
// from the map: a work deleted from the corpus between the impression and the
// page is a row the page can still show by id.
func (d Deps) workLabels(r *http.Request, ids []int64) (map[int64]workTitle, error) {
	out := map[int64]workTitle{}
	if d.Engine == nil || d.Engine.Corpus == nil || len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	marks := make([]string, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
		marks = append(marks, "?")
	}
	rows, err := d.Engine.Corpus.DB.QueryContext(r.Context(),
		`SELECT id, title, COALESCE(url,'') FROM works WHERE id IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var t workTitle
		if err := rows.Scan(&id, &t.title, &t.url); err != nil {
			return out, err
		}
		out[id] = t
	}
	return out, rows.Err()
}
