package corpusquery

// TagGap answers the one question the roadmap calls WRITER-facing (#34): what do the
// works around this tag carry that the tag's own works rarely do?
//
// Every other mode here answers a reader's question. This one answers an author's: "I
// write hurt/comfort; my tag sits beside these forty tags, and their works carry things
// mine do not". A tag page can show what a work HAS; nothing before this showed what the
// genre around a tag expects and the tag's own works are missing.
//
// THE SHAPE, and why it is two hops rather than one:
//
//	seed T
//	  -> its neighbours N: the tags that travel with T most often
//	  -> for each N, the tags C that travel with N on works that do NOT have T
//	  -> rank C by  support x (1 - lift)
//
// One hop would just be tag-neighbours read backwards, which lists what T's works
// already have. The evidence has to come from works that are NOT T's, or "the corpus
// carries this" and "this tag carries this" collapse into the same number.
//
// THE TWO NUMBERS, both printed on the row:
//
//	support = the number of (N, C) co-occurrences over works without T, summed across
//	          T's neighbourhood. This is how strongly the genre around the tag wants C.
//	own     = how many of T's OWN works carry C.
//	lift    = (own / works(T)) / (corpusWorks(C) / corpusWorks)
//
// lift is the honest denominator: a tag that half the archive carries would show up in
// T's works at 50% by accident, and calling that "already covered" would hide every
// common tag from the list. 1.0 means "exactly as often as the corpus"; 0.2 means "this
// tag's works carry it a fifth as often as the archive does".
//
//	gap = support x (1 - min(lift, 1))
//
// so a tag the neighbourhood carries heavily and T's works never carry scores highest,
// and a tag T already carries at or above the corpus rate scores zero regardless of
// support. That clamp is the difference between a gap analysis and a list of popular
// tags: support alone ranks the archive's commonest tags, which every seed already has.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// tagGapNeighbours is how many of the seed's tags make up its neighbourhood. Thirty
	// is wide enough to describe a genre and small enough that a two-hop expansion over
	// them stays one indexed query rather than a flood.
	tagGapNeighbours = 30

	// tagGapCandidates caps the second hop before ranking, so a very common neighbour
	// tag cannot pull a hundred thousand candidates into memory.
	//
	// 150 rather than 600, measured: the two count queries cost roughly linearly in this
	// number, and through the Go driver they were 6.5 s of a 15 s request at 600 against
	// 1.6 s at 150. The list returns 25 rows and the filters reject a minority, so 150
	// candidates is not a bind on the answer -- and a query that takes longer than the
	// server's 20-second request timeout returns a 500, which is not a slow answer but no
	// answer at all.
	tagGapCandidates = 150

	// tagGapRowLimit is how many gaps a default query returns.
	tagGapRowLimit = 25

	// tagGapMinOwnWorks is the floor on the seed's own works. A tag with three works
	// has no distribution: every lift is 0 or 1 and every gap is noise. Below this the
	// query says so rather than ranking.
	tagGapMinOwnWorks = 25

	// tagGapMinCorpusWorks is the floor on a candidate: a tag carried by forty works in
	// the whole archive cannot be "what this genre needs" whatever its neighbourhood
	// support, because the archive has barely heard of it.
	tagGapMinCorpusWorks = 50
)

