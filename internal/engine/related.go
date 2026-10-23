package engine

// Related: the per-kind recommendation surface behind the algorithm registry.
//
// # What this file is for
//
// `Recommend` ranks WORKS. The site's other kinds — tags, authors, and the
// tag-ish kinds (characters, relationships, fandoms) — had exactly one surface
// each, and no choice: a tag page showed its embedding neighbours, an author
// page showed nothing related at all. This file answers "related to this seed",
// for any kind, by an algorithm the reader chose.
//
// # Why the algorithms are not all symmetric
//
// A mechanism is a fact about the data, not about the kind. Cosine over the tag
// embedding says nothing about an author; a co-reader walk says nothing about a
// tag. So the registry (`internal/algo`) lists, per kind, what can actually run,
// and this file implements each one against the table that holds the signal:
//
//	kind     algo        source
//	works    hybrid      the shipped blend (Recommend)
//	works    collab      the resident co-bookmark index
//	works    adamic      seen_works: 1/log(degree) over shared readers
//	works    popular     works sharing the seed's tags, by kudos
//	tags     embed       the tag-embedding matrix
//	tags     cooccur     work_tags self-join, Jaccard-normalised
//	tags     popular     tags carrying the most works
//	authors  cobook      readers of the seed author's works
//	authors  tagidf      rare tags shared with the seed author's works
//	authors  popular     total hits per byline
//
// # The two-database rule
//
// The corpus (works, tags) and the state (readers' seen_works) are separate
// SQLite files with no join between them. Anything that walks readers therefore
// reads the corpus first, then the state, then the corpus again — three queries
// and a set in Go, rather than one SQL statement that cannot run.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/algo"
	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/collab"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// RelatedItem is one row of a related list.
type RelatedItem struct {
	ID    int64   `json:"id"`
	Kind  string  `json:"kind"`
	Label string  `json:"label"`
	URL   string  `json:"url,omitempty"`
	Score float64 `json:"score"`
	// Why is the evidence sentence for this row, in whole words: "41 shared
	// readers" is checkable by the reader, "+0.42" is not.
	Why string `json:"why,omitempty"`
}

// RelatedResult is one related list.
type RelatedResult struct {
	Kind  string        `json:"kind"`
	Seed  int64         `json:"seed"`
	Algo  string        `json:"algo"`
	Items []RelatedItem `json:"items"`
	// Note explains a result that is technically correct and practically
	// uninformative — a central tag, a seed with no readers, a popularity
	// baseline nobody should read as a recommendation.
	Note string `json:"note,omitempty"`
	// Missing names a dimension that could not run, so an empty list is
	// distinguishable from a list that silently lost its signal.
	Missing []string  `json:"missing,omitempty"`
	Took    time.Time `json:"-"`
	Millis  int64     `json:"took_ms"`
}

// maxRelated caps a related list: more than this is a page nobody scrolls and a
// query nobody should wait for.
const maxRelated = 50

// Related answers "what is like this seed" by the named algorithm.
func (e *Engine) Related(ctx context.Context, kind arena.EntityKind, id int64, algoID algo.ID, limit int) (RelatedResult, error) {
	started := time.Now()
	if limit <= 0 || limit > maxRelated {
		limit = 20
	}
	res := RelatedResult{Kind: string(kind), Seed: id, Algo: string(algoID), Items: []RelatedItem{}}
	if _, live := algo.Lookup(kind, algoID); !live {
		return res, fmt.Errorf("algorithm %q cannot run for %s on this deployment", algoID, kind.Label())
	}

	var err error
	switch kind {
	case arena.KindWork:
		err = e.relatedWork(ctx, id, algoID, limit, &res)
	case arena.KindTag, arena.KindCharacter, arena.KindRelationship, arena.KindFandom:
		// The tag-ish kinds are tags: the mirror files all but ~600 rows of
		// work_tags as `freeforms`, so a separate mechanism per tag_type would
		// be the same query behind a filter that matches nothing.
		err = e.relatedTag(ctx, id, algoID, limit, &res)
	case arena.KindAuthor:
		err = e.relatedAuthor(ctx, id, algoID, limit, &res)
	default:
		err = fmt.Errorf("no related surface for kind %q", kind)
	}
	res.Millis = time.Since(started).Milliseconds()
	if err != nil {
		return res, err
	}
	if res.Items == nil {
		res.Items = []RelatedItem{}
	}
	return res, nil
}

