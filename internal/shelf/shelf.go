// Package shelf turns a reader's bookmarks into a shelf: the fandoms they would most enjoy, and
// the works inside each one they are most likely to read next.
//
// It exists because the recommender answers "given this work, what next", and the question a
// reader actually asks first is "what should I be reading at all". The two are not the same query:
// a seed-based recommendation is anchored to one work's neighbourhood, while a shelf has to
// compare entire FANDOMS -- 75,574 of them in the catalogue -- against a taste built from a
// handful of bookmarks.
//
// Three design decisions, each of which was a measurement rather than a preference:
//
//   - A fandom is identified by its TAG NAME matching the catalogue, not by
//     work_tags.tag_type='fandoms'. The mirror's pre-existing 112k works carry 'freeforms' for
//     every tag (the column was flattened by the tool that built it), so a fandom grouping built
//     on that column finds nothing. Names are the identity that survives the flattening:
//     5,336 catalogue fandoms are present in the mirror this way, against ~110 typed rows.
//
//   - A work's score is its taste tags' weight divided by 1+ln(number of tags). A sum alone
//     ranks by how many tags a work carries; a mean alone ranks one-tag works that happen to be
//     tagged with the reader's rarest tag above everything. The log denominator keeps both.
//
//   - A fandom's score is the MEAN OF ITS TOP 25 WORKS, not its mean or its best. Averaging all
//     its works punishes a big fandom for its tail; taking the single best lets one lucky work
//     represent 19,796 others. Top-25 is "what a reader would find if they browsed it".
package shelf

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/harvest"
)

// Options tunes the shelf. Zero values take the defaults in normalise.
type Options struct {
	// Fandoms is how many fandoms the shelf holds (100).
	Fandoms int
	// PerFandom is how many works inside each fandom (200).
	PerFandom int
	// MinFandomWorks is the floor of works a fandom needs IN THE MIRROR to be ranked at all: a
	// fandom with one work in the mirror has no evidence, only an anecdote, and its score would
	// be that single work's score with a confident-looking number beside it.
	MinFandomWorks int
	// TopWorks is how many of a fandom's works set its score (25).
	TopWorks int
	// Lambda blends relevance with diversity inside a fandom (0.75: measured on this corpus).
	Lambda float64
	// PoolPerFandom is how many of a fandom's best-scoring works enter the diversity pass (400).
	// Twice PerFandom is the practical minimum: MMR discards near-duplicates, so a pool exactly
	// PerFandom long returns fewer than PerFandom picks.
	PoolPerFandom int
}

func (o *Options) normalise() {
	if o.Fandoms <= 0 {
		o.Fandoms = 100
	}
	if o.PerFandom <= 0 {
		o.PerFandom = 200
	}
	if o.MinFandomWorks <= 0 {
		o.MinFandomWorks = 3
	}
	if o.TopWorks <= 0 {
		o.TopWorks = 25
	}
	if o.Lambda <= 0 || o.Lambda > 1 {
		o.Lambda = 0.75
	}
	if o.PoolPerFandom <= 0 {
		o.PoolPerFandom = o.PerFandom * 2
	}
}

// Fandom is one shelf entry.
type Fandom struct {
	Name string `json:"name"`
	// Path is AO3's URL segment for the tag, needed to harvest this fandom's missing works.
	Path string `json:"path,omitempty"`
	// CatalogueWorks is AO3's own count, from the harvest catalogue.
	CatalogueWorks int `json:"catalogue_works"`
	// MirrorWorks is how many works of this fandom the local corpus holds.
	MirrorWorks int `json:"mirror_works"`
	// Bookmarked is how many of the reader's own bookmarks are in this fandom.
	Bookmarked int `json:"bookmarked"`
	// Score is the mean of the fandom's top works' taste scores.
	Score float64 `json:"score"`
	// Why names the reader's tags this fandom over-indexes on, and the bookmark count that
	// anchors it. A ranking with no evidence beside it cannot be argued with, which is the
	// failure mode of every opaque recommender.
	Why     []string `json:"why"`
	Works   []Work   `json:"works"`
	ShortBy int      `json:"short_by,omitempty"`
}

