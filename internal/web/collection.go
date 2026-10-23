package web

// "Complete your collection" (#50) — how much of a byline's shelf this reader
// has read, and what is left.
//
// The page already lists an author's works. What it could not say is the one
// thing a reader with a long history actually wants: "you are four works from
// the end of this author". The two halves live in different databases — the
// corpus knows the works, the state database knows the reads — so the count is
// computed from a bounded id list rather than joined.
//
// # Why the count is over the MATCHED LIST, not the page
//
// "4 of 20" where 20 is the page size would be a number about pagination. The
// shelf is the byline's whole catalogue, so the ids are gathered once, capped at
// collectionCap, and the page says "at least" when the cap is hit.

import (
	"context"
	"net/http"
	"strings"
)

// collectionCap bounds the id list this page reads. An author with more works
// than this still gets a progress line; it is marked as measured on the first
// `cap` works rather than claiming the shelf is complete.
const collectionCap = 5000

// collectionInfo returns the reader's progress through a byline match and the
// set of works they have already read.
//
// A missing session yields (nil, nil, nil): the page then simply does not claim
// anything about the reader. An error in the read set is returned so the caller
// can say the count is unavailable — not so it can print zero, which would look
// like "you have read nothing" to a reader who has read everything.
func (d Deps) collectionInfo(ctx context.Context, ownerKey, like string) (*CollectionProgress, map[int64]bool, error) {
	if ownerKey == "" || !d.hasCorpus() {
		return nil, nil, nil
	}
	// One over the cap, so "capped" is a fact about the corpus rather than a
	// guess at equality.
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx,
		`SELECT id FROM works WHERE authors LIKE ? ESCAPE '\'
		 ORDER BY kudos DESC, id LIMIT ?`, like, collectionCap+1)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, 256)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	capped := false
	if len(ids) > collectionCap {
		capped = true
		ids = ids[:collectionCap]
	}
	read, err := d.Engine.Store.ReadAmong(ctx, ownerKey, ids)
	if err != nil {
		return nil, nil, err
	}
	progress := &CollectionProgress{Read: len(read), Total: len(ids), ToGo: len(ids) - len(read), Capped: capped}
	return progress, read, nil
}

// unreadClause builds the SQL fragment excluding works already read, in chunks
// of 400, appending the entity-id parameters to args.
//
// The ids bound here are the ones `seen_works` stores, so the comparison is
// against the production shape rather than a bare integer that would silently
// match nothing.
func unreadClause(read map[int64]bool, args []any) (string, []any) {
	if len(read) == 0 {
		return "", args
	}
	ids := make([]int64, 0, len(read))
	for id := range read {
		ids = append(ids, id)
	}
	var sb strings.Builder
	for start := 0; start < len(ids); start += 400 {
		end := start + 400
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		sb.WriteString(` AND w.id NOT IN (`)
		for i := range chunk {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("?")
		}
		sb.WriteString(")")
		for _, id := range chunk {
			args = append(args, id)
		}
	}
	return sb.String(), args
}

// collectionForOwner resolves the reader for a read-only page.
//
// It deliberately does NOT mint a session: browsing an author page is not an
// act that should create standing state. A reader who has a cookie gets their
// progress; a reader who does not sees the shelf and no promise about
// themselves.
func (d Deps) collectionForOwner(r *http.Request) string {
	if !d.hasStore() {
		return ""
	}
	if c, err := r.Cookie(ArenaCookieName); err == nil && c.Value != "" {
		if ownerKey, rerr := d.Engine.Store.OwnerForSession(r.Context(), c.Value); rerr == nil && ownerKey != "" {
			return ownerKey
		}
	}
	return ""
}