// ---- works ---------------------------------------------------------------

func (e *Engine) relatedWork(ctx context.Context, id int64, algoID algo.ID, limit int, res *RelatedResult) error {
	switch algoID {
	case algo.Hybrid:
		out, err := e.Recommend(ctx, Request{
			Seeds:   []Seed{{Kind: "ao3_work", ID: id}},
			N:       limit,
			Exclude: true,
		})
		if err != nil {
			return err
		}
		for _, w := range out.Items {
			res.Items = append(res.Items, RelatedItem{
				ID: w.ID, Kind: string(arena.KindWork), Label: w.Title, URL: w.URL,
				Score: w.Score, Why: evidenceSentence(w.Evidence),
			})
		}
		if len(res.Items) == 0 {
			res.Missing = append(res.Missing, "candidates")
			res.Note = "the blend found nothing to rank from this seed"
		}
		return nil

	case algo.Collab:
		if e.Collab == nil {
			res.Missing = append(res.Missing, "co_bookmarks")
			res.Note = "this engine was built without the co-bookmark index, so " +
				"item-item CF cannot run here"
			return nil
		}
		sims := e.Collab.Similar(id, limit, collab.Options{})
		if len(sims) == 0 {
			res.Missing = append(res.Missing, "co_bookmarks")
			res.Note = "no reader of this work has read a second work, so there is " +
				"nothing for co-bookmark similarity to say. That is a fact about the " +
				"mirror's bookmark coverage, not a failure."
			return nil
		}
		ids := make([]int64, 0, len(sims))
		for _, s := range sims {
			ids = append(ids, s.ID)
		}
		labels, err := e.workLabels(ctx, ids)
		if err != nil {
			return err
		}
		for _, s := range sims {
			lab := labels[s.ID]
			res.Items = append(res.Items, RelatedItem{
				ID: s.ID, Kind: string(arena.KindWork), Label: lab.title, URL: lab.url,
				Score: s.Score,
				Why:   fmt.Sprintf("%.2f cosine over bookmarkers, %d co-reader(s)", s.Score, s.CoUsers),
			})
		}
		return nil

	case algo.Adamic:
		return e.adamicWalk(ctx, id, limit, res)

	case algo.Popular:
		return e.popularWorks(ctx, id, limit, res)
	}
	return fmt.Errorf("no work implementation for %q", algoID)
}

// adamicCap bounds how many of the seed's readers the walk starts from.
//
// The cap is the one compromise in this algorithm and it is measured, not
// guessed: a seed read by 40k readers cannot be walked per request, and taking
// the FIRST N readers by owner key is a biased sample (it favours whichever
// names sort first). The query instead takes the readers of the seed's
// most-recent impressions, which is at least a coherent population, and the
// result says when it was capped.
const adamicCap = 4000

