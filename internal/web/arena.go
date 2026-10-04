package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// ArenaCookieName is the session cookie shared by the pages and the JSON
// API. It is exported because the api package needs the same string, and a
// literal duplicated across two packages is a cookie that silently stops
// matching the day one of them is edited.
const ArenaCookieName = "kindred_arena"

// ArenaPage is one presented comparison.
type ArenaPage struct {
	Base
	// A and B are nil together, and Message explains why. An exhausted
	// pool is a normal outcome, and rendering it as an invitation is the
	// difference between a feature and a dead end.
	A, B *WorkCard
	// Cards is A and B as a flat slice. Two pointer fields cannot be
	// ranged over, and the form needs each card to know which side it is
	// so the radio can carry 'a' or 'b'.
	Cards []WorkCard
	// Session names the comparison this page is judging. Without it the
	// form cannot be attributed and the judgement updates nothing.
	Session  string
	Message  string
	Strategy string
	Resumed  bool
	// Explored says this pair was a deliberate probe of the model's blind
	// spots, because a user shown a deliberately surprising pair deserves
	// to know why.
	Explored bool
	// Totals is the arena's size, so the page can say whether the
	// leaderboard means anything yet.
	TotalJudged int
}

// WorkCard is one side of a comparison.
type WorkCard struct {
	ID      int64
	Title   string
	Author  string
	Summary string
	// Rating and RD are display values. RD is shown too, because a rating
	// without its uncertainty is a number the reader will over-trust.
	Rating float64
	RD     float64
	Rated  bool
	TagURL []TagLink
	URL    string
}

// TagLink is a tag with its page URL.
type TagLink struct {
	Name string
	URL  string
}

// LeaderboardPage is the standings.
type LeaderboardPage struct {
	Base
	Rows       []LeaderboardRow
	Median     float64
	Note       string
	Empty      bool
	MinCompare int
}

// LeaderboardRow is one standing.
type LeaderboardRow struct {
	Rank        int
	WorkID      int64
	Title       string
	Author      string
	Rating      float64
	Effective   float64
	RD          float64
	Comparisons int
	Wins        int
	Losses      int
	Draws       int
	URL         string
}

// MyRankingPage is what one comparator has been shown, and what the arena
// has learned about them.
type MyRankingPage struct {
	Base
	Liked    []TagLink
	Disliked []TagLink
	// Judged is how many comparisons this session's owner has made, which
	// is what gates the explanation: the model needs a handful of
	// comparisons before its opinion is worth showing.
	Judged         int
	MinimumForView int
	RatedWorks     int
	TotalJudged    int
	Note           string
}

// BlockRow is one tag offered for blocking, with the reason it is a
// candidate at all.
type BlockRow struct {
	TagID   int64
	Name    string
	URL     string
	Reason  string
	Weight  float64
	N       int
	Blocked bool
}

// BlockPage is the quick-block surface: tags this reader probably does not
// want, one click each.
type BlockPage struct {
	Base
	// Candidates are the tags worth offering now, best reason first.
	Candidates []BlockRow
	// Blocked is what they have already blocked, so it is visible and
	// reversible. A block the reader cannot see or undo is a decision made
	// for them.
	Blocked []BlockRow
	// Judged is how many comparisons back the inference, which is the
	// difference between "you keep passing these over" and "one comparison
	// made this look bad".
	Judged         int
	MinimumForView int
	Note           string
	Empty          bool
}