// Work is one fic on the shelf.
type Work struct {
	ID       int64    `json:"id"`
	Title    string   `json:"title"`
	Authors  string   `json:"authors"`
	Rating   string   `json:"rating"`
	Words    int      `json:"words"`
	Kudos    int      `json:"kudos"`
	Complete bool     `json:"complete"`
	Language string   `json:"language,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	URL      string   `json:"url"`
	Score    float64  `json:"score"`
	Tags     []string `json:"tags,omitempty"`
}

// Result is the whole shelf.
type Result struct {
	Reader     string     `json:"reader"`
	Bookmarks  int        `json:"bookmarks"`
	TasteTags  []TasteTag `json:"taste_tags"`
	Fandoms    []Fandom   `json:"fandoms"`
	TotalWorks int        `json:"total_works"`
	Notes      []string   `json:"notes,omitempty"`
}

// TasteTag is one tag of the reader's profile, with the weight it carried.
type TasteTag struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Works  int     `json:"works"`
}

// Build scores every fandom in the catalogue and fills the shelf.
//
// reader is the set of user ids that belong to the reader (the mirror can hold two rows for one
// AO3 name, differing only in case -- this account does).
func Build(ctx context.Context, db *sql.DB, cat *harvest.Catalogue, reader []int64, opt Options) (*Result, error) {
	opt.normalise()
	if len(reader) == 0 {
		return nil, fmt.Errorf("shelf: no reader ids")
	}
	if cat == nil || len(cat.Fandoms) == 0 {
		return nil, fmt.Errorf("shelf: no fandom catalogue")
	}
	res := &Result{Fandoms: []Fandom{}}

	bookmarks, err := readerWorks(ctx, db, reader)
	if err != nil {
		return nil, err
	}
	res.Bookmarks = len(bookmarks)
	if len(bookmarks) == 0 {
		return nil, fmt.Errorf("shelf: this reader has no bookmarked works in the mirror")
	}

	if err := buildTaste(ctx, db, bookmarks); err != nil {
		return nil, err
	}
	tags, err := tasteTags(ctx, db)
	if err != nil {
		return nil, err
	}
	res.TasteTags = tags

	if err := loadCatalogue(ctx, db, cat); err != nil {
		return nil, err
	}
	if err := scoreWorks(ctx, db); err != nil {
		return nil, err
	}
	if err := linkFandoms(ctx, db, bookmarks); err != nil {
		return nil, err
	}

	fandoms, err := rankFandoms(ctx, db, opt)
	if err != nil {
		return nil, err
	}
	if len(fandoms) == 0 {
		res.Notes = append(res.Notes,
			fmt.Sprintf("no fandom in the mirror holds %d works; lower -min-fandom-works", opt.MinFandomWorks))
		return res, nil
	}
	// AO3 tags a work with every name a fandom answers to, and the mirror keeps those names
	// verbatim: "highschool dxd (anime)" and "ハイスクール dxd - 石踏 一榮 | high school dxd -
	// ishibumi ichiei" are one fandom in two spellings, and ASOIAF arrives three ways. Left
	// alone, a 100-fandom shelf is 100 slots spent on maybe 90 fandoms -- so a fandom whose top
	// works mostly sit inside a higher-ranked fandom's is dropped and the slot goes to the next.
	fandoms, dropped, err := dropVariants(ctx, db, fandoms, opt)
	if err != nil {
		return nil, err
	}
	for _, d := range dropped {
		res.Notes = append(res.Notes, d)
	}
	if len(fandoms) > opt.Fandoms {
		fandoms = fandoms[:opt.Fandoms]
	}

	for i := range fandoms {
		works, err := fillFandom(ctx, db, fandoms[i].Name, opt)
		if err != nil {
			return nil, fmt.Errorf("shelf %s: %w", fandoms[i].Name, err)
		}
		fandoms[i].Works = works
		if len(works) < opt.PerFandom {
			fandoms[i].ShortBy = opt.PerFandom - len(works)
		}
		res.TotalWorks += len(works)
	}
	res.Fandoms = fandoms
	return res, nil
}

// readerWorks is the reader's bookmarked works: every row for every id given, because the same
// AO3 account can appear twice under different capitalisation.
func readerWorks(ctx context.Context, db *sql.DB, reader []int64) ([]int64, error) {
	q := `SELECT DISTINCT work_id FROM user_work_interactions WHERE user_id IN (` + placeholders(len(reader)) + `)`
	rows, err := db.QueryContext(ctx, q, toArgs(reader)...)
	if err != nil {
		return nil, fmt.Errorf("reader works: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// buildTaste writes the reader's tag weights into a temp table.
//
// The weight is frequency in the reader's bookmarks divided by how common the tag is in the
// archive (1+ln(df)). Without the divisor a fandom is "similar" to the reader because both are
// tagged 'm/m' -- a tag on a third of the archive says nothing about anyone. This is idf, and it
// is the difference between a shelf of Harry Potter crossovers and a shelf of everything.
func buildTaste(ctx context.Context, db *sql.DB, bookmarks []int64) error {
	stmts := []string{
		`DROP TABLE IF EXISTS temp.taste`,
		`DROP TABLE IF EXISTS temp.tag_df`,
		`CREATE TEMP TABLE tag_df AS SELECT tag_id, COUNT(DISTINCT work_id) AS df FROM work_tags GROUP BY tag_id`,
		`CREATE TEMP TABLE taste(tag_id INTEGER PRIMARY KEY, w REAL NOT NULL)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("taste setup: %w", err)
		}
	}
	args := toArgs(bookmarks)
	_, err := db.ExecContext(ctx, `
		INSERT INTO taste(tag_id, w)
		SELECT wt.tag_id,
		       CAST(COUNT(*) AS REAL) / (1.0 + LN(1.0 + MAX(df.df)))
		FROM work_tags wt JOIN tag_df df ON df.tag_id = wt.tag_id
		WHERE wt.work_id IN (`+placeholders(len(bookmarks))+`)
		GROUP BY wt.tag_id`, args...)
	if err != nil {
		return fmt.Errorf("taste weights: %w", err)
	}
	// Normalise to the strongest tag: the absolute scale of these weights is meaningless, and a
	// score built from unnormalised ones changes when the reader adds a bookmark.
	if _, err := db.ExecContext(ctx, `
		UPDATE taste SET w = w / COALESCE((SELECT MAX(w) FROM taste), 1.0)`); err != nil {
		return fmt.Errorf("taste normalise: %w", err)
	}
	return nil
}

