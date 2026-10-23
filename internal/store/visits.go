package store

// Visit cursors and the "new since your last visit" list (#65).
//
// The roadmap row says it precisely: seen_works is a SET, and a set has no time
// order, so nothing in this tree could answer "what arrived since I last
// looked". The list it needs is not "things I have not seen" (that is every work
// in the mirror minus mine) but "things that appeared while I was away" — which
// needs two instants per reader and a comparison against the corpus's own
// first_seen.
//
// Three decisions:
//
//   - **Two instants, not a visit log.** The product asks about the previous
//     visit and nothing older. A log would grow forever to answer a question
//     about its own last two rows.
//
//   - **The gap is a parameter.** "Away" is a product decision — thirty minutes
//     is a sitting, six hours is a day's break — and it belongs where the caller
//     can reason about it, not inside a schema trigger.
//
//   - **A first visit has no answer, and says so.** There is no "before" to
//     compare against, so the surface must not invent one: an empty list with
//     `FirstVisit` set is the honest answer, and it is different from "nothing
//     new since yesterday", which is a claim about the corpus.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultVisitGap is how long away counts as a new visit.
//
// Thirty minutes: shorter and a reader paging between tabs gets a new visit on
// every request, longer and coming back after lunch reads as the same sitting.
const DefaultVisitGap = 30 * time.Minute

// VisitCursor is where a reader's visits stand.
type VisitCursor struct {
	// PreviousAt is when the visit BEFORE this one began, and is the boundary
	// the new-works list is measured from. Zero on a first visit.
	PreviousAt time.Time
	// CurrentAt is when this visit began.
	CurrentAt time.Time
	// Visits counts how many separate visits this reader has made.
	Visits int
	// FirstVisit is true when there is no previous visit to compare against.
	// It is a separate field rather than PreviousAt.IsZero() so a caller cannot
	// get the distinction wrong twice.
	FirstVisit bool
}

// NoteVisit records that the reader is here, rolling the cursor when the gap
// since their last request has elapsed.
//
// `now` is a parameter rather than time.Now() so the roll can be tested at all:
// a function that reads the clock itself can only be tested by waiting.
func (s *Store) NoteVisit(ctx context.Context, ownerKey string, now time.Time, gap time.Duration) (VisitCursor, error) {
	var cur VisitCursor
	if s == nil || s.DB == nil || ownerKey == "" {
		return VisitCursor{CurrentAt: now, FirstVisit: true, Visits: 1}, nil
	}
	if gap <= 0 {
		gap = DefaultVisitGap
	}
	nowS := now.UTC().Format(time.RFC3339)

	var prev, current, last string
	var visits int
	err := s.DB.QueryRowContext(ctx,
		`SELECT previous_at, current_at, last_at, visits FROM reader_visits WHERE owner_key = ?`,
		ownerKey).Scan(&prev, &current, &last, &visits)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A brand-new reader: today is both the current visit and the only one
		// that has ever happened.
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO reader_visits (owner_key, previous_at, current_at, last_at, visits)
			 VALUES (?, ?, ?, ?, 1)`, ownerKey, nowS, nowS, nowS); err != nil {
			return cur, fmt.Errorf("record first visit: %w", err)
		}
		return VisitCursor{PreviousAt: now, CurrentAt: now, Visits: 1, FirstVisit: true}, nil
	case err != nil:
		return cur, fmt.Errorf("read visit cursor: %w", err)
	}

	curTime, perr := time.Parse(time.RFC3339, current)
	prevTime, qerr := time.Parse(time.RFC3339, prev)
	lastTime, lerr := time.Parse(time.RFC3339, last)
	if lerr != nil {
		perr = lerr
	}
	if perr != nil || qerr != nil {
		// A cursor written by an older format is not worth guessing about: reset
		// it to now, which costs the reader one visit's worth of "new" rather
		// than showing them a list measured from an unparseable instant.
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE reader_visits SET previous_at = ?, current_at = ?, last_at = ?, visits = 1
			 WHERE owner_key = ?`, nowS, nowS, nowS, ownerKey); err != nil {
			return cur, fmt.Errorf("reset visit cursor: %w", err)
		}
		return VisitCursor{PreviousAt: now, CurrentAt: now, Visits: 1, FirstVisit: true}, nil
	}

	if now.Sub(lastTime) < gap {
		// Same visit continuing: current_at stays at the visit's START, because a
		// visit that keeps moving its own start is a visit whose boundary drifts,
		// and the boundary is what the next visit's list is measured from. Only
		// last_at moves.
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE reader_visits SET last_at = ? WHERE owner_key = ?`,
			nowS, ownerKey); err != nil {
			return cur, fmt.Errorf("extend visit: %w", err)
		}
		return VisitCursor{PreviousAt: prevTime, CurrentAt: curTime, Visits: visits}, nil
	}

	// A new visit: the previous one becomes the boundary.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE reader_visits SET previous_at = current_at, current_at = ?, last_at = ?,
		        visits = visits + 1
		 WHERE owner_key = ?`, nowS, nowS, ownerKey); err != nil {
		return cur, fmt.Errorf("roll visit: %w", err)
	}
	return VisitCursor{PreviousAt: curTime, CurrentAt: now, Visits: visits + 1}, nil
}

