package corpus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// AO3Kind is the kind name for AO3 works.
//
// It is arena.KindWork's value rather than a literal, so the corpus's kind and
// the arena's kind cannot drift apart the way two copies of a constant do.
const AO3Kind = string(arena.KindWork)

// AO3 reads AO3 works from a read-only mirror.
//
// Two measured facts shape every line here (SPEC §0.2, §11.2):
//
//   - works.bookmarks is NULLable, and an uncrawled work leaves it NULL.
//     (The live mirror measured 0 NULLs in 112,935 rows on 2026-10-05, so this
//     is a schema fact rather than a property of the current crawl.)
//     It is scanned
//     into a *int64 and a has_bookmarks stat is written, so a later sort
//     cannot mistake NULL for zero and rank the emptiest works highest.
//   - work_tags.tag_type is 'freeforms' for 3,890,504 of 3,891,300 rows,
//     'characters' for 304, 'relationships' for 175 and 'fandoms' for 104
//     (measured 2026-10-14). `EntityCandidates` branches on it, which is why
//     the tag-type-specific arenas exist at all and why the web switcher offers
//     the kinds the corpus can actually serve rather than every kind the arena
//     can name.
type AO3 struct {
	DB *sql.DB
}

func NewAO3(db *sql.DB) *AO3 { return &AO3{DB: db} }

func (a *AO3) Kind() string { return AO3Kind }

const ao3WorkCols = `w.id, w.title, w.url, COALESCE(w.summary,''), COALESCE(w.authors,''),
	w.word_count, COALESCE(w.kudos,0), COALESCE(w.hits,0), w.bookmarks,
	COALESCE(w.complete,0), COALESCE(w.update_date,''), COALESCE(w.first_seen,''),
	COALESCE(w.language,''), COALESCE(w.rating,'')`

// Work is a row of the mirror's works table, kept in the corpus package
// because the SQL lives here and the API should not speak it.
type Work struct {
	ID         int64
	Title      string
	URL        string
	Summary    string
	Authors    string
	WordCount  int64
	Kudos      int64
	Hits       int64
	Bookmarks  *int64 // NULL in the corpus; do not collapse to 0
	Complete   int64
	UpdateDate string
	FirstSeen  string
	Language   string
	Rating     string
}

func scanWork(rows interface{ Scan(...any) error }) (Work, error) {
	var w Work
	err := rows.Scan(&w.ID, &w.Title, &w.URL, &w.Summary, &w.Authors,
		&w.WordCount, &w.Kudos, &w.Hits, &w.Bookmarks,
		&w.Complete, &w.UpdateDate, &w.FirstSeen, &w.Language, &w.Rating)
	return w, err
}

// CountWorks reports the corpus size, for progress and for the log line
// that proves an ingest actually saw the rows.
func (a *AO3) CountWorks(ctx context.Context) (int, error) {
	var n int
	err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&n)
	return n, err
}

// TopByKudos returns the ids of the most-kudosed works in the mirror.
//
// It exists for the reader who opens /recommend with nothing typed: an empty
// form asks them to invent a query, and the honest default is the works the
// mirror's own readers have already chosen most often. Kudos is the only
// corpus-wide popularity the mirror carries — bookmarks live in
// user_work_interactions, which is in the state database, not here — so the
// page names this as "most kudosed" rather than presenting it as a taste
// match.
//
// The tie-break is the id, so two requests get the same seeds and a reader
// who reloads does not see the set change for no reason.
func (a *AO3) TopByKudos(ctx context.Context, limit int) ([]int64, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := a.DB.QueryContext(ctx,
		`SELECT id FROM works ORDER BY COALESCE(kudos,0) DESC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("top by kudos: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("top by kudos: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (a *AO3) CountTags(ctx context.Context) (int, error) {
	var n int
	err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags`).Scan(&n)
	return n, err
}

func (a *AO3) CountWorkTags(ctx context.Context) (int, error) {
	var n int
	err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_tags`).Scan(&n)
	return n, err
}

func (a *AO3) CountEdges(ctx context.Context) (int, error) {
	var n int
	err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM cooccurrence_edges`).Scan(&n)
	return n, err
}

