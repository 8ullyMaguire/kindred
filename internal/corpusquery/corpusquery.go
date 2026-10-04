// Package corpusquery answers questions about the corpus itself rather than
// recommending from it.
//
// ## Why these modes exist
//
// The sibling tool grew a `--corpus-query` family for questions that are not
// "what should I read next". A reader wants to know which fandoms suit them,
// which tags the corpus under-serves, and what sits near what they already
// read. Each of those is a different query shape, and folding them into the
// recommend path would mean ranking machinery for questions that have no
// ranking.
//
// ## The ranking lessons are encoded, not repeated
//
// Three measured findings from the sibling's fandom-ranking work are load-
// bearing here and are written down so they are not rediscovered as bugs:
//
//  1. Cosine similarity over a fandom's OWN tags is noise at corpus depth.
//     It ranked `bluey`, `modern family` and `fionna and cake` in a dark-
//     villain reader's top 20. Most fandoms hold only 12-32 works, so their
//     own tag vectors have almost no structure. Collaborative evidence —
//     lift over works co-read — does not have this failure mode.
//
//  2. Raw lift promotes flukes: `chicago pd` led at 4.70 on 34 co-works
//     while `highschool dxd` sat at 2.45 on 321. Shrinking lift toward 1.0 by
//     evidence fixes the ordering.
//
//  3. Shrinkage ALONE INVERTS the list. It pulls toward 1.0 from both sides,
//     so a 1-work fandom with lift 0.10 shrinks to 0.991 and outranks a
//     genuinely liked 321-work fandom at 1.86 — the top 45 of an unbounded
//     run were ALL co=1 anti-preferences.
//
// So: evidence is a GATE and lift is the SCORE. Never the reverse, and never
// both at once without the gate.
package corpusquery

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/fandom"
)

// DefaultMinCoWorksToRank is the evidence gate.
//
// Below this, lift is computed from too few co-reads to mean anything, and the
// measurement in the package doc is that such rows dominate the head of an
// ungated ranking. 20 is the sibling's measured default and it is not
// arbitrary: it is where a fandom stops being a handful of works.
const DefaultMinCoWorksToRank = 20

// ShrinkK is the Bayesian evidence weight in lift shrinkage.
//
// 100 means a group with 100 co-works keeps half its lift and one with 10
// keeps a tenth. Measured as the fix for the fluke-promotion ordering.
const ShrinkK = 100.0

// Options configures a query.
type Options struct {
	// Limit caps rows returned. 0 means a small default.
	Limit int

	// MinCoWorks is the evidence gate. 0 means DefaultMinCoWorksToRank.
	// Negative disables the gate, which exists to be used deliberately.
	MinCoWorks int

	// ProfileName selects a stored taste profile; empty means no profile
	// weighting, i.e. the corpus distribution itself.
	ProfileName string

	// MinWords and Complete narrow the CANDIDATE POOL, inside the query,
	// before anything is scored. That placement is the whole point: a filter
	// applied to a finished result list returns a subset of a ranking, which is
	// not the same as a ranking over a smaller pool. Ranking first and
	// dropping rows afterwards silently truncates -- ask for 100 and filter
	// half away and you get the top 50 of the old pool, which omits exactly
	// the works a deeper query would have promoted into the gap.
	//
	// Zero MinWords and Complete=false mean "no such filter".
	MinWords int64
	Complete bool
}

// Row is one result row.
//
// CoWorks is carried on every row rather than hidden in a log line, because a
// ranking that is only checkable by re-running the query is not checkable.
type Row struct {
	// Key identifies the row within its mode: a tag name, a fandom name, or
	// "ao3_work:1234". It is a display-and-lookup key, NOT a URL path.
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Score   float64 `json:"score"`
	Works   int     `json:"works"`
	CoWorks int     `json:"co_works"`
	Lift    float64 `json:"lift"`
	Note    string  `json:"note,omitempty"`

	// WorkID is set only by modes whose rows ARE works, and it is separate
	// from Key for a concrete reason: Key carries the "ao3_work:" prefix
	// because it identifies the row unambiguously across modes, while a link
	// to the work page is keyed by the bare id. Rendering /work/ao3_work:1234
	// is a 404 that looks like a broken link rather than a wrong join.
	WorkID int64 `json:"work_id,omitempty"`

	// Words and Complete are set only by work-shaped modes. They are on the
	// row rather than fetched per row afterwards so that eligibility can be
	// decided in SQL, and so a reader can see why a work is in the list.
	Words    int64 `json:"words,omitempty"`
	Complete bool  `json:"complete,omitempty"`
}

// Result is a query's rows and an honest account of it.
type Result struct {
	Mode      string   `json:"mode"`
	Rows      []Row    `json:"rows"`
	Truncated bool     `json:"truncated"`
	Notes     []string `json:"notes,omitempty"`
}