// tasteTags reads the heaviest profile tags back, for the report and for a fandom's "why".
func tasteTags(ctx context.Context, db *sql.DB) ([]TasteTag, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.name, ta.w FROM taste ta JOIN tags t ON t.id = ta.tag_id
		ORDER BY ta.w DESC, t.name LIMIT 40`)
	if err != nil {
		return nil, fmt.Errorf("taste tags: %w", err)
	}
	defer rows.Close()
	var out []TasteTag
	for rows.Next() {
		var tt TasteTag
		if err := rows.Scan(&tt.Name, &tt.Weight); err != nil {
			return nil, err
		}
		out = append(out, tt)
	}
	return out, rows.Err()
}

// loadCatalogue puts the catalogue's fandom names in a temp table so a tag can be recognised as a
// fandom by name, whatever its stored type says.
func loadCatalogue(ctx context.Context, db *sql.DB, cat *harvest.Catalogue) error {
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS temp.cf`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TEMP TABLE cf(name TEXT PRIMARY KEY, works INTEGER NOT NULL, path TEXT)`); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO cf(name, works, path) VALUES (lower(?), ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, f := range cat.Fandoms {
		if _, err := ins.ExecContext(ctx, f.Name, f.Works, f.Path); err != nil {
			return fmt.Errorf("catalogue %s: %w", f.Name, err)
		}
	}
	return tx.Commit()
}

// scoreWorks scores every work the reader's taste touches.
func scoreWorks(ctx context.Context, db *sql.DB) error {
	for _, st := range []string{`DROP TABLE IF EXISTS temp.work_scores`, `DROP TABLE IF EXISTS temp.work_tagcount`} {
		if _, err := db.ExecContext(ctx, st); err != nil {
			return err
		}
	}
	// The denominator is the work's TOTAL tag count, not the number of the reader's tags it
	// happens to carry. Dividing by the matches made the guard a no-op -- a work carrying every
	// taste tag plus forty irrelevant ones scored exactly what the same work without them scored,
	// which is the tag-stuffing the denominator exists to punish.
	if _, err := db.ExecContext(ctx, `
		CREATE TEMP TABLE work_tagcount AS
		SELECT work_id, COUNT(DISTINCT tag_id) AS n FROM work_tags GROUP BY work_id`); err != nil {
		return fmt.Errorf("work tag counts: %w", err)
	}
	// Ordered by score and indexed: the per-fandom selection reads only the top of it, and a
	// fandom query re-reads it 100 times.
	if _, err := db.ExecContext(ctx, `
		CREATE TEMP TABLE work_scores AS
		SELECT wt.work_id AS work_id,
		       SUM(ta.w) / (1.0 + LN(COALESCE(tc.n, 1))) AS s
		FROM work_tags wt
		JOIN taste ta ON ta.tag_id = wt.tag_id
		LEFT JOIN work_tagcount tc ON tc.work_id = wt.work_id
		GROUP BY wt.work_id`); err != nil {
		return fmt.Errorf("work scores: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX temp.work_scores_s ON work_scores(s DESC, work_id)`); err != nil {
		return err
	}
	return nil
}