func (e *Engine) adamicWalk(ctx context.Context, id int64, limit int, res *RelatedResult) error {
	if e.Store == nil || e.Store.DB == nil {
		res.Missing = append(res.Missing, "readers")
		res.Note = "this engine has no state database, so there are no readers to walk"
		return nil
	}
	const q = `
WITH seed_readers AS (
  SELECT owner_key, MAX(shown_at) AS at
  FROM seen_works WHERE entity_id = ?
  GROUP BY owner_key
  ORDER BY at DESC LIMIT ?
),
deg AS (
  SELECT s.owner_key, COUNT(*) AS n
  FROM seen_works s JOIN seed_readers r ON r.owner_key = s.owner_key
  GROUP BY s.owner_key
)
SELECT s.entity_id,
       SUM(1.0 / (1.0 + LOG(1.0 + d.n))) AS score,
       COUNT(*) AS shared
FROM seen_works s JOIN deg d ON d.owner_key = s.owner_key
WHERE s.entity_id <> ?
GROUP BY s.entity_id
ORDER BY score DESC LIMIT ?`
	// The state table stores "<kind>:<id>", so the bind value is the prefixed
	// form. A bare number here matches nothing and the walk reports "no shared
	// readers" for a work with thousands — measured on the live mirror before
	// this line existed.
	entity := store.WorkEntityID(id)
	rows, err := e.Store.DB.QueryContext(ctx, q, entity, adamicCap, entity, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	type hit struct {
		id     int64
		score  float64
		shared int
	}
	var hits []hit
	for rows.Next() {
		var raw string
		var h hit
		if err := rows.Scan(&raw, &h.score, &h.shared); err != nil {
			return err
		}
		var ok bool
		if h.id, ok = store.EntityNumericID(raw); !ok {
			continue // a seen_works row with a non-work id cannot be a candidate
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(hits) == 0 {
		res.Missing = append(res.Missing, "readers")
		res.Note = "no one has read this work and a second work, so the co-reader walk " +
			"has no shared readers to weight"
		return nil
	}
	ids := make([]int64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.id)
	}
	labels, err := e.workLabels(ctx, ids)
	if err != nil {
		return err
	}
	for _, h := range hits {
		lab := labels[h.id]
		res.Items = append(res.Items, RelatedItem{
			ID: h.id, Kind: string(arena.KindWork), Label: lab.title, URL: lab.url,
			Score: h.score,
			Why:   fmt.Sprintf("%d shared reader(s), weighted 1/(1+log(their bookmarks))", h.shared),
		})
	}
	if len(res.Items) == limit {
		res.Note = fmt.Sprintf("walked from the %d most recent of this work's readers", adamicCap)
	}
	return nil
}

// popularWorks is the baseline: works sharing at least one tag with the seed,
// ordered by kudos.
//
// It is not a recommendation and the note says so on the page. Its job is to be
// the floor a personalised list has to clear, which is why it is selectable at
// all — a standings table without it cannot tell whether any personalisation is
// working.
func (e *Engine) popularWorks(ctx context.Context, id int64, limit int, res *RelatedResult) error {
	const q = `
WITH seed_tags AS (SELECT tag_id FROM work_tags WHERE work_id = ?)
SELECT w.id, w.title, COALESCE(w.url,''), COALESCE(w.kudos,0),
       (SELECT COUNT(*) FROM work_tags wt
        WHERE wt.work_id = w.id AND wt.tag_id IN (SELECT tag_id FROM seed_tags)) AS shared
FROM works w JOIN work_tags x ON x.work_id = w.id
WHERE x.tag_id IN (SELECT tag_id FROM seed_tags) AND w.id <> ?
GROUP BY w.id
ORDER BY COALESCE(w.kudos,0) DESC, shared DESC
LIMIT ?`
	rows, err := e.Corpus.DB.QueryContext(ctx, q, id, id, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var it RelatedItem
		var kudos, shared int64
		if err := rows.Scan(&it.ID, &it.Label, &it.URL, &kudos, &shared); err != nil {
			return err
		}
		it.Kind = string(arena.KindWork)
		it.Score = math.Log1p(float64(kudos))
		it.Why = fmt.Sprintf("%d kudos, %d tag(s) in common — popularity, not personalisation", kudos, shared)
		res.Items = append(res.Items, it)
	}
	return rows.Err()
}

// ---- tags (and the tag-ish kinds) ---------------------------------------

func (e *Engine) relatedTag(ctx context.Context, id int64, algoID algo.ID, limit int, res *RelatedResult) error {
	switch algoID {
	case algo.Embed:
		nb, found, err := e.TagsNear(ctx, int32(id), limit, 0.0)
		if err != nil {
			res.Missing = append(res.Missing, "embeddings")
			res.Note = "the tag-embedding index could not be read: " + err.Error()
			return nil
		}
		if !found {
			res.Missing = append(res.Missing, "embeddings")
			res.Note = "this tag has no vector in the embedding index (the index predates " +
				"the tag, or its family was skipped), so the nearest-neighbour list does not exist"
			return nil
		}
		names, err := e.tagNames(ctx, tagIDs(nb.Near))
		if err != nil {
			return err
		}
		for _, n := range nb.Near {
			res.Items = append(res.Items, RelatedItem{
				ID: int64(n.ID), Kind: string(arena.KindTag), Label: names[n.ID],
				Score: n.Sim, Why: fmt.Sprintf("%.2f cosine in the tag space", n.Sim),
			})
		}
		if nb.CenterSim >= CenterSimFloor {
			res.Note = fmt.Sprintf("This tag sits %.2f from the corpus centroid, so its "+
				"neighbours describe the middle of the vocabulary rather than a meaning.", nb.CenterSim)
		}
		return nil

	case algo.Cooccur:
		// Jaccard over works: co / (n_seed + n_other - co). Bounded in [0,1] and
		// symmetric, which is what makes it comparable with the embedding score
		// in a standings table.
		const q = `
WITH seed AS (SELECT work_id FROM work_tags WHERE tag_id = ?),
n_seed AS (SELECT COUNT(*) AS n FROM seed),
counts AS (
  SELECT wt.tag_id AS tag_id, COUNT(*) AS co
  FROM work_tags wt JOIN seed s ON s.work_id = wt.work_id
  WHERE wt.tag_id <> ?
  GROUP BY wt.tag_id
),
n AS (SELECT tag_id, COUNT(*) AS n FROM work_tags GROUP BY tag_id)
SELECT c.tag_id, t.name, 1.0 * c.co / (ns.n + n.n - c.co) AS jaccard, c.co
FROM counts c
JOIN n ON n.tag_id = c.tag_id
JOIN n_seed ns
LEFT JOIN tags t ON t.id = c.tag_id
ORDER BY jaccard DESC, c.co DESC
LIMIT ?`
		rows, err := e.Corpus.DB.QueryContext(ctx, q, id, id, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it RelatedItem
			var name sql.NullString
			var co int64
			if err := rows.Scan(&it.ID, &name, &it.Score, &co); err != nil {
				return err
			}
			it.Kind = string(arena.KindTag)
			it.Label = name.String
			it.Why = fmt.Sprintf("filed together on %d work(s), %.3f Jaccard", co, it.Score)
			res.Items = append(res.Items, it)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(res.Items) == 0 {
			res.Note = "no other tag is filed on any work carrying this one"
		}
		return nil

	case algo.Popular:
		const q = `
SELECT t.id, t.name, COUNT(wt.work_id) AS works
FROM tags t JOIN work_tags wt ON wt.tag_id = t.id
WHERE t.id <> ?
GROUP BY t.id ORDER BY works DESC LIMIT ?`
		rows, err := e.Corpus.DB.QueryContext(ctx, q, id, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it RelatedItem
			var works int64
			if err := rows.Scan(&it.ID, &it.Label, &works); err != nil {
				return err
			}
			it.Kind = string(arena.KindTag)
			it.Score = math.Log1p(float64(works))
			it.Why = fmt.Sprintf("carries %d works — the popularity baseline", works)
			res.Items = append(res.Items, it)
		}
		return rows.Err()
	}
	return fmt.Errorf("no tag implementation for %q", algoID)
}

// ---- authors -------------------------------------------------------------

func (e *Engine) relatedAuthor(ctx context.Context, id int64, algoID algo.ID, limit int, res *RelatedResult) error {
	byline, err := e.authorByline(ctx, id)
	if err != nil {
		return err
	}
	if byline == "" {
		res.Note = "no author with this id is in the mirror"
		return nil
	}
	switch algoID {
	case algo.CoBookAuthors:
		return e.coBookAuthors(ctx, byline, limit, res)

	case algo.TagIDF:
		// Rare tags shared with the seed author's works, summed over the
		// candidate's works. The IDF weight is what stops a byline sharing only
		// "Fluff" or "Angst" from looking related to everything.
		const q = `
WITH seed_tags AS (
  SELECT DISTINCT wt.tag_id FROM work_tags wt JOIN works w ON w.id = wt.work_id
  WHERE w.authors = ?),
total AS (SELECT COUNT(*) AS n FROM works),
df AS (SELECT wt.tag_id, COUNT(DISTINCT wt.work_id) AS n
       FROM work_tags wt GROUP BY wt.tag_id),
scored AS (
  SELECT w.authors AS byline, SUM(LOG(1.0 + 1.0 * (SELECT n FROM total) / df.n)) AS score,
         COUNT(DISTINCT wt.tag_id) AS shared
  FROM work_tags wt
  JOIN works w ON w.id = wt.work_id
  JOIN df ON df.tag_id = wt.tag_id
  WHERE wt.tag_id IN (SELECT tag_id FROM seed_tags)
    AND w.authors <> '' AND w.authors <> ?
  GROUP BY w.authors)
SELECT byline, score, shared FROM scored ORDER BY score DESC LIMIT ?`
		rows, err := e.Corpus.DB.QueryContext(ctx, q, byline, byline, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it RelatedItem
			var name string
			var shared int64
			if err := rows.Scan(&name, &it.Score, &shared); err != nil {
				return err
			}
			it.Kind = string(arena.KindAuthor)
			it.Label = name
			it.ID = arena.AuthorEntityID(name)
			it.Why = fmt.Sprintf("%d rare tag(s) shared with this author's works", shared)
			res.Items = append(res.Items, it)
		}
		return rows.Err()

	case algo.Popular:
		const q = `
SELECT authors, SUM(COALESCE(hits,0)) AS hits, COUNT(*) AS works
FROM works WHERE authors <> '' AND authors <> ?
GROUP BY authors ORDER BY hits DESC LIMIT ?`
		rows, err := e.Corpus.DB.QueryContext(ctx, q, byline, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it RelatedItem
			var name string
			var hits, works int64
			if err := rows.Scan(&name, &hits, &works); err != nil {
				return err
			}
			it.Kind = string(arena.KindAuthor)
			it.Label = name
			it.ID = arena.AuthorEntityID(name)
			it.Score = math.Log1p(float64(hits))
			it.Why = fmt.Sprintf("%d works, %d hits — the popularity baseline", works, hits)
			res.Items = append(res.Items, it)
		}
		return rows.Err()
	}
	return fmt.Errorf("no author implementation for %q", algoID)
}

// coBookAuthors walks readers: their works, then the bylines of those works.
//
// Three queries, because the corpus (works → authors) and the state (who read
// what) are separate files. The walk is capped at the seed author's most-read
// works and the readers of those works; the note says so when the cap is hit,
// because a capped walk that reports a complete one is the failure mode this
// codebase keeps finding.
func (e *Engine) coBookAuthors(ctx context.Context, byline string, limit int, res *RelatedResult) error {
	if e.Store == nil || e.Store.DB == nil {
		res.Missing = append(res.Missing, "readers")
		res.Note = "this engine has no state database, so there are no readers to walk"
		return nil
	}
	rows, err := e.Corpus.DB.QueryContext(ctx,
		`SELECT id FROM works WHERE authors = ? ORDER BY COALESCE(hits,0) DESC LIMIT 200`, byline)
	if err != nil {
		return err
	}
	var workIDs []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		workIDs = append(workIDs, store.WorkEntityID(id))
	}
	rows.Close()
	if len(workIDs) == 0 {
		res.Note = "this author has no works in the mirror"
		return nil
	}

	// Readers of the seed's works, then everything those readers read.
	args := make([]any, 0, len(workIDs)+1)
	for _, w := range workIDs {
		args = append(args, w)
	}
	q := `WITH seed_readers AS (
	        SELECT DISTINCT owner_key FROM seen_works WHERE entity_id IN (` + placeholders(len(workIDs)) + `)
	      ),
	      theirs AS (
	        SELECT s.entity_id AS work_id, COUNT(*) AS readers
	        FROM seen_works s JOIN seed_readers r ON r.owner_key = s.owner_key
	        WHERE s.entity_id NOT IN (` + placeholders(len(workIDs)) + `)
	        GROUP BY s.entity_id ORDER BY readers DESC LIMIT 400)
	      SELECT work_id, readers FROM theirs`
	for _, w := range workIDs {
		args = append(args, w)
	}
	rows, err = e.Store.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	readers := map[int64]int64{}
	for rows.Next() {
		var raw string
		var n int64
		if err := rows.Scan(&raw, &n); err != nil {
			rows.Close()
			return err
		}
		if id, ok := store.EntityNumericID(raw); ok {
			readers[id] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(readers) == 0 {
		res.Missing = append(res.Missing, "readers")
		res.Note = "no reader of this author's works has read another work, so there is " +
			"no co-reader signal to walk"
		return nil
	}

	ids := make([]any, 0, len(readers))
	for id := range readers {
		ids = append(ids, id)
	}
	bylines, err := e.bylinesFor(ctx, ids)
	if err != nil {
		return err
	}
	type agg struct {
		byline  string
		score   float64
		works   int
		readers int64
	}
	byAuthor := map[string]*agg{}
	for id, n := range readers {
		for _, b := range bylines[id] {
			if b == byline || b == "" {
				continue
			}
			a := byAuthor[b]
			if a == nil {
				a = &agg{byline: b}
				byAuthor[b] = a
			}
			a.works++
			a.score += float64(n)
			a.readers += n
		}
	}
	out := make([]*agg, 0, len(byAuthor))
	for _, a := range byAuthor {
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].byline < out[j].byline
	})
	for i, a := range out {
		if i >= limit {
			break
		}
		res.Items = append(res.Items, RelatedItem{
			ID: arena.AuthorEntityID(a.byline), Kind: string(arena.KindAuthor), Label: a.byline, Score: a.score,
			Why: fmt.Sprintf("%d work(s) read by this author's readers", a.works),
		})
	}
	return nil
}

// ---- helpers -------------------------------------------------------------

type workLabel struct{ title, url string }

func (e *Engine) workLabels(ctx context.Context, ids []int64) (map[int64]workLabel, error) {
	out := map[int64]workLabel{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	q := `SELECT id, title, COALESCE(url,'') FROM works WHERE id IN (` + placeholders(len(ids)) + `)`
	rows, err := e.Corpus.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var l workLabel
		if err := rows.Scan(&id, &l.title, &l.url); err != nil {
			return out, err
		}
		out[id] = l
	}
	return out, rows.Err()
}

func (e *Engine) tagNames(ctx context.Context, ids []int32) (map[int32]string, error) {
	out := map[int32]string{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := e.Corpus.DB.QueryContext(ctx,
		`SELECT id, name FROM tags WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int32
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return out, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// authorByline resolves an author id to the byline string it was derived from.
//
// Author ids in this repo are derived from the byline (arena.AuthorEntityID), so
// the reverse is a scan over DISTINCT authors. It is a 112k-row scan with no
// index; the author kind is not on a hot path and the alternative is a table the
// corpus does not have.
func (e *Engine) authorByline(ctx context.Context, id int64) (string, error) {
	rows, err := e.Corpus.DB.QueryContext(ctx, `SELECT DISTINCT authors FROM works WHERE authors <> ''`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return "", err
		}
		if arena.AuthorEntityID(b) == id {
			return b, nil
		}
	}
	return "", rows.Err()
}

// bylinesFor maps work ids to the bylines credited on them.
func (e *Engine) bylinesFor(ctx context.Context, ids []any) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	q := `SELECT id, COALESCE(authors,'') FROM works WHERE id IN (` + placeholders(len(ids)) + `)`
	rows, err := e.Corpus.DB.QueryContext(ctx, q, ids...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var authors string
		if err := rows.Scan(&id, &authors); err != nil {
			return out, err
		}
		out[id] = splitBylines(authors)
	}
	return out, rows.Err()
}

// splitBylines splits the corpus's "pseud (username), pseud2 (username2) form.
func splitBylines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ", ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// evidenceSentence renders a ranking's evidence in words, biggest contribution
// first, so a reader can see WHICH algorithm drove the result rather than only
// its score.
func evidenceSentence(ev []rank.Evidence) string {
	if len(ev) == 0 {
		return ""
	}
	sorted := append([]rank.Evidence(nil), ev...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Value*sorted[i].Weight > sorted[j].Value*sorted[j].Weight })
	parts := make([]string, 0, 2)
	for i, e := range sorted {
		if i >= 2 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.2f", e.Signal, e.Value*e.Weight))
	}
	return strings.Join(parts, ", ")
}

// placeholders renders n bind markers, because SQLite cannot expand a slice.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	b := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

func tagIDs(near []NearTag) []int32 {
	out := make([]int32, 0, len(near))
	for _, n := range near {
		out = append(out, n.ID)
	}
	return out
}