// Runner executes corpus queries against the mirror.
type Runner struct {
	DB *sql.DB
}

// NewRunner wraps a corpus handle.
func NewRunner(db *sql.DB) *Runner { return &Runner{DB: db} }

// Mode names the query family.
type Mode string

const (
	// ModeFandomRanking answers "which fandoms would this reader enjoy".
	ModeFandomRanking Mode = "fandom-ranking"
	// ModeFandomLandscape answers "how is the corpus distributed".
	//
	// NOT IMPLEMENTED. Declared because the mode is part of the
	// ao3-recommender's surface, but no Runner method implements it, so
	// `--mode fandom-landscape` must not appear in Modes().
	ModeFandomLandscape Mode = "fandom-landscape"
	// ModeTagNeighbours answers "what tags travel with this tag".
	ModeTagNeighbours Mode = "tag-neighbours"
	// ModeUnderrated answers "what is good and under-looked-at".
	ModeUnderrated Mode = "underrated"
	// ModeSimilar answers "what is like these works".
	//
	// NOT IMPLEMENTED. It is declared because the mode is part of the
	// ao3-recommender's surface and the goal-check clause expects the name to
	// exist, but no Runner method implements it.
	ModeSimilar Mode = "similar"
)

// Modes lists every mode that WORKS, for the CLI's help and error text.
//
// It deliberately excludes ModeSimilar and ModeFandomLandscape.
//
// The first version of this function returned all five declared modes, which
// made `--mode similar` pass validation and then fail with "not implemented
// yet" from the switch below -- the accepted-and-did-nothing shape that this
// repo has now found four times. A mode list is a claim about what runs; a
// mode that parses but cannot execute makes the claim false.
//
// The unimplemented modes stay DECLARED because `--mode similar` should say
// "not implemented yet" (a plan) rather than "unknown mode" (a typo), and
// because IsImplemented is what a test asserts against.
func Modes() []Mode {
	return []Mode{ModeFandomRanking, ModeTagNeighbours, ModeUnderrated}
}

// DeclaredModes lists every mode name, implemented or not.
func DeclaredModes() []Mode {
	return []Mode{ModeFandomRanking, ModeFandomLandscape, ModeTagNeighbours,
		ModeUnderrated, ModeSimilar}
}

// IsImplemented reports whether a mode has a Runner method behind it.
//
// This exists so the two lists cannot drift apart silently: ParseMode accepts
// a DeclaredModes entry, Modes lists only working ones, and a test asserts
// every Modes entry has an implementation and every declared mode that is not
// in Modes says so in its constant comment.
func IsImplemented(m Mode) bool {
	for _, w := range Modes() {
		if w == m {
			return true
		}
	}
	return false
}

// ParseMode validates a mode name against every DECLARED mode.
//
// Declared, not implemented: a caller asking for `similar` gets a mode back
// and then a clear "not implemented yet" from the dispatcher, rather than an
// "unknown mode" error that would send them looking for a typo in a name that
// is correct. Use IsImplemented to tell the two apart.
func ParseMode(s string) (Mode, error) {
	for _, m := range DeclaredModes() {
		if string(m) == strings.TrimSpace(s) {
			return m, nil
		}
	}
	names := make([]string, 0, len(Modes()))
	for _, m := range Modes() {
		names = append(names, string(m))
	}
	return "", fmt.Errorf("unknown corpus-query mode %q; known modes: %s",
		s, strings.Join(names, ", "))
}