// TagGap ranks the tags a tag's neighbourhood carries and its own works do not.
func (r *Runner) TagGap(ctx context.Context, tag string, opts Options) (Result, error) {
	trace := os.Getenv("KINDRED_TAG_GAP_TRACE") != ""
	mark := time.Now()
	lap := func(what string) {
		if trace {
			fmt.Fprintf(os.Stderr, "tag-gap %-14s %s\n", what, time.Since(mark).Round(time.Millisecond))
			mark = time.Now()
		}
	}
	defer lap("total")
	res := Result{Mode: string(ModeTagGap)}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return res, fmt.Errorf("tag-gap: a tag is required")
	}
	if opts.Limit <= 0 {
		opts.Limit = tagGapRowLimit
	}
	if opts.MinCoWorks == 0 {
		opts.MinCoWorks = DefaultMinCoWorksToRank
	}
	minCorpus := tagGapMinCorpusWorks
	if opts.MinCorpusWorks != 0 {
		minCorpus = opts.MinCorpusWorks
	}

	var id, ownWorks int64
	err := r.DB.QueryRowContext(ctx,
		`SELECT t.id, COUNT(wt.work_id) FROM tags t
		 LEFT JOIN work_tags wt ON wt.tag_id = t.id
		 WHERE t.name = ? GROUP BY t.id`, tag).Scan(&id, &ownWorks)
	if err == sql.ErrNoRows || (err == nil && id == 0) {
		res.Notes = append(res.Notes, fmt.Sprintf("no tag named %q in this mirror", tag))
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("tag-gap: resolve %q: %w", tag, err)
	}

	var corpusWorks int64
	if err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&corpusWorks); err != nil {
		return res, fmt.Errorf("tag-gap: corpus size: %w", err)
	}
	res.Notes = append(res.Notes, fmt.Sprintf(
		"seed %q carries %d works of %d in the archive", tag, ownWorks, corpusWorks))
	if ownWorks < tagGapMinOwnWorks {
		// Named rather than ranked: with fewer works than this, "the tag's own works
		// rarely carry it" is a statement about a handful of works.
		res.Notes = append(res.Notes, fmt.Sprintf(
			"too few works to describe this tag's habit: %d, and a gap needs at least %d",
			ownWorks, tagGapMinOwnWorks))
		return res, nil
	}

	// The neighbourhood: what travels with the seed, by co-occurrence.
	nrows, err := r.DB.QueryContext(ctx, `
		SELECT b.tag_id, t.name, COUNT(*) AS co
		FROM work_tags a
		JOIN work_tags b ON b.work_id = a.work_id AND b.tag_id <> a.tag_id
		JOIN tags t ON t.id = b.tag_id
		WHERE a.tag_id = ?
		GROUP BY b.tag_id
		ORDER BY co DESC, b.tag_id
		LIMIT ?`, id, tagGapNeighbours)
	if err != nil {
		return res, fmt.Errorf("tag-gap: neighbourhood: %w", err)
	}
	defer nrows.Close()

	type neighbour struct {
		id   int64
		name string
		co   int
	}
	var hood []neighbour
	for nrows.Next() {
		var n neighbour
		if err := nrows.Scan(&n.id, &n.name, &n.co); err != nil {
			return res, fmt.Errorf("tag-gap: neighbourhood scan: %w", err)
		}
		hood = append(hood, n)
	}
	if err := nrows.Err(); err != nil {
		return res, fmt.Errorf("tag-gap: neighbourhood rows: %w", err)
	}
	if len(hood) == 0 {
		res.Notes = append(res.Notes, "this tag's works carry no other tag, so it has no neighbourhood")
		return res, nil
	}
	// The neighbourhood is closed before the next query closes the handle: a second
	// query on the same connection while these rows are open is the shape that produces
	// "database is locked" under load and a wrong empty result under none.
	if err := nrows.Close(); err != nil {
		return res, fmt.Errorf("tag-gap: neighbourhood close: %w", err)
	}
	lap("neighbourhood")

	// The second hop: what the neighbourhood carries on works that are NOT the seed's.
	//
	// The NOT EXISTS is the whole difference between this and tag-neighbours. Without
	// it, the support is dominated by the seed's own works, and every candidate is one
	// the seed's works already carry -- which is the walk's inert-pool lesson one mode
	// over: evidence gathered from inside the set you are trying to describe cannot say
	// anything new about it.
	inList := make([]string, 0, len(hood))
	hoodArgs := make([]any, 0, len(hood))
	for _, n := range hood {
		inList = append(inList, "?")
		hoodArgs = append(hoodArgs, n.id)
	}
	// The id list appears TWICE in the query, and the arguments must be in PLACEHOLDER
	// order, not in the order they were convenient to build: IN(...), NOT EXISTS(seed),
	// NOT IN(...), HAVING(min), LIMIT. Building them as "hood, seed, min, limit, hood"
	// put the minimum (50) where the exclusion list starts and a tag id where the HAVING
	// threshold belongs -- which reads as "no candidates found", the wrong answer every
	// other test in this file was written to distinguish from a right one. It was found
	// by printing the query's arguments rather than by reading them.
	args := make([]any, 0, 2*len(hood)+3)
	args = append(args, hoodArgs...)                 // IN (neighbourhood)
	args = append(args, id)                          // NOT EXISTS (the seed)
	args = append(args, hoodArgs...)                 // NOT IN (neighbourhood)
	args = append(args, minCorpus, tagGapCandidates) // HAVING, LIMIT
	// MATERIALIZED, and the materialisation is the whole performance story: measured on
	// this mirror (3.89M work_tags rows, a 30-tag neighbourhood covering ~600k works) the
	// plain join took 10.0 s because the NOT EXISTS was evaluated once per JOINED row --
	// 21M index lookups rather than 600k. As a materialised CTE it is 2.6 s, and the
	// answer is identical.
	crow, err := r.DB.QueryContext(ctx, `
		WITH hood_works AS MATERIALIZED (
			SELECT DISTINCT w.work_id FROM work_tags w
			WHERE w.tag_id IN (`+strings.Join(inList, ",")+`)
			  AND NOT EXISTS (
				SELECT 1 FROM work_tags x WHERE x.work_id = w.work_id AND x.tag_id = ?
			  )
		)
		SELECT c.tag_id, t.name, COUNT(*) AS support
		FROM hood_works hw
		JOIN work_tags c ON c.work_id = hw.work_id
		JOIN tags t ON t.id = c.tag_id
		WHERE c.tag_id NOT IN (`+strings.Join(inList, ",")+`)
		GROUP BY c.tag_id
		HAVING support >= ?
		ORDER BY support DESC
		LIMIT ?`, args...)
	if err != nil {
		return res, fmt.Errorf("tag-gap: candidates: %w", err)
	}
	defer crow.Close()

	type candidate struct {
		id      int64
		name    string
		support int
	}
	var cands []candidate
	for crow.Next() {
		var c candidate
		if err := crow.Scan(&c.id, &c.name, &c.support); err != nil {
			return res, fmt.Errorf("tag-gap: candidate scan: %w", err)
		}
		cands = append(cands, c)
	}
	if err := crow.Err(); err != nil {
		return res, fmt.Errorf("tag-gap: candidate rows: %w", err)
	}
	crow.Close()

	lap("candidates")
	if len(cands) == 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"no tag outside the neighbourhood is carried by the works around %q often "+
				"enough to rank (support >= %d)", tag, opts.MinCoWorks))
		return res, nil
	}

	// The neighbourhood's own work count: the denominator for "does the genre around
	// this tag carry this candidate MORE than the archive does".
	//
	// WITHOUT THIS the ranking is a popularity list. The first live run ranked
	// hurt/comfort's gaps as underage sex, vaginal sex, oral sex -- true statements
	// about the neighbourhood (M/M works carry them heavily) and useless ones, because
	// they lead every seed's list identically. support is dominated by how MANY works
	// carry a tag; the question is whether the neighbourhood carries it more often than
	// the archive, which is a rate.
	var neighWorks int64
	if err := r.DB.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT w.work_id) FROM work_tags w
		WHERE w.tag_id IN (`+strings.Join(inList, ",")+`)
		  AND NOT EXISTS (
			SELECT 1 FROM work_tags x WHERE x.work_id = w.work_id AND x.tag_id = ?
		  )`, append(append([]any{}, hoodArgs...), id)...).Scan(&neighWorks); err != nil {
		return res, fmt.Errorf("tag-gap: neighbourhood size: %w", err)
	}

	lap("neighworks")
	// The two rates each candidate needs: how often the SEED carries it, and how often
	// the archive does. Both in one pass over the candidate ids.
	// Two counts per candidate, in two cheap queries rather than one expensive join:
	//
	//	corpus = how many works carry it anywhere (index on tag_id, one seek per tag)
	//	own    = how many of the SEED's works carry it (index on work_id over the seed's
	//	         ~12k works, then grouped by tag)
	//
	// The first version joined tags to work_tags to work_tags with a LEFT JOIN per row,
	// which on 600 candidates x ~5k works each was ~9 s of the endpoint's 10 s. The two
	// queries below are the same numbers without the per-row join.
	idList := make([]string, 0, len(cands))
	candArgs := make([]any, 0, len(cands))
	for _, c := range cands {
		idList = append(idList, "?")
		candArgs = append(candArgs, c.id)
	}
	inline := strings.Join(idList, ",")
	corpus := map[int64]int{}
	crows, err := r.DB.QueryContext(ctx,
		`SELECT tag_id, COUNT(*) FROM work_tags WHERE tag_id IN (`+inline+`) GROUP BY tag_id`,
		candArgs...)
	if err != nil {
		return res, fmt.Errorf("tag-gap: corpus counts: %w", err)
	}
	for crows.Next() {
		var cid int64
		var n int
		if err := crows.Scan(&cid, &n); err != nil {
			crows.Close()
			return res, fmt.Errorf("tag-gap: corpus count scan: %w", err)
		}
		corpus[cid] = n
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return res, fmt.Errorf("tag-gap: corpus count rows: %w", err)
	}
	crows.Close()

	own := map[int64]int{}
	orows, err := r.DB.QueryContext(ctx, `
		SELECT wt.tag_id, COUNT(*) FROM work_tags wt
		WHERE wt.work_id IN (SELECT work_id FROM work_tags WHERE tag_id = ?)
		  AND wt.tag_id IN (`+inline+`)
		GROUP BY wt.tag_id`, append([]any{id}, candArgs...)...)
	if err != nil {
		return res, fmt.Errorf("tag-gap: own counts: %w", err)
	}
	for orows.Next() {
		var cid int64
		var n int
		if err := orows.Scan(&cid, &n); err != nil {
			orows.Close()
			return res, fmt.Errorf("tag-gap: own count scan: %w", err)
		}
		own[cid] = n
	}
	if err := orows.Err(); err != nil {
		orows.Close()
		return res, fmt.Errorf("tag-gap: own count rows: %w", err)
	}
	orows.Close()

	lap("counts")
	corpusShare := func(c int) float64 {
		if corpusWorks == 0 {
			return 0
		}
		return float64(c) / float64(corpusWorks)
	}
	for _, c := range cands {
		if c.support < opts.MinCoWorks {
			continue
		}
		cw := corpus[c.id]
		if cw < minCorpus {
			continue
		}
		share := corpusShare(cw)
		ownRate := float64(own[c.id]) / float64(ownWorks)
		lift := 0.0
		if share > 0 {
			lift = ownRate / share
		}
		// How much the neighbourhood over-carries the candidate, relative to the
		// archive, shrunk toward 1.0 by its own evidence. A candidate the neighbourhood
		// carries at exactly the archive's rate has enrichment 1 and no gap, whatever
		// its raw support.
		enrichment := 0.0
		if share > 0 && neighWorks > 0 {
			enrichment = (float64(c.support) / float64(neighWorks)) / share
		}
		enrichShrunk := 1 + (enrichment-1)*float64(c.support)/(float64(c.support)+ShrinkK)
		// NOTE: there is no explicit `enrichShrunk <= 1` guard here. It was written, and a
		// mutation showed it was dead: shrinking pulls toward 1 without crossing it, so
		// enrichment <= 1 implies enrichShrunk <= 1 implies a non-positive gap, which the
		// `gap <= 0` test below already refuses. Both factors of the score are required to
		// be positive, and ONE comparison enforces it.
		// A tag the seed carries at or above the corpus rate has NO gap: 1-lift is zero
		// at parity and negative above it, so the row is dropped below rather than
		// ranked low. There was an explicit math.Min(lift, 1) here, and a mutation run
		// proved it redundant -- deleting it changed no test outcome, because a lift
		// above 1 already produces a non-positive gap. The exclusion is the `gap <= 0`,
		// which is what a mutant does break.
		gap := float64(c.support) * (enrichShrunk - 1) * (1 - lift)
		if gap <= 0 {
			continue
		}
		res.Rows = append(res.Rows, Row{
			Key:     c.name,
			Label:   c.name,
			Score:   gap,
			Works:   cw,
			CoWorks: own[c.id],
			Lift:    lift,
			Note: fmt.Sprintf("%d of %d of this tag's works carry it (%.2fx the corpus "+
				"rate); the neighbourhood carries it %.1fx the corpus rate",
				own[c.id], ownWorks, lift, enrichShrunk),
		})
	}

	sort.SliceStable(res.Rows, func(i, j int) bool {
		if res.Rows[i].Score != res.Rows[j].Score {
			return res.Rows[i].Score > res.Rows[j].Score
		}
		return res.Rows[i].Works > res.Rows[j].Works
	})
	if len(res.Rows) > opts.Limit {
		res.Rows = res.Rows[:opts.Limit]
		res.Truncated = true
	}
	names := make([]string, 0, 5)
	for _, n := range hood[:min(5, len(hood))] {
		names = append(names, n.name)
	}
	res.Notes = append(res.Notes, fmt.Sprintf(
		"neighbourhood of %d tags (%s, ...); support counts co-occurrences on works "+
			"WITHOUT %q, so the evidence comes from outside the tag",
		len(hood), strings.Join(names, ", "), tag))
	res.Notes = append(res.Notes, fmt.Sprintf(
		"gap = support x (enrichment-1) x (1-lift), where enrichment is how much more "+
			"often the %d neighbourhood works carry the tag than the archive does and lift "+
			"is how often this tag's own works carry it against the archive; shrunk by "+
			"support/(support+%.0f)", neighWorks, ShrinkK))
	res.Notes = append(res.Notes, "a tag this tag already carries at or above the corpus "+
		"rate has no gap and is not listed, and neither does a tag the neighbourhood "+
		"carries no more often than the archive")
	return res, nil
}