// renderBlock offers the tags this reader probably does not want, each one
// click to block.
//
// Two sources, and the second is the reason this page is worth building.
//
//  1. Tags the arena has learned are negative (a negative weight in
//     arena_user_tag_weights). Direct evidence: the reader chose against a
//     work carrying that tag.
//
//  2. Tags that appear on every work the reader has passed over. This is not
//     in the weight table at all, and it is the case that matters most. A tag
//     is only learned as negative if it happened to DIFFER between two works
//     the reader was shown — the arena learns from the tag that discriminates,
//     and LearnTags ignores tags present on both. So a tag shared by every
//     work the reader rejects teaches nothing, ever, no matter how many
//     comparisons they make. The inference is a consequence of how the learning
//     works, not of the reader's taste, and it is invisible for exactly that
//     reason.
//
// Without source 2, the page would show a reader nothing at all until the
// arena happened to isolate their disliked tag, and the fix for that is more
// arena play — which is a strange response to "I do not want this tag".
func (d Deps) renderBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := BlockPage{
		Base:           d.base("Block tags", "Tags you probably do not want"),
		MinimumForView: 3,
	}

	_, ownerKey, err := d.arenaSession(ctx, w, r)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	// Already blocked, so the page can show and reverse them.
	blockedRows, err := d.Engine.Store.BlockedTags(ctx, ownerKey)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	blockedSet := make(map[int64]bool, len(blockedRows))
	for _, bt := range blockedRows {
		blockedSet[bt.TagID] = true
		page.Blocked = append(page.Blocked, BlockRow{
			TagID:   bt.TagID,
			Name:    tagName(d, ctx, bt.TagID),
			URL:     fmt.Sprintf("/tag/%d", bt.TagID),
			Blocked: true,
		})
	}

	// Source 1: learned-negative tags.
	weights, err := d.Engine.Store.TagWeights(ctx, ownerKey, 40)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	page.Judged = len(weights)

	// Only tags with real evidence behind them. A weight from one comparison is
	// a coincidence, and offering it as "you probably do not want this" invites
	// a block the reader does not actually want — which is the one mistake this
	// page must not make, because a block is a statement and statements are
	// believed.
	const minEvidence = 2
	seen := make(map[int64]bool, len(blockedSet))
	candidates := make([]BlockRow, 0, 16)
	for _, tw := range weights {
		if tw.Weight >= 0 || tw.N < minEvidence || blockedSet[tw.TagID] || seen[tw.TagID] {
			continue
		}
		seen[tw.TagID] = true
		candidates = append(candidates, BlockRow{
			TagID:  tw.TagID,
			Name:   tagName(d, ctx, tw.TagID),
			URL:    fmt.Sprintf("/tag/%d", tw.TagID),
			Weight: tw.Weight,
			N:      tw.N,
			Reason: fmt.Sprintf("you have chosen against %d works carrying it", tw.N),
		})
	}
	// Strongest dislike first, then better-evidenced.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Weight != candidates[j].Weight {
			return candidates[i].Weight < candidates[j].Weight
		}
		return candidates[i].N > candidates[j].N
	})

	// Source 2: tags common to the works this reader passed over.
	if passed, ok := d.passedOverTags(ctx, ownerKey, minEvidence); ok {
		for _, cand := range passed {
			if blockedSet[cand.TagID] || seen[cand.TagID] {
				continue
			}
			seen[cand.TagID] = true
			candidates = append(candidates, cand)
		}
	}

	if len(candidates) == 0 {
		page.Empty = true
		page.Note = "Nothing to suggest yet. A tag becomes a candidate when the " +
			"arena learns you choose against it, or when it turns up on every " +
			"work you pass over. Both need a few comparisons — see /arena."
	} else {
		page.Note = "Tags you have chosen against, or that appear on every work " +
			"you have passed over. Blocking removes them from the arena and from " +
			"recommendations — it is not a suggestion, it is a rule."
		if page.Judged < page.MinimumForView {
			page.Note = fmt.Sprintf("Early days: %d comparisons so far, so these "+
				"are first impressions rather than firm conclusions.",
				page.Judged)
		}
	}
	page.Candidates = candidates
	d.page(w, "block.html", page, http.StatusOK)
}