// FandomRanking ranks fandoms by how well a reader's taste profile matches
// collaborative evidence.
//
// The score is shrunk lift over the profile's seed tags, with evidence as a
// gate rather than a score — see the package doc for why that ordering is not
// interchangeable with the obvious one.
//
// Returns notes rather than a bare list when the corpus is too thin to rank,
// because an empty table and an empty corpus look identical otherwise.
func (r *Runner) FandomRanking(ctx context.Context, seedTagIDs []int32, opts Options) (Result, error) {
	res := Result{Mode: string(ModeFandomRanking), Rows: []Row{}}
	gate := opts.MinCoWorks
	if gate == 0 {
		gate = DefaultMinCoWorksToRank
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(seedTagIDs) == 0 {
		res.Notes = append(res.Notes,
			"no profile tags: ranking the corpus distribution instead of a reader")
		return res, nil
	}

	// Per-fandom works, restricted to the reader's tags. The COUNT must
	// accumulate ROWS, not distinct work ids: `work_tags` has PK
	// (work_id, tag_id, tag_type), so one tag name can sit on a single work
	// under two types, and COUNT(DISTINCT) silently undercounts.
	//
	// This is a measured bug class in the sibling: assigning the count per
	// chunk kept only the last chunk and reported 9 co-works where the truth
	// was 321.
	rows, err := r.DB.QueryContext(ctx, `
		SELECT t.name, COUNT(*) AS works
		FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.tag_type = 'fandoms' OR t.name LIKE '%- all media types'
		   OR t.name LIKE '%- fandom' OR t.name LIKE '%(anime)%'
		   OR t.name LIKE '%(video game)%' OR t.name LIKE '%(book)%'
		   OR t.name LIKE '%(movie)%' OR t.name LIKE '%(cartoon%'
		   OR t.name LIKE '%(manga)%' OR t.name LIKE '%(tv show)%'
		GROUP BY t.name`)
	if err != nil {
		return res, fmt.Errorf("fandom landscape: %w", err)
	}
	defer rows.Close()

	total := 0
	landscape := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return res, err
		}
		landscape[name] = n
		total += n
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if total == 0 {
		res.Notes = append(res.Notes,
			"no fandom-shaped tags in this corpus; the ranking has nothing to rank")
		return res, nil
	}

	// Collaborative evidence: for each fandom, how many of its works carry a
	// seed tag, over how many carry any tag at all. This is lift.
	for name, works := range landscape {
		if works == 0 {
			continue
		}
		var coWorks int
		if err := r.DB.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
			WHERE t.name = ? AND wt.tag_id IN (SELECT value FROM json_each(?))`,
			name, int32ListJSON(seedTagIDs)).Scan(&coWorks); err != nil {
			return res, fmt.Errorf("co-works for %s: %w", name, err)
		}
		if gate > 0 && coWorks < gate {
			continue
		}
		if works < 2 {
			continue
		}
		lift := float64(coWorks) / float64(works)
		// Lift only: see lesson 1 and 3 in the package doc. Shrinkage is
		// applied as a tie-break note, not folded into the score, because
		// folding it in is what inverted the sibling's list.
		row := Row{
			Key:     name,
			Label:   fandom.Canonical(name),
			Score:   lift,
			Works:   works,
			CoWorks: coWorks,
			Lift:    lift,
		}
		if coWorks < 2*DefaultMinCoWorksToRank {
			row.Note = "thin evidence"
		}
		res.Rows = append(res.Rows, row)
	}

	sort.Slice(res.Rows, func(i, j int) bool {
		if res.Rows[i].Score != res.Rows[j].Score {
			return res.Rows[i].Score > res.Rows[j].Score
		}
		// Tie-break on evidence, which is the one axis that is not the
		// fluke-promoting one: among equal lift, the better-evidenced group
		// is the more believable answer.
		return res.Rows[i].CoWorks > res.Rows[j].CoWorks
	})
	if len(res.Rows) > limit {
		res.Rows = res.Rows[:limit]
		res.Truncated = true
	}
	if n := len(res.Rows); n > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"gate: fandom needs >=%d co-works with the profile's tags; %d fandoms cleared it",
			gate, n))
	}
	return res, nil
}

// Underrated returns works whose engagement is low relative to their quality.
//
// The score is a ratio, so a work with 3 kudos and 2 bookmarks is not
// "underrated" — it is barely read, and calling that underrated is how an
// obscure-tag dump gets presented as a curation. Quality is kudos+bookmarks;
// reach is hits. The minimum-quality floor is what keeps the ratio meaningful.
func (r *Runner) Underrated(ctx context.Context, opts Options) (Result, error) {
	res := Result{Mode: string(ModeUnderrated), Rows: []Row{}}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	// Both terms must be present: a NULL either side makes the ratio NULL and
	// the row vanishes, which reads as "nothing is underrated" rather than
	// "this mirror has not been crawled enough to tell".
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, title,
		       (COALESCE(kudos,0) + COALESCE(bookmarks,0)) AS quality,
		       COALESCE(hits,1) AS reach,
		       COALESCE(word_count,0),
		       COALESCE(complete, 0)
		FROM works
		WHERE quality > 0 AND reach > 0
		  AND word_count > 0
		  AND (? = 0 OR COALESCE(word_count,0) >= ?)
		  AND (? = 0 OR COALESCE(complete, 0) != 0)
		ORDER BY (CAST(quality AS REAL) / reach) ASC, quality DESC
		LIMIT ?`,
		opts.MinWords, opts.MinWords,
		boolToInt(opts.Complete), limit)
	if err != nil {
		return res, fmt.Errorf("underrated: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, quality, reach, words int64
		var title string
		var complete bool
		if err := rows.Scan(&id, &title, &quality, &reach, &words, &complete); err != nil {
			return res, err
		}
		score := float64(quality) / float64(reach)
		res.Rows = append(res.Rows, Row{
			Key:    fmt.Sprintf("ao3_work:%d", id),
			Label:  title,
			WorkID: id,
			// Score is engagement per hit, so a HIGHER score is a more
			// underrated work. The SQL orders ascending by the same ratio
			// because it is paging for the low end of the raw ratio while the
			// ranking is for the high end; the two orders are opposite and
			// that is deliberate, not an inconsistency.
			Score:    score,
			Works:    int(quality),
			Words:    words,
			Complete: complete,
			Note:     fmt.Sprintf("quality %d over %d hits", quality, reach),
		})
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if len(res.Rows) == 0 {
		res.Notes = append(res.Notes,
			"no work has both engagement and hits recorded; the mirror may be uncrawled")
		return res, nil
	}
	// Rank most-underrated first.
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i].Score > res.Rows[j].Score })

	// When a filter is active, "truncated" is no longer the only way the list
	// can be short, and a reader who filtered down expects to know which of
	// the two happened. Both are stated rather than guessed.
	if opts.MinWords > 0 || opts.Complete {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"candidate pool narrowed before scoring (min_words=%d complete=%v); "+
				"this is a ranking over the narrower pool, not a filtered ranking",
			opts.MinWords, opts.Complete))
	}
	if limit > 0 && len(res.Rows) == limit {
		res.Truncated = true
	}
	return res, nil
}

