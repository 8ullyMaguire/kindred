package web

// /algo?mode=explain -- one work, two sentences, one question (#55).
//
// The page's other mode asks which WEIGHT VECTOR ranks better, and it can be answered without a
// reader ever reading a title. This mode asks a smaller question about one work: which of the two
// sentences this deployment can back with a number explains why it is here. The two are the tags
// it shares with the seed and the readers who bookmarked both -- the only two kinds that rest on
// something countable, since there is no runtime model here to write a third.
//
// Three refusals are deliberate:
//
//   - An item with only ONE of the two sentences is not shown. Comparing a sentence with an empty
//     column invites a vote about nothing, which is the defect /algo already refuses for two
//     identical tunes.
//   - A vote with no session is refused (the store refuses it, and the page mints a session
//     before asking), because an unowned vote cannot be de-duplicated.
//   - The standings name a leader only past the sign test's power floor, and print the record
//     before the verdict either way.

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/explain"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// explanationItems is how many ranked works the explain view inspects per request. Every one of
// them costs a tag lookup and a co-reader count, so this is a display budget rather than a
// ranking budget.
const explanationItems = 20

// renderAlgoExplain builds the duel list for a seed.
func (d Deps) renderAlgoExplain(w http.ResponseWriter, r *http.Request, page AlgoPage) {
	seed, err := engine.ParseSeed(page.Seed)
	if err != nil {
		page.Err = err.Error()
		d.page(w, "algo.html", page, http.StatusBadRequest)
		return
	}
	if d.Engine == nil || d.Engine.Corpus == nil {
		page.Err = "no corpus is loaded on this server"
		d.page(w, "algo.html", page, http.StatusServiceUnavailable)
		return
	}

	n := intParamOr(r, "n", 12)
	if n < 2 {
		n = 2
	}
	if n > explanationItems {
		n = explanationItems
	}

	// The seed's own tags, with names, once.
	seedPairs, err := d.Engine.Corpus.TagPairs(r.Context(), []int64{seed.ID})
	if err != nil {
		page.Err = "could not read the seed's tags: " + err.Error()
		d.page(w, "algo.html", page, http.StatusInternalServerError)
		return
	}
	seedTags := seedPairs[seed.ID]
	if len(seedTags) == 0 {
		// Without tags there is no tag sentence, so there is no duel to ask for. Said plainly
		// rather than rendered as an empty list.
		page.ExplainNote = "this seed has no tags in the mirror, so neither explanation can be written for its neighbours"
		d.page(w, "algo.html", page, http.StatusOK)
		return
	}

	// Pure relevance, like the A/B view: the question is about the sentence, not about the
	// diversifier's taste.
	res, err := d.Engine.Recommend(r.Context(), engine.Request{
		Kind: seed.Kind, Seeds: []engine.Seed{seed}, Tune: tuneOrEmpty(page.A),
		Exclude: true, N: n, Lambda: 1,
	})
	if err != nil {
		page.Err = err.Error()
		d.page(w, "algo.html", page, http.StatusInternalServerError)
		return
	}
	if len(res.Items) == 0 {
		page.ExplainNote = "the ranking is empty for this seed, so there is nothing to explain"
		d.page(w, "algo.html", page, http.StatusOK)
		return
	}

	ids := make([]int64, len(res.Items))
	for i, c := range res.Items {
		ids[i] = c.ID
	}
	pairs, err := d.Engine.Corpus.TagPairs(r.Context(), append([]int64{seed.ID}, ids...))
	if err != nil {
		page.Err = "could not read the candidates' tags: " + err.Error()
		d.page(w, "algo.html", page, http.StatusInternalServerError)
		return
	}
	readers, err := d.Engine.Corpus.SharedReadersMany(r.Context(), seed.ID, ids)
	if err != nil {
		page.Err = "could not count shared readers: " + err.Error()
		d.page(w, "algo.html", page, http.StatusInternalServerError)
		return
	}

	// Rarity ordering needs the shared tags' own support. One query over the (small) union of
	// shared tags, not one per candidate.
	sharedIDs := map[int32]bool{}
	for _, c := range res.Items {
		for _, p := range pairs[c.ID] {
			if hasTagPair(seedTags, p.ID) {
				sharedIDs[p.ID] = true
			}
		}
	}
	support, err := d.Engine.Corpus.TagSupport(r.Context(), keysOf(sharedIDs))
	if err != nil {
		page.Err = "could not read tag support: " + err.Error()
		d.page(w, "algo.html", page, http.StatusInternalServerError)
		return
	}

	seedTitle := ""
	if len(res.Items) > 0 {
		seedTitle = seedTitleOf(d, r, seed.ID)
	}

	inspected := 0
	for _, c := range res.Items {
		inspected++
		shared := sharedTagNames(seedTags, pairs[c.ID], support)
		duel, ok := explain.Build(c.ID, c.Title, seedTitle, shared, readers[c.ID])
		if !ok {
			page.ExplainSkipped++
			continue
		}
		page.Duels = append(page.Duels, duel)
	}
	page.ExplainInspected = inspected

	// Standings, when this deployment has a state database to read them from.
	if d.Algo != nil {
		if rows, err := d.Algo.ExplanationStandings(r.Context()); err == nil {
			page.ExplainStandings = rows
		}
	}
	if len(page.Duels) == 0 {
		page.ExplainNote = fmt.Sprintf(
			"none of the %d ranked works has BOTH a shared tag and at least %d shared readers, "+
				"so there is nothing to compare: a sentence alone in a column is not a duel",
			inspected, explain.MinSharedReaders)
	}
	d.page(w, "algo.html", page, http.StatusOK)
}