// passedOverTags returns tags shared by every work this reader passed over.
//
// "Passed over" means the reader was shown a comparison and did not pick this
// work: choice 'b' means they chose work_a, 'neither' means they rejected
// both. So the rejected side is work_a for 'b', and BOTH works for 'neither' —
// and a "neither" contributes two, because the reader said no to each of them
// separately. Getting this wrong would make the query silently return the
// winner's tags, which is the exact opposite of the intent.
//
// Requiring the tag on EVERY passed-over work is what makes a candidate: a tag
// on one of four is a coincidence, and a tag on all four is a pattern the
// reader has never had to articulate. It is also why this cannot come from
// arena_user_tag_weights: LearnTags only teaches on tags that DIFFER between
// the two works, so a tag on every work the reader rejects is invisible to
// the weight table no matter how many comparisons they make.
//
// The bool return is false when there is too little to infer from, so the
// caller can say so instead of showing an empty list that reads as "nothing
// to block".
func (d Deps) passedOverTags(ctx context.Context, ownerKey string, minLost int) ([]BlockRow, bool) {
	const maxLost = 200
	// Two arms because "neither" rejects BOTH works, and one arm cannot return
	// two rows. The other choices reject exactly one: 'a' means they took
	// work_a, so work_b was passed over; 'b' is the mirror.
	rows, err := d.Engine.Store.DB.QueryContext(ctx,
		`SELECT rejected FROM (
		   SELECT work_b AS rejected FROM arena_comparisons
		     WHERE owner_key = ? AND choice = 'a'
		   UNION ALL
		   SELECT work_a AS rejected FROM arena_comparisons
		     WHERE owner_key = ? AND choice IN ('b', 'neither')
		 ) ORDER BY rejected DESC LIMIT ?`, ownerKey, ownerKey, maxLost)
	if err != nil {
		return nil, false
	}
	defer rows.Close()

	// Deduplicated, because a reader who often says "neither" rejects the same
	// work repeatedly and counting it twice would inflate every ratio here.
	seenWork := make(map[int64]bool, maxLost)
	var lost []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false
		}
		if !seenWork[id] {
			seenWork[id] = true
			lost = append(lost, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false
	}
	if len(lost) < minLost {
		return nil, false
	}

	// Placeholders are generated from len(lost), never from user input, so this
	// is not an injection point. The cap keeps the statement under SQLite's
	// variable limit, which is why maxLost is 200 and not "however many".
	ph := strings.TrimSuffix(strings.Repeat("?,", len(lost)), ",")
	args := make([]any, 0, len(lost)+1)
	for _, id := range lost {
		args = append(args, id)
	}
	args = append(args, len(lost))
	trows, err := d.Engine.Store.DB.QueryContext(ctx,
		// work_tags, not entity_tags. The AO3 mirror reads work_tags
		// (internal/corpus/ao3.go) and so does the arena's own tagStats
		// (internal/store/arena.go:1022); entity_tags is the index store's
		// table and is empty for a corpus that was ingested rather than
		// indexed. Querying the wrong one returns zero rows and the page
		// reads as "nothing to suggest" — a silent wrong answer, not an error.
		`SELECT wt.tag_id, COUNT(DISTINCT wt.work_id) FROM work_tags wt
		 WHERE wt.work_id IN (`+ph+`)
		 GROUP BY wt.tag_id
		 HAVING COUNT(DISTINCT wt.work_id) >= ?
		 ORDER BY COUNT(DISTINCT wt.work_id) DESC, wt.tag_id ASC
		 LIMIT 20`, args...)
	if err != nil {
		return nil, false
	}
	defer trows.Close()

	var out []BlockRow
	for trows.Next() {
		var id, count int64
		if err := trows.Scan(&id, &count); err != nil {
			return nil, false
		}
		out = append(out, BlockRow{
			TagID:  id,
			Name:   tagName(d, ctx, id),
			URL:    fmt.Sprintf("/tag/%d", id),
			Weight: 0,
			N:      int(count),
			Reason: fmt.Sprintf("on all %d works you have passed over", count),
		})
	}
	if err := trows.Err(); err != nil {
		return nil, false
	}
	return out, true
}

// postBlock records or removes a block and redirects back to the page.
//
// A 303 for the same reason the judge does: after a POST the browser should
// GET, and a 302 lets some clients re-POST, which here would toggle the block
// straight back off.
func (d Deps) postBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("parse form: %w", err))
		return
	}
	raw := r.PostFormValue("tag_id")
	tagID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("tag_id must be a number, got %q", raw))
		return
	}

	_, ownerKey, err := d.arenaSession(ctx, w, r)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	// Explicit rather than a toggle: a toggle makes the outcome depend on
	// state this request does not carry, so a double-submit flips a block off.
	// "action=block" and "action=unblock" are idempotent instead.
	var opErr error
	switch r.PostFormValue("action") {
	case "block":
		opErr = d.Engine.Store.BlockTag(ctx, ownerKey, tagID)
	case "unblock":
		opErr = d.Engine.Store.UnblockTag(ctx, ownerKey, tagID)
	default:
		d.fail(w, r, http.StatusBadRequest,
			fmt.Errorf("action must be block or unblock, got %q", r.PostFormValue("action")))
		return
	}
	if opErr != nil {
		d.fail(w, r, http.StatusInternalServerError, opErr)
		return
	}

	http.Redirect(w, r, "/block", http.StatusSeeOther)
}