// linkFandoms maps every fandom to its works, and counts the reader's bookmarks per fandom.
func linkFandoms(ctx context.Context, db *sql.DB, bookmarks []int64) error {
	for _, s := range []string{`DROP TABLE IF EXISTS temp.fw`, `DROP TABLE IF EXISTS temp.mine`} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TEMP TABLE fw AS
		SELECT cf.name AS fandom, wt.work_id AS work_id
		FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		JOIN cf ON cf.name = lower(t.name)`); err != nil {
		return fmt.Errorf("link fandoms: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX temp.fw_f ON fw(fandom, work_id)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TEMP TABLE mine(fandom TEXT PRIMARY KEY, n INTEGER NOT NULL)`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO mine(fandom, n)
		SELECT fandom, COUNT(DISTINCT work_id) FROM fw
		WHERE work_id IN (`+placeholders(len(bookmarks))+`) GROUP BY fandom`, toArgs(bookmarks)...)
	if err != nil {
		return fmt.Errorf("bookmark fanoms: %w", err)
	}
	return nil
}

// rankFandoms scores each fandom by the mean of its top works and returns them best first.
func rankFandoms(ctx context.Context, db *sql.DB, opt Options) ([]Fandom, error) {
	rows, err := db.QueryContext(ctx, `
		WITH sized AS (
			SELECT fandom, COUNT(DISTINCT work_id) AS n FROM fw GROUP BY fandom
		), top AS (
			SELECT fw.fandom, ws.s,
			       ROW_NUMBER() OVER (PARTITION BY fw.fandom ORDER BY ws.s DESC, fw.work_id) AS rn
			FROM fw JOIN work_scores ws ON ws.work_id = fw.work_id
		), scored AS (
			SELECT fandom, AVG(s) AS score, COUNT(*) AS considered
			FROM top WHERE rn <= ? GROUP BY fandom
		)
		SELECT s.fandom, s.n, sc.score, COALESCE(c.works, 0), COALESCE(c.path, ''),
		       COALESCE(m.n, 0)
		FROM sized s
		JOIN scored sc ON sc.fandom = s.fandom
		JOIN cf c ON c.name = s.fandom
		LEFT JOIN mine m ON m.fandom = s.fandom
		WHERE s.n >= ?
		ORDER BY sc.score DESC, s.n DESC
		LIMIT ?`, opt.TopWorks, opt.MinFandomWorks, opt.Fandoms*3)
	if err != nil {
		return nil, fmt.Errorf("rank fandoms: %w", err)
	}
	defer rows.Close()
	var out []Fandom
	for rows.Next() {
		var f Fandom
		if err := rows.Scan(&f.Name, &f.MirrorWorks, &f.Score, &f.CatalogueWorks, &f.Path, &f.Bookmarked); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// dropVariants removes fandoms that are spelling variants of one already accepted.
//
// The test is mechanical, not semantic: two fandoms are the same if half of the smaller one's top
// works are the larger one's. That is a measurement of the corpus rather than a judgement about
// names, so it needs no list of aliases to maintain -- and it fires on exactly the case that made
// it necessary (a fandom whose works are a subset of another's).
func dropVariants(ctx context.Context, db *sql.DB, fandoms []Fandom, opt Options) ([]Fandom, []string, error) {
	if len(fandoms) == 0 {
		return fandoms, nil, nil
	}
	const sigCap = 300
	names := make([]string, len(fandoms))
	for i, f := range fandoms {
		names[i] = f.Name
	}
	q := `SELECT fw.fandom, fw.work_id, ws.s FROM fw JOIN work_scores ws ON ws.work_id = fw.work_id
	      WHERE fw.fandom IN (` + placeholders(len(names)) + `)
	      ORDER BY fw.fandom, ws.s DESC`
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("fandom signatures: %w", err)
	}
	defer rows.Close()
	sig := make(map[string]map[int64]struct{}, len(names))
	for rows.Next() {
		var fandom string
		var workID int64
		var score float64
		if err := rows.Scan(&fandom, &workID, &score); err != nil {
			return nil, nil, err
		}
		set := sig[fandom]
		if set == nil {
			set = map[int64]struct{}{}
			sig[fandom] = set
		}
		if len(set) < sigCap {
			set[workID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	kept := make([]Fandom, 0, opt.Fandoms)
	var notes []string
	for _, f := range fandoms {
		dup := -1
		for i, k := range kept {
			if overlapRatio(sig[f.Name], sig[k.Name]) > 0.5 {
				dup = i
				break
			}
		}
		if dup >= 0 {
			// The BROADER fandom keeps the slot, not the higher-scoring one. A variant can score
			// higher precisely because it is narrower: its few works all carry the reader's tags,
			// while the fandom it is a spelling of also holds everything else. Keeping the
			// narrower one would hand the reader a shelf slot that cannot hold 200 works.
			if f.MirrorWorks > kept[dup].MirrorWorks {
				notes = append(notes, fmt.Sprintf("%q dropped: it is a variant of %q, which holds %d works",
					kept[dup].Name, f.Name, f.MirrorWorks))
				kept[dup] = f
			} else {
				notes = append(notes, fmt.Sprintf("%q dropped: its works are a variant of %q",
					f.Name, kept[dup].Name))
			}
			continue
		}
		kept = append(kept, f)
		// No early break at opt.Fandoms: a variant can arrive AFTER the list is already full and
		// REPLACE the narrower entry it is a spelling of (a fandom with 7 works beats one with 3
		// even when the 3 scored higher). Stopping at the quota kept the narrow one -- which is
		// how a one-fandom shelf came back holding the variant.
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Score > kept[j].Score })
	return kept, notes, nil
}

// overlapRatio is |A and B| / min(|A|,|B|): half of the SMALLER set, because the question is
// whether one fandom's works are contained in the other's, not whether they are the same size.
func overlapRatio(a, b map[int64]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small := len(a)
	if len(b) < small {
		small = len(b)
	}
	inter := 0
	for id := range a {
		if _, ok := b[id]; ok {
			inter++
		}
	}
	return float64(inter) / float64(small)
}

// fillFandom picks the works a reader would most likely read inside one fandom.
func fillFandom(ctx context.Context, db *sql.DB, fandom string, opt Options) ([]Work, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT w.id, w.title, w.authors, COALESCE(w.rating, ''), COALESCE(w.word_count, 0),
		       COALESCE(w.kudos, 0), COALESCE(w.complete, 0), COALESCE(w.language, ''),
		       COALESCE(w.summary, ''), w.url, ws.s
		FROM fw JOIN work_scores ws ON ws.work_id = fw.work_id
		JOIN works w ON w.id = ws.work_id
		WHERE fw.fandom = ?
		-- A taste score alone puts works nobody has read at the top: the reader's rarest tags are
		-- exactly the ones an unread work can carry. That is not a defect for THIS reader (their
		-- bookmarks include works with 1-3 kudos), so popularity is a mild blend rather than a
		-- gate: 25% of the ordering, log-scaled so a 10,000-kudos work cannot flatten a fandom.
		ORDER BY ws.s * (1.0 + 0.25 * LN(1.0 + COALESCE(w.kudos, 0)) / 8.0) DESC, w.id
		LIMIT ?`, fandom, opt.PoolPerFandom)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pool []Work
	for rows.Next() {
		var w Work
		var complete int
		if err := rows.Scan(&w.ID, &w.Title, &w.Authors, &w.Rating, &w.Words, &w.Kudos,
			&complete, &w.Language, &w.Summary, &w.URL, &w.Score); err != nil {
			return nil, err
		}
		w.Complete = complete != 0
		pool = append(pool, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return nil, nil
	}
	tagSets, err := workTags(ctx, db, pool)
	if err != nil {
		return nil, err
	}
	return mmr(pool, tagSets, opt.PerFandom, opt.Lambda), nil
}

