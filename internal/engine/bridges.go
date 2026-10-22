package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/fandom"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// bridgeScanLimit bounds how many works sharing a seed fandom tag are
// inspected for a second fandom.
//
// The fandom tags are among the most common in the corpus, so the set is
// large. The bound chooses by kudos, which is the only ordering available
// without scoring each candidate, and a bridge nobody has read is not a
// recommendation. It is a cap rather than a full scan for the same reason the
// candidate pool is: a request must not walk a million rows to decorate one
// page.
const bridgeScanLimit = 600

// Bridge is a crossover work: it carries the seed's fandom and at least one
// other.
type Bridge struct {
	rank.Candidate
	// OtherFandoms are the canonical names of the fandom tags this work
	// carries that the seed does not. They are the reason the work is a
	// bridge, so the panel shows them rather than a score.
	OtherFandoms []string `json:"other_fandoms"`
}

// BridgeResult is the bridge lookup with the evidence needed to read it.
type BridgeResult struct {
	// SeedFandoms are the seed work's canonical fandom names, which is what
	// "the seed's fandom" means below.
	SeedFandoms []string `json:"seed_fandoms"`
	Items       []Bridge `json:"items"`
	// Note explains an empty result. It is set only when there is something
	// to explain: a work with no fandom-shaped tag is not the same fact as
	// a fandom with no crossovers, and an empty list with no reason reads as
	// a broken page.
	Note string `json:"note,omitempty"`
}

// Bridges finds works that sit in this work's fandom and in another one.
//
// ## What this is for
//
// A reader who likes one fandom does not only want more of it; the works that
// introduce a second fandom are how a reading habit grows. Tag overlap cannot
// find these -- a crossover's extra fandom is a tag the seed does not carry,
// so it is a difference rather than a match -- and a plain recommendation will
// bury it under works that match the seed's tags more completely.
//
// ## Why fandom shape rather than tag_type
//
// The mirror files 3,890,504 of its 3,891,300 tag rows as 'freeforms', fandom
// tags included. A `tag_type = 'fandoms'` query returns 51 works out of
// 113,995, so it would report "no crossovers" on a corpus full of them. The
// same measured reason internal/fandom exists for the diversity cap applies
// here: the name shape is the only signal the corpus actually holds.
func (e *Engine) Bridges(ctx context.Context, workID int64, limit int) (BridgeResult, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	var out BridgeResult
	if e.Corpus == nil {
		return out, fmt.Errorf("bridges: this engine has no corpus")
	}

	pairs, err := e.Corpus.TagPairs(ctx, []int64{workID})
	if err != nil {
		return out, fmt.Errorf("bridges: seed tags: %w", err)
	}
	seedKeys := map[string]string{} // canonical -> display name
	tagIDs := make([]int32, 0, len(pairs[workID]))
	for _, p := range pairs[workID] {
		if !fandom.LooksLikeFandom(p.Name) {
			continue
		}
		key := fandom.Canonical(p.Name)
		if key == "" {
			continue
		}
		seedKeys[key] = p.Name
		tagIDs = append(tagIDs, p.ID)
	}
	out.SeedFandoms = sortedValues(seedKeys)
	if len(tagIDs) == 0 {
		out.Note = "This work carries no fandom-shaped tag, so there is no fandom to bridge out of. " +
			"Fandoms are recognised by tag name shape, because the mirror files almost every tag as a freeform."
		return out, nil
	}

	args := make([]any, 0, len(tagIDs)+2)
	for _, id := range tagIDs {
		args = append(args, id)
	}
	args = append(args, workID, bridgeScanLimit)
	rows, err := e.Corpus.DB.QueryContext(ctx, `
		SELECT DISTINCT wt.work_id
		FROM work_tags wt
		JOIN works w ON w.id = wt.work_id
		WHERE wt.tag_id IN (`+makeMarks(len(tagIDs))+`)
		  AND wt.work_id <> ?
		ORDER BY w.kudos DESC, w.id
		LIMIT ?`, args...)
	if err != nil {
		return out, fmt.Errorf("bridges: candidate works: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, fmt.Errorf("bridges: scan candidate: %w", err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, fmt.Errorf("bridges: candidate works: %w", err)
	}
	if len(ids) == 0 {
		out.Note = "No other work in this mirror shares this work's fandom tag, so no crossover can be found."
		return out, nil
	}

	candPairs, err := e.Corpus.TagPairs(ctx, ids)
	if err != nil {
		return out, fmt.Errorf("bridges: candidate tags: %w", err)
	}
	type found struct {
		other []string
	}
	bridges := map[int64]found{}
	for _, id := range ids {
		otherKeys := map[string]bool{}
		// A candidate is only a bridge if it ALSO carries the seed's fandom:
		// sharing a tag id is how it entered the scan, but a tag id can be
		// reused under two types, and the claim on the page is about fandom
		// families rather than ids.
		sameFandom := false
		for _, p := range candPairs[id] {
			if !fandom.LooksLikeFandom(p.Name) {
				continue
			}
			key := fandom.Canonical(p.Name)
			if key == "" {
				continue
			}
			if _, isSeed := seedKeys[key]; isSeed {
				sameFandom = true
				continue
			}
			otherKeys[key] = true
		}
		if !sameFandom || len(otherKeys) == 0 {
			continue
		}
		bridges[id] = found{other: sortedSet(otherKeys)}
	}
	if len(bridges) == 0 {
		out.Note = "Every work sharing this fandom stays inside it. That is a real answer about the mirror: " +
			"a fandom-fandom crossover needs both tags on the same work, and this sample has none."
		return out, nil
	}

	// Rank by how many OTHER fandoms the work spans, then by popularity.
	// A work in three fandoms is a stronger bridge than one in two, but the
	// tie-break still prefers the work a reader is more likely to open.
	order := make([]int64, 0, len(bridges))
	for id := range bridges {
		order = append(order, id)
	}
	ents, err := e.Corpus.CandidateRowsSlim(ctx, order)
	if err != nil {
		return out, fmt.Errorf("bridges: load candidates: %w", err)
	}
	kudos := make(map[int64]float64, len(ents))
	for _, en := range ents {
		kudos[en.ID] = en.Stats["kudos"]
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := bridges[order[i]], bridges[order[j]]
		if len(a.other) != len(b.other) {
			return len(a.other) > len(b.other)
		}
		if kudos[order[i]] != kudos[order[j]] {
			return kudos[order[i]] > kudos[order[j]]
		}
		return order[i] < order[j]
	})
	if len(order) > limit {
		order = order[:limit]
	}

	selected, err := e.Corpus.CandidateRowsSlim(ctx, order)
	if err != nil {
		return out, fmt.Errorf("bridges: load selected: %w", err)
	}
	cands, err := e.toCandidates(ctx, selected)
	if err != nil {
		return out, fmt.Errorf("bridges: tags: %w", err)
	}
	byID := make(map[int64]rank.Candidate, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}
	for _, id := range order {
		c, ok := byID[id]
		if !ok {
			continue
		}
		other := bridges[id].other
		c.Evidence = []rank.Evidence{{
			Signal: "fandom_bridge",
			Value:  float64(len(other)),
			Reason: "also in " + strings.Join(other, ", "),
		}}
		out.Items = append(out.Items, Bridge{Candidate: c, OtherFandoms: other})
	}
	if out.Items == nil {
		out.Items = []Bridge{}
	}
	return out, nil
}

func makeMarks(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