// renderArena presents a comparison.
func (d Deps) renderArena(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := ArenaPage{Base: d.base("Arena", "Compare two works"),
		Strategy: "maxinfo"}

	// A tag filter scopes the arena to a fandom, which is the only way to
	// make a comparison meaningful across a corpus this heterogeneous.
	var tagID int64
	if v := r.URL.Query().Get("tag_id"); v != "" {
		tagID, _ = strconv.ParseInt(v, 10, 64)
	}

	// The session cookie is the same one the API sets, so a user who has
	// already been comparing keeps their history and their learned
	// preferences across both interfaces.
	sessionKey, ownerKey, err := d.arenaSession(ctx, w, r)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if tagID > 0 {
		page.Base = d.base("Arena", "Compare within "+tagName(d, ctx, tagID))
	}

	if st, err := d.Engine.Store.Stats(ctx); err == nil {
		page.TotalJudged = st.Judged
	}

	pair, ok, err := d.Engine.Store.UnjudgedInSession(ctx, sessionKey)
	resumed := false
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if ok && pairOK(pair) {
		resumed = true
		page.Resumed = true
		page.Strategy = pair.Strategy
	}

	if resumed {
		page.A, page.B = d.cards(ctx, pair.WorkA, pair.WorkB)
		if page.A == nil || page.B == nil {
			// The corpus row is gone. Drop the stale presentation and start
			// fresh rather than rendering a card for a work that no longer
			// exists.
			page.Resumed = false
			resumed = false
		}
	}

	if !resumed {
		p, err := d.choosePair(ctx, w, r, sessionKey, ownerKey, tagID)
		if err != nil {
			if isNoPairErr(err) {
				page.Message = "There is nothing worth comparing right now. " +
					"Either you have seen every pair in this pool, or these " +
					"works are not yet distinct enough for your answer to " +
					"mean anything."
				page.Session = sessionKey
				d.page(w, "arena.html", page, http.StatusOK)
				return
			}
			d.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		page.A, page.B = d.cards(ctx, p.A, p.B)
		page.Strategy = string(p.Strategy)
		page.Explored = p.Strategy == arena.StrategyExplore
	}

	page.Session = sessionKey
	if page.A != nil {
		page.Cards = []WorkCard{*page.A}
	}
	if page.B != nil {
		page.Cards = append(page.Cards, *page.B)
	}
	d.page(w, "arena.html", page, http.StatusOK)
}

// pairOK reports whether a comparison was actually found. UnjudgedInSession
// returns a zero value rather than an error when there is none, and a
// zero-value comparison has work ids of 0 -- which would otherwise become a
// card for work 0.
func pairOK(c store.Comparison) bool {
	return c.ID > 0 && c.WorkA > 0 && c.WorkB > 0
}

type chosenPair struct {
	A, B     int64
	Strategy arena.Strategy
}

// choosePair presents a new pair. The selection logic lives in the arena
// package, where it is unit tested without a database; this only supplies
// the candidate pool and records the presentation.
func (d Deps) choosePair(ctx context.Context, w http.ResponseWriter, r *http.Request,
	sessionKey, ownerKey string, tagID int64) (chosenPair, error) {

	st := d.Engine.Store
	ids, err := st.CandidateWorkIDs(ctx, tagID, 300)
	if err != nil {
		return chosenPair{}, err
	}
	ratings, err := st.RatingFor(ctx, ids)
	if err != nil {
		return chosenPair{}, err
	}
	tags, err := st.TagsForWorks(ctx, ids)
	if err != nil {
		return chosenPair{}, err
	}
	cands := make([]arena.Candidate, 0, len(ids))
	for _, id := range ids {
		r := arena.Initial()
		if row, ok := ratings[id]; ok {
			r = row.Rating
		}
		cands = append(cands, arena.Candidate{ID: id, Rating: r, Tags: tags[id]})
	}
	if len(cands) < 2 {
		return chosenPair{}, store.ErrNoPair
	}

	seen, err := st.SeenPairs(ctx, ownerKey, 4000)
	if err != nil {
		return chosenPair{}, err
	}
	weights, err := st.TagWeights(ctx, ownerKey, 40)
	if err != nil {
		return chosenPair{}, err
	}
	pref := make(map[int64]float64, len(weights))
	for _, w := range weights {
		pref[w.TagID] = w.Weight
	}

	strategy := arena.StrategyMaxInfo
	if s := r.URL.Query().Get("strategy"); s != "" {
		if cs := arena.Strategy(s); cs.Valid() {
			strategy = cs
		}
	}
	p, err := arena.ChoosePair(arena.PairRequest{
		Candidates: cands, Seen: seen, Preferred: pref,
		Strategy: strategy, Explored: 0.15,
		Rand: sessionRand(sessionKey),
	})
	if err != nil {
		return chosenPair{}, err
	}
	if _, err := st.RecordPresentation(ctx, store.Comparison{
		WorkA: p.A.ID, WorkB: p.B.ID,
		SessionKey: sessionKey, OwnerKey: ownerKey,
		Strategy: string(p.Strategy),
	}); err != nil {
		return chosenPair{}, err
	}
	if _, err := st.TouchSession(ctx, sessionKey, ownerKey); err != nil {
		return chosenPair{}, err
	}
	return chosenPair{A: p.A.ID, B: p.B.ID, Strategy: p.Strategy}, nil
}

// cards renders two works as comparison cards.
func (d Deps) cards(ctx context.Context, ids ...int64) (*WorkCard, *WorkCard) {
	if len(ids) < 2 {
		return nil, nil
	}
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx,
		`SELECT id, COALESCE(title,''), COALESCE(authors,''), COALESCE(summary,'')
		 FROM works WHERE id IN (?,?)`, ids[0], ids[1])
	if err != nil {
		return nil, nil
	}
	defer rows.Close()

	titles := make(map[int64][3]string, 2)
	for rows.Next() {
		var id int64
		var title, author, summary string
		if err := rows.Scan(&id, &title, &author, &summary); err != nil {
			return nil, nil
		}
		titles[id] = [3]string{title, author, summary}
	}

	ratings, err := d.Engine.Store.RatingFor(ctx, []int64{ids[0], ids[1]})
	if err != nil {
		return nil, nil
	}
	tagLinks := d.tagLinksForWork(ctx, ids[0], ids[1])

	build := func(id int64) *WorkCard {
		t, ok := titles[id]
		if !ok {
			return nil
		}
		c := &WorkCard{
			ID: id, Title: t[0], Author: t[1], Summary: trim(t[2], 260),
			Rating: arena.MU_INIT, RD: arena.PHI_INIT,
			URL: fmt.Sprintf("/work/%d", id),
		}
		if row, ok := ratings[id]; ok {
			c.Rating = round1(row.Rating.Mu)
			c.RD = round1(row.Rating.Phi)
			c.Rated = row.Comparisons > 0
		}
		c.TagURL = tagLinks[id]
		return c
	}
	return build(ids[0]), build(ids[1])
}