// workTags loads the tag names of the candidate works, for diversity and for display.
func workTags(ctx context.Context, db *sql.DB, pool []Work) (map[int64][]string, error) {
	ids := make([]int64, len(pool))
	for i, w := range pool {
		ids[i] = w.ID
	}
	rows, err := db.QueryContext(ctx, `
		SELECT wt.work_id, t.name FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id IN (`+placeholders(len(ids))+`)`, toArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]string, len(ids))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// mmr selects k works, blending score with novelty against what is already chosen.
//
// Without it a fandom's 200 works are its most-tagged works, and the second hundred are the
// first hundred of a neighbouring fandom: the same AU with the same pairing, tagged twice as
// thoroughly. Lambda 0.75 is the value this corpus measured for the recommender's own lists.
func mmr(pool []Work, tagSets map[int64][]string, k int, lambda float64) []Work {
	if k <= 0 || len(pool) == 0 {
		return nil
	}
	if k > len(pool) {
		k = len(pool)
	}
	top := pool[0].Score
	if top <= 0 {
		top = 1
	}
	chosen := make([]Work, 0, k)
	taken := make([]bool, len(pool))
	skipped := make([]bool, len(pool))
	// Only the neighbours of what is already chosen matter, so similarity accumulates per
	// candidate instead of being recomputed over every pair at every step.
	penalty := make([]float64, len(pool))
	for len(chosen) < k {
		best, bestScore := -1, -1.0
		for i := range pool {
			if taken[i] {
				continue
			}
			s := lambda*(pool[i].Score/top) - (1-lambda)*penalty[i]
			if s > bestScore {
				best, bestScore = i, s
			}
		}
		if best < 0 {
			break
		}
		taken[best] = true
		picked := pool[best]
		picked.Tags = tagSets[picked.ID]
		chosen = append(chosen, picked)
		// Raise every remaining candidate's penalty by its overlap with the new pick. Kept as a
		// MAX rather than a sum: a near-duplicate of ANY chosen work is redundant, and a sum
		// would let twenty mediocre overlaps outrank one exact duplicate.
		for i := range pool {
			if taken[i] {
				continue
			}
			j := jaccard(tagSets[pool[i].ID], tagSets[picked.ID])
			// A near-identical work is EXCLUDED, not merely penalised. A penalty only decides an
			// order among works of comparable relevance, so on a 200-item page the same AU
			// tagged twice over still arrives -- just later. On a shelf the duplicate is worth
			// nothing at any position, so a work sharing ~all its tags with something already
			// picked is dropped (and counted).
			if j >= duplicateCut {
				taken[i] = true
				skipped[i] = true
				continue
			}
			if j > penalty[i] {
				penalty[i] = j
			}
		}
	}
	return chosen
}

