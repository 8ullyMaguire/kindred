package corpus

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AO3Kind is the kind name for AO3 works.
const AO3Kind = "ao3_work"

// AO3 reads AO3 works from a read-only mirror.
//
// Two measured facts shape every line here (SPEC §0.2, §11.2):
//
//   - works.bookmarks is NULL for 112,890 of 112,935 rows. It is scanned
//     into a *int64 and a has_bookmarks stat is written, so a later sort
//     cannot mistake NULL for zero and rank the emptiest works highest.
//   - work_tags.tag_type is 'freeforms' for 3,890,504 of 3,891,300 rows.
//     Nothing branches on tag type; it is carried as a label.
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
			e.Stats["language_is_english"] = boolToFloat(language.String == "en")
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
	// sort on bookmarks silently ranks NULL as 0 — measured, 112,890 of
	// 112,935 rows are NULL.
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

// parseDate converts an ISO date to epoch days. Returns 0 for anything
// unparseable, which the signals treat as "unknown" rather than "old".
func parseDate(s string) float64 {
	for _, layout := range []string{"2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.Unix() / 86400)
		}
	}
	return 0
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