// GraphTags returns the distinct tag ids the co-occurrence graph actually
// references.
//
// It does not read cooccurrence_graph_meta, which claimed 20,262 nodes
// while the edges referenced 123,047 (SPEC §0.1, §11.1). That row is
// stale and a stale metadata row is a lie you inherit if you read it.
func (a *AO3) GraphTags(ctx context.Context) (map[int32]bool, int, error) {
	rows, err := a.DB.QueryContext(ctx, `
		SELECT tag_a_id FROM cooccurrence_edges
		UNION
		SELECT tag_b_id FROM cooccurrence_edges`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	set := make(map[int32]bool, 1<<17)
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		set[id] = true
	}
	return set, len(set), rows.Err()
}

// Edge is one undirected co-occurrence, emitted in both directions by
// EdgeIter so the CSR builder sees each undirected edge once and
// symmetrises it itself.
type Edge struct {
	A     int32
	B     int32
	Count int64
}

// EdgeIter streams every co-occurrence edge once.
func (a *AO3) EdgeIter(ctx context.Context, fn func(Edge) error) error {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT tag_a_id, tag_b_id, cooccur_count FROM cooccurrence_edges`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.A, &e.B, &e.Count); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Entity loads one work with its tags. It goes through CandidateRows
// rather than querying the work columns separately, so the single-work
// path and the pool path build an Entity through exactly one code path —
// two constructors is how a field ends up populated in one and missing
// in the other.
func (a *AO3) Entity(ctx context.Context, id int64) (Entity, error) {
	ents, err := a.CandidateRows(ctx, []int64{id})
	if err != nil {
		return Entity{}, err
	}
	if len(ents) == 0 {
		return Entity{}, fmt.Errorf("work %d: %w", id, ErrNotFound)
	}
	return ents[0], nil
}

var ErrNotFound = fmt.Errorf("not found in corpus")

// TagPair is a tag id together with its name.
//
// Both halves travel as one because both are needed together: the graph
// is keyed by id and a response shows names. Carrying them as a pair is
// what stops them arriving out of step — matching an id list against a
// name list by position is how a ranking ends up looking entirely
// plausible while scoring every candidate against the wrong tags.
type TagPair struct {
	ID   int32
	Name string
}

// TagPairs returns the tags of each work, id and name together.
//
// The graph is keyed by tag id while the corpus reports tag names, so
// resolving one from the other client-side would mean a 634,231-entry
// map. That map was measured at 50 MB of RSS and deleted once. work_tags
// joins tags in one row, so both halves arrive together: no extra query,
// no extra map, and no way for the two lists to disagree.
// TagIDByName resolves one tag's id by exact name (case-sensitive: the
// mirror stores every tag lowercase, and callers lowercase before calling).
// sql.ErrNoRows is returned unwrapped so a caller can distinguish "no such
// tag" from a real fault with errors.Is.
func (a *AO3) TagIDByName(ctx context.Context, name string) (int64, error) {
	var id int64
	err := a.DB.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: no tag named %q", ErrNotFound, name)
	}
	return id, err
}

func (a *AO3) TagPairs(ctx context.Context, ids []int64) (map[int64][]TagPair, error) {
	if len(ids) == 0 {
		return map[int64][]TagPair{}, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := a.DB.QueryContext(ctx,
		`SELECT wt.work_id, wt.tag_id, t.name
		 FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		 WHERE wt.work_id IN (`+makePlaceholders(len(args))+`)
		 ORDER BY wt.work_id, wt.tag_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("tag pairs: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]TagPair, len(ids))
	for rows.Next() {
		var work int64
		var pair TagPair
		if err := rows.Scan(&work, &pair.ID, &pair.Name); err != nil {
			return nil, err
		}
		out[work] = append(out[work], pair)
	}
	return out, rows.Err()
}

// CandidateRows loads works with their tags in one pass — no N+1. Ranking
// pulls thousands of candidates per request, so a per-candidate tag
// query is the single largest avoidable cost in the engine.
// CandidateRowsSlim loads ranking fields only, omitting the summary.
//
// Ranking reads stats and tags. The summary is the corpus's largest text
// field — a 39-chapter fic's summary is a paragraph of prose — and
// loading it for every candidate in the pool is the difference between a
// 25 MiB and a 7 MiB request. The engine ranks from the slim rows and
// then loads the summary for the handful that survive, which is a
// different number by an order of magnitude.
func (a *AO3) CandidateRowsSlim(ctx context.Context, ids []int64) ([]Entity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := a.DB.QueryContext(ctx, `
		SELECT id, url, title, word_count, hits, kudos, bookmarks, language,
		       complete, update_date, first_seen
		FROM works WHERE id IN (`+makePlaceholders(len(args))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Entity, 0, len(ids))
	for rows.Next() {
		var (
			e          Entity
			title      sql.NullString
			url        sql.NullString
			bookmarks  sql.NullInt64
			language   sql.NullString
			complete   sql.NullInt64
			updateDate sql.NullString
			firstSeen  sql.NullString
			wordCount  sql.NullInt64
			hits       sql.NullInt64
			kudos      sql.NullInt64
		)
		if err := rows.Scan(&e.ID, &url, &title, &wordCount, &hits, &kudos,
			&bookmarks, &language, &complete, &updateDate, &firstSeen); err != nil {
			return nil, err
		}
		e.Kind = AO3Kind
		e.Title = title.String
		e.URL = url.String
		e.Stats = map[string]float64{
			"hits": float64(hits.Int64), "kudos": float64(kudos.Int64),
			"word_count":    float64(wordCount.Int64),
			"has_bookmarks": boolToFloat(bookmarks.Valid),
		}
		if bookmarks.Valid {
			e.Stats["bookmarks"] = float64(bookmarks.Int64)
		}
		if language.Valid {
			e.Stats["language_is_english"] = boolToFloat(strings.EqualFold(language.String, "en") || strings.EqualFold(language.String, "english"))
		}
		if complete.Valid {
			e.Stats["complete"] = float64(complete.Int64)
		}
		// parseDate is the one parser in this package, and the slim loader
		// uses it too. A second parser is how a recency signal starts
		// disagreeing with itself depending on which query loaded the row.
		if updateDate.Valid {
			e.Stats["update_date"] = parseDate(updateDate.String)
		}
		if firstSeen.Valid {
			e.Stats["first_seen"] = parseDate(firstSeen.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (a *AO3) CandidateRows(ctx context.Context, ids []int64) ([]Entity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	byID := make(map[int64]int, len(ids))
	order := make([]int64, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if _, seen := byID[id]; seen {
			continue
		}
		byID[id] = len(order)
		order = append(order, id)
		args = append(args, id)
	}

	q := `SELECT ` + ao3WorkCols + ` FROM works w WHERE w.id IN (` +
		makePlaceholders(len(args)) + `)`
	rows, err := a.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Entity, 0, len(order))
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, entityFromWork(w))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	index := make(map[int64]int, len(out))
	for i, e := range out {
		index[e.ID] = i
	}
	if err := a.attachTags(ctx, out, index, order); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *AO3) attachTags(ctx context.Context, ents []Entity, index map[int64]int, ids []int64) error {
	if len(ents) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := a.DB.QueryContext(ctx, `
		SELECT wt.work_id, t.name, wt.tag_type
		FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id IN (`+makePlaceholders(len(args))+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var workID int64
		var name, typ string
		if err := rows.Scan(&workID, &name, &typ); err != nil {
			return err
		}
		i, ok := index[workID]
		if !ok {
			continue
		}
		ents[i].Tags = append(ents[i].Tags, Tag{Name: name, Type: typ, Weight: 1})
	}
	return rows.Err()
}

func entityFromWork(w Work) Entity {
	e := Entity{
		ID:      w.ID,
		Kind:    AO3Kind,
		Title:   w.Title,
		URL:     w.URL,
		Summary: w.Summary,
		Stats:   make(map[string]float64, 7),
	}
	e.Stats["kudos"] = float64(w.Kudos)
	e.Stats["hits"] = float64(w.Hits)
	e.Stats["word_count"] = float64(w.WordCount)
	e.Stats["complete"] = float64(w.Complete)
	// has_bookmarks is the explicit NULL/zero distinction. Without it a
	// sort on bookmarks silently ranks NULL as 0 — the column is NULLable, and
	// the fixture makes one work in seven NULL to keep the path exercised.
	if w.Bookmarks == nil {
		e.Stats["has_bookmarks"] = 0
	} else {
		e.Stats["bookmarks"] = float64(*w.Bookmarks)
		e.Stats["has_bookmarks"] = 1
	}
	if w.UpdateDate != "" {
		e.Stats["update_date"] = parseDate(w.UpdateDate)
	}
	if w.FirstSeen != "" {
		e.Stats["first_seen"] = parseDate(w.FirstSeen)
	}
	if w.Language != "" {
		e.Stats["language_"+w.Language] = 1
	}
	return e
}

// parseDate converts a date to epoch days. Returns 0 for anything
// unparseable, which the signals treat as "unknown" rather than "old".
//
// "2 Jan 2006" is AO3's own display format and is what the LIVE mirror stores
// in works.update_date ("17 Sep 2008"); the ISO layouts cover first_seen /
// last_updated (RFC3339 with microseconds) and any normalised export. It was
// missing, and the consequence was silent: every live work parsed to 0, the
// recency signal treated them all as undated and skipped them, and no test
// noticed because the test fixture wrote ISO dates the parser accepted. A
// fixture in the wrong format validates the wrong behaviour.
func parseDate(s string) float64 {
	if t, ok := ParseDate(s); ok {
		return float64(t.Unix() / 86400)
	}
	return 0
}

// ParseDate reads a corpus date in any of the layouts the mirror uses, and is
// the single list they all go through.
//
// Exported because a consumer that needs the DATE rather than epoch days — an
// RSS pubDate, say — must not carry its own copy: two copies drift, and the
// drift is silent. One would parse "31 Oct 2025" and the other would not, and
// only the surface built on the second copy would show it.
func ParseDate(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2 Jan 2006",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Placeholders renders n comma-separated bind markers. Exported because
// the engine builds the same IN (...) lists the corpus does.
func Placeholders(n int) string { return makePlaceholders(n) }

func makePlaceholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ── entity arenas: candidates for kinds other than works ─────────────────────

// minWorksPerTag is the arena's evidence floor. A tag carried by two works
// cannot be meaningfully ranked against one carried by two thousand, and the
// floor is also what keeps the candidate table a head rather than a tail: of
// 634,231 tags in the mirror, 91,084 clear it (measured 2026-10-14).
const minWorksPerTag = 3

// authorSampleRows bounds the author loader's scan. The mirror has no author
// table — only `works.authors`, a comma-space list of bylines — so authors can
// only be counted by sampling works. SQLite cannot GROUP BY the Go-side
// normaliser, which is why the dedupe happens in Go and why the scan is bounded
// rather than complete: a sampled head of the distribution is what an arena
// wants, and a full scan of every work would cost far more than it tells us.
const authorSampleRows = 4000

// EntityCandidates loads one kind's arena candidates from the mirror.
//
// This is the slow half of the arena's candidate machinery, and it is meant to
// be called on a refresh rather than on a page view: the freeform query groups
// 3,890,504 rows and measured 2.2s against the live mirror. The state store
// keeps the materialised result (see store.EnsureEntityCandidates); nothing on a
// request path calls this directly except to fill that table.
//
// Works are refused rather than served empty: a work arena's candidates come
// from a tag (`Store.CandidateWorkIDs`) and from the session's own history, and
// an empty list here would look like "no works matched" instead of "wrong
// loader".
func (a *AO3) EntityCandidates(ctx context.Context, kind arena.EntityKind, limit int) ([]arena.EntityCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	switch {
	case kind == arena.KindWork:
		return nil, fmt.Errorf("corpus: the %s arena's candidates come from a tag, not from this loader", kind)
	case kind.IsTag():
		tagType, _ := kind.TagType()
		return a.tagCandidates(ctx, tagType, limit)
	case kind == arena.KindAuthor:
		return a.authorCandidates(ctx, limit)
	default:
		return nil, fmt.Errorf("corpus: no candidate loader for kind %q", kind)
	}
}

// tagCandidates returns the `limit` most-carried tags of one type.
//
// The head of the distribution on purpose. A uniform sample of 634,231 tags
// would be mostly tags used once, and a ranking of tags nobody uses is not a
// ranking anyone wants; the arena's job is to order the tags in play. Names come
// from the corpus's `tags` table in a second query rather than a join, because a
// join against a GROUP BY of this size is the slow shape this function exists to
// keep off the request path.
func (a *AO3) tagCandidates(ctx context.Context, tagType string, limit int) ([]arena.EntityCandidate, error) {
	rows, err := a.DB.QueryContext(ctx, `
		SELECT tag_id, COUNT(*) AS n
		FROM work_tags
		WHERE tag_type = ?
		GROUP BY tag_id
		HAVING n >= ?
		ORDER BY n DESC, tag_id
		LIMIT ?`, tagType, minWorksPerTag, limit)
	if err != nil {
		return nil, fmt.Errorf("corpus: tag candidates (%s): %w", tagType, err)
	}
	defer func() { _ = rows.Close() }()

	cands := make([]arena.EntityCandidate, 0, limit)
	for rows.Next() {
		var c arena.EntityCandidate
		if err := rows.Scan(&c.ID, &c.Weight); err != nil {
			return nil, fmt.Errorf("corpus: scan tag candidate: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corpus: tag candidates: %w", err)
	}
	if len(cands) == 0 {
		// A kind whose tag type the mirror has almost nothing of is a
		// normal state, not a fault: 'characters' held 304 rows when this
		// was written. The caller decides whether to offer it.
		return nil, nil
	}

	names, err := a.tagNames(ctx, idsOf(cands))
	if err != nil {
		return nil, err
	}
	kept := cands[:0]
	for _, c := range cands {
		name, ok := names[c.ID]
		if !ok {
			// A tag id in work_tags with no row in tags is a corpus that
			// disagrees with itself. Dropping it is the honest option:
			// an arena cannot present a candidate it cannot name.
			continue
		}
		c.Name = name
		kept = append(kept, c)
	}
	return kept, nil
}

// TagNames resolves tag ids to names in one query per batch.
//
// Exported because the embedding finder produces tag IDS from the state
// database and the mirror is the only place their NAMES live: a page that
// showed "tag 29 is similar to tag 840" would be reporting a similarity
// nobody can act on. One query, so the two sets cannot disagree.
func (a *AO3) TagNames(ctx context.Context, ids []int64) (map[int64]string, error) {
	return a.tagNames(ctx, ids)
}

// tagNames resolves tag ids to names, in one query per batch.
func (a *AO3) tagNames(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := a.DB.QueryContext(ctx,
		`SELECT id, name FROM tags WHERE id IN (`+makePlaceholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("corpus: tag names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("corpus: scan tag name: %w", err)
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corpus: tag names: %w", err)
	}
	return out, nil
}

// authorCandidates samples works.authors and returns distinct bylines.
//
// Two authors who share a byline are one entity here (see arena.AuthorEntityID);
// the weight is how often the byline appeared in the sample, which is a sampled
// count and is documented as one rather than presented as a true total.
func (a *AO3) authorCandidates(ctx context.Context, limit int) ([]arena.EntityCandidate, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT authors FROM works WHERE authors <> '' ORDER BY RANDOM() LIMIT ?`, authorSampleRows)
	if err != nil {
		return nil, fmt.Errorf("corpus: author candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type entry struct {
		name  string
		count int
	}
	byID := make(map[int64]*entry)
	for rows.Next() {
		var authors string
		if err := rows.Scan(&authors); err != nil {
			return nil, fmt.Errorf("corpus: scan authors: %w", err)
		}
		for _, byline := range arena.SplitAuthors(authors) {
			id := arena.AuthorEntityID(byline)
			name := strings.TrimSpace(byline)
			if e, ok := byID[id]; ok {
				e.count++
				// The sample is read in RANDOM order, so the display name
				// cannot be "whichever spelling came first": that made the
				// name change between runs for an entity whose id did
				// not. Prefer the most informative byline (longest), then
				// the lexicographically smallest, so the choice is a
				// function of the data rather than of the scan order.
				if betterAuthorName(name, e.name) {
					e.name = name
				}
				continue
			}
			byID[id] = &entry{name: name, count: 1}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corpus: author candidates: %w", err)
	}

	cands := make([]arena.EntityCandidate, 0, len(byID))
	for id, e := range byID {
		cands = append(cands, arena.EntityCandidate{ID: id, Name: e.name, Weight: e.count})
	}
	// Most-seen first, id as the tie-break so the table is reproducible: a
	// sample that reorders equal candidates on every refresh would make the
	// candidate table's diff unreadable for no gain.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Weight != cands[j].Weight {
			return cands[i].Weight > cands[j].Weight
		}
		return cands[i].ID < cands[j].ID
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	return cands, nil
}

// idsOf is the id column of a candidate slice, for the name lookup.
func idsOf(cands []arena.EntityCandidate) []int64 {
	ids := make([]int64, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	return ids
}

// betterAuthorName reports whether candidate is a better display name than
// current for the same author id.
//
// Deterministic on purpose. The candidate loader samples works in random order,
// and taking the first spelling it saw made the name depend on that order: the
// same author could be "alpha" on one run and "Alpha (alpha_user)" on the next,
// with the same id and the same rating. A name that changes between runs is
// worse than either choice.
func betterAuthorName(candidate, current string) bool {
	if len(candidate) != len(current) {
		return len(candidate) > len(current)
	}
	return candidate < current
}