// duplicateCut is the tag overlap at which one work is dropped because another already picked
// says the same thing. 0.9 rather than 0.8: a work with a genuinely different ship, warning or
// trope still lands under 0.8, and a fandom page that refuses those is not a shelf either.
const duplicateCut = 0.9

// jaccard is the tag-set overlap of two works.
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(a))
	for _, t := range a {
		set[strings.ToLower(t)] = struct{}{}
	}
	inter := 0
	seen := make(map[string]struct{}, len(b))
	for _, t := range b {
		lt := strings.ToLower(t)
		if _, dup := seen[lt]; dup {
			continue
		}
		seen[lt] = struct{}{}
		if _, ok := set[lt]; ok {
			inter++
		}
	}
	union := len(set) + len(seen) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Why explains a fandom: the profile tags its works carry most above their archive rate.
//
// The rate is a LIFT, not a count: a tag on 40 works of a 2,000-work fandom is 2% of that fandom,
// and if it is 0.2% of the archive it is ten times over-represented there. A count alone would
// put the archive's own boilerplate ('m/m') at the top of every fandom's explanation.
func Why(ctx context.Context, db *sql.DB, fandom string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 3
	}
	var fandomWorks, corpusWorks int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT work_id) FROM fw WHERE fandom = ?`, fandom).
		Scan(&fandomWorks); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&corpusWorks); err != nil {
		return nil, err
	}
	if fandomWorks == 0 || corpusWorks == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.name, COUNT(DISTINCT fw.work_id) AS here, MAX(df.df) AS df
		FROM fw JOIN work_tags wt ON wt.work_id = fw.work_id
		JOIN tag_df df ON df.tag_id = wt.tag_id
		JOIN tags t ON t.id = wt.tag_id
		JOIN taste ta ON ta.tag_id = wt.tag_id
		WHERE fw.fandom = ?
		GROUP BY t.name
		HAVING here >= 3
		ORDER BY (here * 1.0 / MAX(df.df)) DESC
		LIMIT ?`, fandom, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		var here, df int
		if err := rows.Scan(&name, &here, &df); err != nil {
			return nil, err
		}
		if df == 0 {
			continue
		}
		share := float64(here) / float64(fandomWorks)
		archive := float64(df) / float64(corpusWorks)
		out = append(out, fmt.Sprintf("%s (%.1fx the archive rate)", name, share/archive))
	}
	return out, rows.Err()
}