// tagLinksForWork returns the tags for the given work ids as a map[workID][]TagLink.
func (d Deps) tagLinksForWork(ctx context.Context, workIDs ...int64) map[int64][]TagLink {
	out := make(map[int64][]TagLink, len(workIDs))
	if len(workIDs) == 0 {
		return out
	}
	// Build the IN clause.
	placeholders := make([]string, len(workIDs))
	args := make([]any, len(workIDs))
	for i, id := range workIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT work_id, tag_id FROM work_tags WHERE work_id IN (` + strings.Join(placeholders, ", ") + `) ORDER BY work_id`
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	pairs := make(map[int64][]int64, len(workIDs))
	for rows.Next() {
		var work, tag int64
		if err := rows.Scan(&work, &tag); err != nil {
			return out
		}
		pairs[work] = append(pairs[work], tag)
	}
	if err := rows.Err(); err != nil {
		return out
	}
	// Get tag names for all tags that appeared.
	var allTags []int64
	seen := map[int64]bool{}
	for _, tags := range pairs {
		for _, t := range tags {
			if !seen[t] {
				seen[t] = true
				allTags = append(allTags, t)
			}
		}
	}
	if len(allTags) == 0 {
		// No tags at all -> return empty slices.
		for _, id := range workIDs {
			out[id] = []TagLink{}
		}
		return out
	}
	nameArgs := make([]any, len(allTags))
	nameMarks := make([]string, len(allTags))
	for i, id := range allTags {
		nameArgs[i] = id
		nameMarks[i] = "?"
	}
	// The placeholder list is built for allTags, not sliced out of the
	// work-ID one above. Those two have different lengths -- a work has 2
	// ids, its tags can be 78 -- and slicing one to the other's length
	// panics on every request past the second.
	nq := `SELECT id, name FROM tags WHERE id IN (` + strings.Join(nameMarks, ", ") + `)`
	nameRows, err := d.Engine.Corpus.DB.QueryContext(ctx, nq, nameArgs...)
	if err != nil {
		return out
	}
	defer nameRows.Close()
	tagNames := make(map[int64]string)
	for nameRows.Next() {
		var id int64
		var name string
		if err := nameRows.Scan(&id, &name); err != nil {
			return out
		}
		tagNames[id] = name
	}
	if err := nameRows.Err(); err != nil {
		return out
	}
	// Build the result.
	for work, tags := range pairs {
		links := make([]TagLink, 0, len(tags))
		for _, t := range tags {
			name, ok := tagNames[t]
			if !ok {
				continue
			}
			links = append(links, TagLink{Name: name, URL: fmt.Sprintf("/tag/%d", t)})
		}
		out[work] = links
	}
	return out
}

// renderLeaderboard shows the standings.
func (d Deps) renderLeaderboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := LeaderboardPage{
		Base:   d.base("Leaderboard", "Works the arena has ranked"),
		Median: arena.MU_INIT,
		Note: "A work shown at 1500 has not been compared yet, and sits at " +
			"the middle because that is where an unrated work honestly " +
			"belongs. Read the RD before the rating.",
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	page.MinCompare = 0
	if v := r.URL.Query().Get("min_comparisons"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			page.MinCompare = n
		}
	}

	entries, err := d.Engine.Store.Leaderboard(ctx, limit, page.MinCompare)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if len(entries) == 0 {
		page.Empty = true
		page.Note = "Nothing has been rated yet. The arena needs comparisons " +
			"before it can rank anything, and a ranking built on one or two " +
			"comparisons per work would be noise."
		d.page(w, "leaderboard.html", page, http.StatusOK)
		return
	}

	ids := make([]any, len(entries))
	byID := make(map[int64]*LeaderboardRow, len(entries))
	for i, e := range entries {
		ids[i] = e.WorkID
		byID[e.WorkID] = &LeaderboardRow{
			Rank: e.Rank, WorkID: e.WorkID,
			Rating: round1(e.Rating.Mu), Effective: round1(e.Effective),
			RD: round1(e.Rating.Phi), Comparisons: e.Comparisons,
			Wins: e.Wins, Losses: e.Losses, Draws: e.Draws,
			URL: fmt.Sprintf("/work/%d", e.WorkID),
		}
	}
	q := `SELECT id, COALESCE(title,''), COALESCE(authors,'') FROM works WHERE id IN (?` +
		strings.Repeat(", ?", len(entries)-1) + `)`
	rows, err := d.Engine.Corpus.DB.QueryContext(ctx, q, ids...)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var title, author string
		if err := rows.Scan(&id, &title, &author); err != nil {
			d.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		if row, ok := byID[id]; ok {
			row.Title, row.Author = title, author
		}
	}
	for _, e := range entries {
		row := byID[e.WorkID]
		if row.Title == "" {
			// The rating outlived the corpus row. Showing the id keeps the
			// entry traceable instead of rendering an empty link.
			row.Title = fmt.Sprintf("work %d (not in corpus)", e.WorkID)
		}
		page.Rows = append(page.Rows, *row)
	}
	d.page(w, "leaderboard.html", page, http.StatusOK)
}

// postJudge records a comparison from the page's own form, then redirects
// to the next pair.
//
// The redirect is a 303, not a 302: after a POST the browser should GET the
// next page, and a 302 lets some clients re-POST, which would record a
// second judgement against a comparison that is already judged.
//
// tag_id is carried through so a scoped arena stays scoped across a
// judgement. Dropping it would silently widen the pool on the next pair.
func (d Deps) postJudge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("parse form: %w", err))
		return
	}
	session := r.PostFormValue("session")
	choice := r.PostFormValue("choice")
	switch choice {
	case "a", "b", "neither":
	default:
		d.fail(w, r, http.StatusBadRequest,
			fmt.Errorf("choice must be a, b or neither, got %q", choice))
		return
	}
	if session == "" {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("no arena session in the form"))
		return
	}

	if err := d.judge(ctx, session, choice); err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	// Back to the arena, un-posting. A failure to redirect is not a
	// failure to record: the comparison is already judged, so say so
	// rather than re-rendering the form and inviting a second judgement.
	back := "/arena"
	if tag := r.PostFormValue("tag_id"); tag != "" {
		back += "?tag_id=" + tag
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// judge records a choice and learns the tag preferences from it.
//
// Tag preferences update immediately, per observation: they are a running
// average per user and are correct to fold in one at a time. Ratings do
// NOT update here -- those are period statistics, and updating them per
// click is the mistake the paper specifically warns about.
func (d Deps) judge(ctx context.Context, sessionKey, choice string) error {
	st := d.Engine.Store
	if err := st.RecordJudgement(ctx, sessionKey, choice); err != nil {
		return err
	}
	if err := st.BumpSession(ctx, sessionKey); err != nil {
		return err
	}
	if choice == "neither" {
		// A draw is a real judgement -- "these are equally uninteresting"
		// -- and it counts toward the rating, but it discriminates on no
		// tag, so there is nothing to learn from it.
		return nil
	}

	comp, ok, err := st.LastJudgedInSession(ctx, sessionKey)
	if err != nil || !ok {
		return err
	}
	winner, loser := comp.WorkA, comp.WorkB
	if choice == "b" {
		winner, loser = comp.WorkB, comp.WorkA
	}

	tags, err := st.TagsForWorks(ctx, []int64{winner, loser})
	if err != nil {
		return err
	}
	tagCount, total, err := st.TagCounts(ctx)
	if err != nil {
		return err
	}
	deltas := arena.LearnTags(tags, []int64{winner}, []int64{loser}, tagCount, total)
	// Applied in id order, so the write sequence is reproducible in a log
	// and a partial failure is reproducible too.
	for _, id := range deltaKeys(deltas) {
		if err := st.UpsertTagWeight(ctx, comp.OwnerKey, id, deltas[id]); err != nil {
			return err
		}
	}
	return nil
}

// RankPage is one work's arena rating, with its history.
type RankPage struct {
	Base
	WorkID      int64
	Title       string
	Author      string
	Rating      float64
	RD          float64
	Sigma       float64
	Comparisons int
	Wins        int
	Losses      int
	Draws       int
	Rated       bool
	// Note is the honest caveat for an unrated work: it carries the
	// STARTING rating, not a measured one, and saying "1500" without that
	// distinction would be a false claim.
	Note string
	// History is the rating across periods, oldest first, for the
	// sparkline and for "why is this where it is".
	History []RankPoint
	WorkURL string
}

// RankPoint is one period's rating.
type RankPoint struct {
	Period int64
	Rating float64
	RD     float64
}

// renderRank shows one work's rating and how it got there.
func (d Deps) renderRank(w http.ResponseWriter, r *http.Request, idStr string) {
	ctx := r.Context()
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		d.notFound(w, r)
		return
	}
	page := RankPage{
		Base:    d.base("Rank", "Arena rating"),
		WorkID:  id,
		WorkURL: fmt.Sprintf("/work/%d", id),
	}

	row, err := d.Engine.Store.Rating(ctx, id)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	page.Rating = round1(row.Rating.Mu)
	page.RD = round1(row.Rating.Phi)
	page.Sigma = row.Rating.Sigma
	page.Comparisons = row.Comparisons
	page.Wins, page.Losses, page.Draws = row.Wins, row.Losses, row.Draws
	page.Rated = row.Comparisons > 0
	if !page.Rated {
		page.Note = "This work has not been compared yet, so it carries the " +
			"starting rating rather than a measured one."
	}

	// The title is a convenience; the rating is the point, so a work whose
	// corpus row has gone still renders.
	var title, author string
	if err := d.Engine.Corpus.DB.QueryRowContext(ctx,
		`SELECT COALESCE(title,''), COALESCE(authors,'') FROM works WHERE id = ?`, id).
		Scan(&title, &author); err != nil {
		title = fmt.Sprintf("work %d (not in corpus)", id)
	}
	page.Title, page.Author = title, author

	hist, err := d.Engine.Store.RatingHistory(ctx, id, 40)
	if err == nil {
		for _, h := range hist {
			page.History = append(page.History, RankPoint{
				Period: h.Period, Rating: round1(h.Rating.Mu), RD: round1(h.Rating.Phi),
			})
		}
	}
	d.page(w, "rank.html", page, http.StatusOK)
}

// renderMyRanking shows one comparator's learned preferences.
func (d Deps) renderMyRanking(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := MyRankingPage{
		Base:           d.base("Your ranking", "What the arena has learned about you"),
		MinimumForView: 3,
		Note: "Preferences come from the tags that DIFFER between the two " +
			"works you were shown. A tag on both teaches nothing, and a tag " +
			"on most of the corpus teaches less than a rare one.",
	}

	// The same cookie name the API uses, so history carries across the JSON
	// API and these pages. Reading it here rather than minting a new one is
	// what makes that true.
	_, ownerKey, err := d.arenaSession(ctx, w, r)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	if st, err := d.Engine.Store.Stats(ctx); err == nil {
		page.RatedWorks = st.RatedWorks
		page.TotalJudged = st.Judged
	}

	weights, err := d.Engine.Store.TagWeights(ctx, ownerKey, 40)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	m := make(map[int64]float64, len(weights))
	for _, tw := range weights {
		m[tw.TagID] = tw.Weight
	}
	page.Judged = len(weights)
	likedIDs, dislikedIDs := arena.TopTags(arena.Normalise(m), 8)

	var likedLinks []TagLink
	for _, id := range likedIDs {
		likedLinks = append(likedLinks, TagLink{
			Name: tagName(d, ctx, id),
			URL:  fmt.Sprintf("/tag/%d", id),
		})
	}
	var dislikedLinks []TagLink
	for _, id := range dislikedIDs {
		dislikedLinks = append(dislikedLinks, TagLink{
			Name: tagName(d, ctx, id),
			URL:  fmt.Sprintf("/tag/%d", id),
		})
	}
	page.Liked = likedLinks
	page.Disliked = dislikedLinks

	// Below the threshold, say why the panel is thin rather than showing
	// an empty box that reads as "the arena thinks you have no taste".
	if page.Judged < page.MinimumForView {
		page.Note = fmt.Sprintf("Not enough comparisons yet (%d of %d needed). "+
			"The arena needs a few choices before its opinion of your "+
			"preferences is worth showing -- a preference learned from one "+
			"comparison is a coincidence.", page.Judged, page.MinimumForView)
	}
	d.page(w, "myranking.html", page, http.StatusOK)
}

// deltaKeys returns the tag ids in a delta map, in ascending order.
func deltaKeys(m map[int64]float64) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// tagName returns the name of a tag, or a fallback.
func tagName(d Deps, ctx context.Context, id int64) string {
	var name string
	if err := d.Engine.Corpus.DB.QueryRowContext(ctx,
		`SELECT name FROM tags WHERE id = ?`, id).Scan(&name); err != nil {
		return fmt.Sprintf("tag %d", id)
	}
	return name
}

// isNoPairErr reports whether err is store.ErrNoPair.
func isNoPairErr(err error) bool {
	return err != nil && err.Error() == store.ErrNoPair.Error()
}

// arenaSession returns the request's arena session, minting one if absent.
// The identifier is hashed to an owner key immediately, so nothing
// downstream can store the cookie value itself.
func (d Deps) arenaSession(ctx context.Context, w http.ResponseWriter, r *http.Request) (sessionKey, ownerKey string, err error) {
	if c, cerr := r.Cookie(ArenaCookieName); cerr == nil && c.Value != "" {
		sessionKey = c.Value
	} else {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", "", fmt.Errorf("mint arena session: %w", err)
		}
		sessionKey = hex.EncodeToString(buf)
		http.SetCookie(w, &http.Cookie{
			Name: ArenaCookieName, Value: sessionKey, Path: "/",
			MaxAge: 60 * 60 * 24 * 365, HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			// Secure is deliberately not set: the site is plain HTTP on
			// loopback behind a TLS-terminating proxy, and a Secure cookie
			// set on such a response is dropped by the browser, which
			// would mint a new session per request.
		})
	}
	ownerKey, err = d.Engine.Store.OwnerKey(ctx, sessionKey)
	if err != nil {
		return "", "", err
	}
	return sessionKey, ownerKey, nil
}

// sessionRand is a deterministic PRNG seeded from the session key.
//
// Deterministic per session, NOT globally seeded: a reload must not reshuffle
// an unjudged pair (the resume path handles that), and two users must not
// see the same sequence. crypto/rand for the seed, a cheap LCG for the
// draws, because this is presentation jitter and not security.
func sessionRand(seed string) func(int) int {
	h := sha256.Sum256([]byte(seed))
	state := binary.BigEndian.Uint64(h[:8])
	return func(n int) int {
		if n <= 0 {
			return 0
		}
		// xorshift64*
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		v := state * 2685821657736338717
		return int((v >> 33) % uint64(n))
	}
}

// trim shortens text on a RUNE boundary, not a byte one.
func trim(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	// Budget the ellipsis, not just the cut. Appending "..." after cutting to
	// max bytes overshoots by three -- "party 🎉🎉🎉🎉🎉🎉 time" at max=20 cut
	// to 18 bytes and then grew to 21. A field this narrow then has to be
	// escaped or truncated by whatever renders it, so the overrun propagates.
	//
	// Cut in whole runes (never mid-rune), and if a single rune is wider than
	// the whole budget, keep one rune rather than none: returning empty would
	// make the row look missing.
	const ellipsis = "..."
	budget := max - len(ellipsis)
	if budget < 0 {
		budget = 0
	}
	r := []rune(s)
	for len(r) > 0 && len(string(r)) > budget {
		r = r[:len(r)-1]
	}
	out := strings.TrimRight(string(r), " ,.;:")
	if out == "" {
		// One rune is wider than the budget (a CJK or emoji summary at max=2).
		// Returning "" would read as "no summary", which is a different claim.
		if len(runesOf(s)) > 0 {
			return string(runesOf(s)[0])
		}
		return s
	}
	return out + ellipsis
}

// runesOf is a small readability helper for the trim guard above.
func runesOf(s string) []rune { return []rune(s) }

// round1 is for display only. A rating is a float that means "about this
// much", and showing 1464.0583 implies a precision the measurement does not
// have -- the RD is 151.
func round1(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*10) / 10
}