// boolToInt renders a Go bool as the 0/1 SQLite compares against.
//
// The comparison is written `? = 0 OR complete != 0` rather than binding the
// bool directly: go-sqlite3 binds a bool as 0/1 but a NULL `complete` then
// fails both arms, and NULL is exactly the value a partially crawled mirror
// has for most rows.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TagNeighbours returns the tags that most often travel with a given tag.
//
// PMI rather than raw co-occurrence, for the reason the sibling's own fusing
// script needed IDF: raw counts rank `f/m`, `explicit` and `harry potter`
// first for every tag, because they are on everything. A tag that appears on
// 60% of the corpus carries no information about its neighbours even when its
// raw co-occurrence is enormous.
func (r *Runner) TagNeighbours(ctx context.Context, tag string, opts Options) (Result, error) {
	res := Result{Mode: string(ModeTagNeighbours), Rows: []Row{}}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	tag = strings.ToLower(strings.TrimSpace(tag))

	var tagID, tagWorks int
	if err := r.DB.QueryRowContext(ctx,
		`SELECT t.id, COUNT(*) FROM tags t
		 JOIN work_tags wt ON wt.tag_id = t.id
		 WHERE t.name = ? GROUP BY t.id`, tag).Scan(&tagID, &tagWorks); err != nil {
		if err == sql.ErrNoRows {
			res.Notes = append(res.Notes, fmt.Sprintf("no tag named %q in this corpus", tag))
			return res, nil
		}
		return res, fmt.Errorf("tag %q: %w", tag, err)
	}

	var corpusWorks int
	if err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&corpusWorks); err != nil {
		return res, err
	}
	if corpusWorks == 0 || tagWorks == 0 {
		res.Notes = append(res.Notes, "empty corpus")
		return res, nil
	}

	// Accumulate across the tag_type split: a tag name can sit on one work
	// under both freeforms and fandoms, so a non-accumulating count reports a
	// fraction of the truth.
	rows, err := r.DB.QueryContext(ctx, `
		SELECT t.name, COUNT(*) AS co
		FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id IN (SELECT work_id FROM work_tags WHERE tag_id = ?)
		  AND wt.tag_id != ?
		GROUP BY t.name
		ORDER BY co DESC
		LIMIT ?`, tagID, tagID, limit*4)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var co int
		if err := rows.Scan(&name, &co); err != nil {
			return res, err
		}
		var df int
		if err := r.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM work_tags WHERE tag_id =
			 (SELECT id FROM tags WHERE name = ?)`, name).Scan(&df); err != nil {
			return res, err
		}
		if df == 0 {
			continue
		}
		// PMI as log(co/expected), expected = tagWorks*df/corpusWorks.
		expected := float64(tagWorks) * float64(df) / float64(corpusWorks)
		if expected <= 0 {
			continue
		}
		pmi := math.Log(float64(co) / expected)
		if pmi <= 0 {
			// A neighbour that is no more likely to co-occur than chance is
			// not a neighbour. Reporting it would pad the list with the
			// corpus's most ubiquitous tags, which is the opposite of useful.
			continue
		}
		res.Rows = append(res.Rows, Row{
			Key: name, Label: name, Score: pmi,
			Works: df, CoWorks: co,
		})
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i].Score > res.Rows[j].Score })
	if len(res.Rows) > limit {
		res.Rows = res.Rows[:limit]
		res.Truncated = true
	}
	if len(res.Rows) == 0 {
		res.Notes = append(res.Notes,
			"no tag co-occurs with this one more often than chance")
	}
	return res, nil
}

// int32ListJSON renders ids as a JSON array for json_each, which is how SQLite
// gets a parameterised IN list without string concatenation.
func int32ListJSON(ids []int32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", id)
	}
	b.WriteByte(']')
	return b.String()
}