// postAlgoExplain records one preference between two explanations.
func (d Deps) postAlgoExplain(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		d.fail(w, r, http.StatusBadRequest, err)
		return
	}
	seedStr := strings.TrimSpace(r.PostFormValue("seed"))
	workStr := strings.TrimSpace(r.PostFormValue("work"))
	choice := strings.TrimSpace(r.PostFormValue("choice"))

	back := "/algo?mode=explain&seed=" + urlQuery(seedStr) + "&a=" + urlQuery(defaultA(r))
	seed, err := engine.ParseSeed(seedStr)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, err)
		return
	}
	var workID int64
	if _, err := fmt.Sscanf(workStr, "%d", &workID); err != nil || workID == 0 {
		d.fail(w, r, http.StatusBadRequest, fmt.Errorf("work must be a numeric id, got %q", workStr))
		return
	}
	switch choice {
	case store.DuelTags, store.DuelReaders, store.DuelNeither:
	default:
		d.fail(w, r, http.StatusBadRequest,
			fmt.Errorf("choice must be tags, readers or neither, got %q", choice))
		return
	}
	if d.Algo == nil {
		d.fail(w, r, http.StatusServiceUnavailable,
			fmt.Errorf("this deployment has no state database to record votes in"))
		return
	}

	// The session is minted BEFORE the vote is recorded: the store refuses an unowned vote, and
	// a reader who has never visited /arena has no cookie yet.
	_, ownerKey, err := d.arenaSession(r.Context(), w, r)
	if err != nil {
		d.fail(w, r, http.StatusServiceUnavailable, err)
		return
	}
	if err := d.Algo.RecordExplanationDuel(r.Context(), ownerKey, store.ExplanationDuel{
		SeedID: seed.ID, WorkID: workID, Winner: choice,
	}); err != nil {
		d.fail(w, r, http.StatusBadRequest, err)
		return
	}
	http.Redirect(w, r, back+"&judged="+url.QueryEscape(choice), http.StatusSeeOther)
}

// defaultA reads the tune the explain view ranked with, so a redirect returns to the same view.
func defaultA(r *http.Request) string {
	if a := strings.TrimSpace(r.PostFormValue("algo_a")); a != "" {
		return a
	}
	if a := strings.TrimSpace(r.URL.Query().Get("a")); a != "" {
		return a
	}
	return "default"
}

func tuneOrEmpty(name string) string {
	if name == "" || name == "default" {
		return ""
	}
	return name
}

// hasTagPair reports whether the seed carries the tag.
func hasTagPair(seedTags []corpus.TagPair, id int32) bool {
	for _, p := range seedTags {
		if p.ID == id {
			return true
		}
	}
	return false
}

// sharedTagNames returns the shared tag names ordered RAREST FIRST, then by name so two runs
// agree. Rare-first is the order the sentence needs: "coffee shop au" is on a fifth of the
// archive and explains nothing, while a tag on forty works is the reason the work was found.
func sharedTagNames(seedTags []corpus.TagPair, candTags []corpus.TagPair, support map[int64]int) []string {
	var out []string
	for _, c := range candTags {
		if !hasTagPair(seedTags, c.ID) {
			continue
		}
		out = append(out, c.Name)
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := supportOf(support, seedTags, out[i]), supportOf(support, seedTags, out[j])
		if si != sj {
			return si < sj
		}
		return out[i] < out[j]
	})
	return out
}

func supportOf(support map[int64]int, seedTags []corpus.TagPair, name string) int {
	for _, p := range seedTags {
		if p.Name == name {
			return support[int64(p.ID)]
		}
	}
	return 0
}

// seedTitleOf reads the seed's own title, for the sentence that names it.
func seedTitleOf(d Deps, r *http.Request, id int64) string {
	if d.Engine == nil || d.Engine.Corpus == nil {
		return ""
	}
	ent, err := d.Engine.Corpus.Entity(r.Context(), id)
	if err != nil {
		return ""
	}
	return ent.Title
}

func keysOf(m map[int32]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, int64(k))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