// NewWork is one row of the "new since" list: the work, and when the mirror
// first saw it — the second field is what makes the list checkable.
type NewWork struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Authors   string `json:"authors,omitempty"`
	URL       string `json:"url,omitempty"`
	FirstSeen string `json:"first_seen"`
}

// NewSince lists works the mirror first saw after `since`, minus anything this
// reader has already met.
//
// "Already met" means seen_works, both halves: a work they READ and a work they
// were SHOWN. A list that re-offers what the reader scrolled past is the exact
// failure this surface exists to avoid.
func (s *Store) NewSince(ctx context.Context, ownerKey string, since time.Time, limit int) ([]NewWork, error) {
	if s == nil || s.Corpus == nil {
		return nil, errors.New("no corpus is attached")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	args := []any{since.UTC().Format(time.RFC3339)}
	// The `IS NOT NULL AND <> ''` guard is DEFENSIVE and, measured, redundant:
	// datetime('') is NULL and NULL > datetime(?) is NULL, so an undated work is
	// already excluded by the comparison alone. A mutation deleting the guard
	// therefore leaves every test green — the redundancy is deliberate (a corpus
	// with a different ''-handling would still be safe) and the test pins the
	// OUTCOME, which is that a work with no arrival date never appears.
	q := `SELECT w.id, w.title, w.authors, w.url, w.first_seen
	      FROM works w
	      WHERE w.first_seen IS NOT NULL AND w.first_seen <> ''
	        AND datetime(w.first_seen) > datetime(?)`

	// seen_works lives in the STATE database and works in the CORPUS one, and
	// SQLite cannot join across two files without ATTACH. So the exclusion is
	// read first and bound as an IN list, in chunks of 400 — the marker limit the
	// rest of this tree respects. Joining would have been shorter to write and
	// impossible to run.
	if ownerKey != "" {
		seen, err := s.seenEntityIDs(ctx, ownerKey)
		if err != nil {
			return nil, err
		}
		for start := 0; start < len(seen); start += 400 {
			end := start + 400
			if end > len(seen) {
				end = len(seen)
			}
			q += ` AND w.id NOT IN (` + placeholders(end-start) + `)`
			for _, id := range seen[start:end] {
				args = append(args, id)
			}
		}
	}
	q += ` ORDER BY datetime(w.first_seen) DESC, w.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.Corpus.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NewWork, 0, limit)
	for rows.Next() {
		var w NewWork
		if err := rows.Scan(&w.ID, &w.Title, &w.Authors, &w.URL, &w.FirstSeen); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// seenEntityIDs is every work this reader has met, read OR shown.
//
// Both halves: a work they scrolled past was still put in front of them, and
// re-offering it is the failure the "new since" list exists to avoid. Ids that
// do not parse are skipped rather than failing the list — seen_works has no
// constraint on entity_id, so one legacy row must not empty the page.
func (s *Store) seenEntityIDs(ctx context.Context, ownerKey string) ([]int64, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT entity_id FROM seen_works WHERE owner_key = ?`, ownerKey)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0, 64)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, ok := EntityNumericID(raw)
		if !ok {
			continue
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// WorkEntityID is the form `seen_works` stores for a work.
//
// The prefix is not decoration: the column holds several kinds, and a bare
// number would collide with a tag id of the same value. Every reader of the
// table therefore has to strip it, which is why these two helpers live here
// rather than being inlined at each call site.
func WorkEntityID(id int64) string { return "ao3_work:" + strconv.FormatInt(id, 10) }

// EntityNumericID parses "<kind>:<n>" and returns n.
//
// A bare number is accepted too, because rows written before the prefix existed
// exist, and refusing them would silently ignore a reader's history. Anything
// else is not an entity id and reports false rather than 0 — a zero would then
// be excluded from a list as if it were a work.
func EntityNumericID(s string) (int64, bool) {
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