// AuthorsFrom aggregates the shelf's authors, weighted by how many works they have on it and how
// strongly those works scored: an author with eight works on the shelf is a better answer than
// one with a single high-scoring work.
func AuthorsFrom(res *Result, limit int) []AuthorRow {
	type agg struct {
		works int
		score float64
		best  []string
	}
	by := map[string]*agg{}
	for _, f := range res.Fandoms {
		for _, w := range f.Works {
			for _, a := range strings.Split(w.Authors, ",") {
				a = strings.TrimSpace(a)
				if a == "" || strings.EqualFold(a, "Anonymous") || strings.EqualFold(a, "orphan_account") {
					continue
				}
				e := by[a]
				if e == nil {
					e = &agg{}
					by[a] = e
				}
				e.works++
				e.score += w.Score
				if !containsStr(e.best, f.Name) && len(e.best) < 3 {
					e.best = append(e.best, f.Name)
				}
			}
		}
	}
	out := make([]AuthorRow, 0, len(by))
	for name, e := range by {
		out = append(out, AuthorRow{Name: name, Works: e.works, Score: e.score, Fandoms: e.best})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Works != out[j].Works {
			return out[i].Works > out[j].Works
		}
		if math.Abs(out[i].Score-out[j].Score) > 1e-9 {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// AuthorRow is one author on the shelf.
type AuthorRow struct {
	Name    string   `json:"name"`
	Works   int      `json:"works"`
	Score   float64  `json:"score"`
	Fandoms []string `json:"fandoms,omitempty"`
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
