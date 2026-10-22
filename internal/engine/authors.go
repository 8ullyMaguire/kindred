package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// Author similarity bounds. Authors are not entities in the mirror: a byline
// is a string on each work, so the profile below is a SAMPLE of the works whose
// byline matches, not a complete bibliography. The constants are named rather
// than inlined because the sample size is a claim about the answer, and the
// evidence string says which sample produced it.
const (
	similarAuthorSeedWorks  = 500
	similarAuthorCandidates = 5000
	similarAuthorSeedTags   = 24
)

// SimilarAuthor is one author near the named author, with the evidence.
type SimilarAuthor struct {
	Name string `json:"name"`
	// SharedWorks is how many of this author's works in the sampled pool
	// carried at least one of the seed author's distinguishing tags. It is
	// not a bibliography size, and the evidence string says so.
	SharedWorks int `json:"shared_works"`
	// Score is a cosine over the distinguishing tags both authors carry.
	Score float64 `json:"score"`
	// Evidence is the "why this author?" line: the shared tags, rarest
	// first. A similarity number without the tags that produced it is the
	// kind of answer this project exists to avoid.
	Evidence []rank.Evidence `json:"evidence,omitempty"`
}

// SimilarAuthors ranks authors whose works carry the same distinguishing tags
// as the named author.
//
// ## Why tags and not an author table
//
// The mirror has no author table. A byline is a string on `works.authors`,
// sometimes a comma-separated list. Any author identity is therefore derived,
// and this uses the same substring rule the /author page already uses, so the
// two surfaces cannot disagree about who an author is. That rule has the
// documented cost that two people sharing a name collide; inventing a finer
// identity from punctuation would invent relationships the corpus never
// asserted.
//
// ## Why the seed's RARE tags
//
// A profile built from every tag is dominated by "Fluff" and "General
// Audiences", which every author carries and which therefore says nothing.
// The seed's tags are ranked by document frequency and the rarest are kept, so
// the similarity means "these authors agree where the corpus discriminates".
// This is the same measurement that justifies the IDF-weighted work signal,
// applied to authors rather than works.
func (e *Engine) SimilarAuthors(ctx context.Context, author string, limit int) ([]SimilarAuthor, error) {
	author = strings.TrimSpace(author)
	if author == "" {
		return nil, fmt.Errorf("similar authors: an author name is required")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	if e.Corpus == nil {
		return nil, fmt.Errorf("similar authors: this engine has no corpus")
	}

	seedIDs, err := e.authorWorkIDs(ctx, author)
	if err != nil {
		return nil, err
	}
	if len(seedIDs) == 0 {
		return nil, fmt.Errorf("%w: no work in this mirror has byline %q", corpus.ErrNotFound, author)
	}

	seedPairs, err := e.Corpus.TagPairs(ctx, seedIDs)
	if err != nil {
		return nil, fmt.Errorf("similar authors: seed tags: %w", err)
	}
	// Profile: how often each tag appears across the seed author's works.
	profile := map[int32]float64{}
	names := map[int32]string{}
	for _, id := range seedIDs {
		for _, p := range seedPairs[id] {
			profile[p.ID]++
			names[p.ID] = p.Name
		}
	}
	if len(profile) == 0 {
		return []SimilarAuthor{}, nil
	}

	// Weight by rarity and keep the most distinguishing tags. A tag on every
	// work in the corpus contributes nothing; a tag on forty is the profile.
	freq, total := e.docFrequenciesForTagIDs(ctx, profile)
	for id := range profile {
		profile[id] *= idfOf(total, freq[id])
		if profile[id] <= 0 {
			delete(profile, id)
		}
	}
	if len(profile) == 0 {
		return []SimilarAuthor{}, nil
	}
	tagIDs := make([]int32, 0, len(profile))
	for id, w := range profile {
		if w > 0 {
			tagIDs = append(tagIDs, id)
		}
	}
	// Rarest first, which is both how the profile is trimmed and the order
	// the evidence line prints.
	sort.Slice(tagIDs, func(i, j int) bool {
		if freq[tagIDs[i]] != freq[tagIDs[j]] {
			return freq[tagIDs[i]] < freq[tagIDs[j]]
		}
		return tagIDs[i] < tagIDs[j]
	})
	if len(tagIDs) > similarAuthorSeedTags {
		tagIDs = tagIDs[:similarAuthorSeedTags]
	}
	keep := make(map[int32]bool, len(tagIDs))
	norm := 0.0
	for _, id := range tagIDs {
		keep[id] = true
		norm += profile[id] * profile[id]
	}
	if norm <= 0 {
		return []SimilarAuthor{}, nil
	}
	norm = math.Sqrt(norm)

	// Candidate works carrying any distinguishing tag. Ordered by kudos so a
	// cap that bites keeps the works a reader might open rather than an
	// arbitrary slice.
	args := make([]any, 0, len(tagIDs)+1)
	for _, id := range tagIDs {
		args = append(args, int64(id))
	}
	args = append(args, similarAuthorCandidates)
	rows, err := e.Corpus.DB.QueryContext(ctx, `
		SELECT w.id, COALESCE(w.authors,'')
		FROM works w
		JOIN work_tags wt ON wt.work_id = w.id
		WHERE wt.tag_id IN (`+makeMarks(len(tagIDs))+`)
		GROUP BY w.id
		ORDER BY MAX(w.kudos) DESC, w.id
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("similar authors: candidate works: %w", err)
	}
	candWorks := make([]int64, 0, 256)
	authorsOf := map[int64][]string{}
	for rows.Next() {
		var id int64
		var byline string
		if err := rows.Scan(&id, &byline); err != nil {
			rows.Close()
			return nil, fmt.Errorf("similar authors: scan candidate: %w", err)
		}
		candWorks = append(candWorks, id)
		authorsOf[id] = arena.SplitAuthors(byline)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("similar authors: candidate works: %w", err)
	}
	if len(candWorks) == 0 {
		return []SimilarAuthor{}, nil
	}
	candPairs, err := e.Corpus.TagPairs(ctx, candWorks)
	if err != nil {
		return nil, fmt.Errorf("similar authors: candidate tags: %w", err)
	}

	type acc struct {
		name   string
		works  int
		shared map[int32]bool
	}
	seedKey := arena.NormalizeAuthorName(author)
	byAuthor := map[string]*acc{}
	for _, workID := range candWorks {
		// The tags this work carries that are in the seed's profile.
		var workShared []int32
		for _, p := range candPairs[workID] {
			if keep[p.ID] {
				workShared = append(workShared, p.ID)
				names[p.ID] = p.Name
			}
		}
		if len(workShared) == 0 {
			continue
		}
		for _, raw := range authorsOf[workID] {
			key := arena.NormalizeAuthorName(raw)
			if key == "" || key == seedKey {
				continue
			}
			a := byAuthor[key]
			if a == nil {
				a = &acc{name: arena.NameWithoutPseud(raw), shared: map[int32]bool{}}
				byAuthor[key] = a
			}
			a.works++
			for _, id := range workShared {
				a.shared[id] = true
			}
		}
	}
	if len(byAuthor) == 0 {
		return []SimilarAuthor{}, nil
	}

	out := make([]SimilarAuthor, 0, len(byAuthor))
	for _, a := range byAuthor {
		sharedIDs := make([]int32, 0, len(a.shared))
		for id := range a.shared {
			sharedIDs = append(sharedIDs, id)
		}
		// Rarest first: the first tag is the one that did the work, and it
		// is the one a reader can argue with.
		sort.Slice(sharedIDs, func(i, j int) bool {
			if freq[sharedIDs[i]] != freq[sharedIDs[j]] {
				return freq[sharedIDs[i]] < freq[sharedIDs[j]]
			}
			return sharedIDs[i] < sharedIDs[j]
		})
		dot := 0.0
		sharedNames := make([]string, 0, len(sharedIDs))
		for _, id := range sharedIDs {
			dot += profile[id]
			sharedNames = append(sharedNames, names[id])
		}
		// Binary vector for the author over the seed's dimensions. Cosine
		// then measures agreement on the seed tags, not how prolific the
		// author is: a writer with two hundred works does not get a higher
		// similarity for having more of them.
		cosine := dot / (norm * math.Sqrt(float64(len(sharedIDs))))
		out = append(out, SimilarAuthor{
			Name:        a.name,
			SharedWorks: a.works,
			Score:       cosine,
			Evidence: []rank.Evidence{{
				Signal: "author_tag_profile",
				Value:  cosine,
				Reason: fmt.Sprintf("shares %d distinguishing tag%s with %q: %s%s",
					len(sharedIDs), plural(len(sharedIDs)), author,
					strings.Join(firstN(sharedNames, 4), ", "), moreSuffix(len(sharedNames), 4)),
			}},
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].SharedWorks != out[j].SharedWorks {
			return out[i].SharedWorks > out[j].SharedWorks
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// authorWorkIDs resolves the substring rule the /author page uses. It is
// deliberately the same LIKE rather than exact equality: a reader arrives at
// this method from a byline link, and a byline can carry a trailing
// " (username)" that the page's link also carries.
func (e *Engine) authorWorkIDs(ctx context.Context, author string) ([]int64, error) {
	needle := "%" + likeEscape(author) + "%"
	rows, err := e.Corpus.DB.QueryContext(ctx, `
		SELECT id FROM works
		WHERE authors LIKE ? ESCAPE '\'
		ORDER BY kudos DESC, id
		LIMIT ?`, needle, similarAuthorSeedWorks)
	if err != nil {
		return nil, fmt.Errorf("similar authors: seed works: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("similar authors: scan seed work: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// docFrequenciesForTagIDs reads document frequencies for a profile, degrading
// to zero rather than failing the request. A similarity with no frequencies is
// a plain tag count, which is worse but not a wrong answer.
func (e *Engine) docFrequenciesForTagIDs(ctx context.Context, profile map[int32]float64) (map[int32]float64, float64) {
	ids := make([]int32, 0, len(profile))
	for id := range profile {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	freq, total, err := e.Corpus.TagDocFreq(ctx, ids)
	if err != nil {
		return map[int32]float64{}, 0
	}
	return freq, total
}

// idfOf is the same correction the IDF-weighted work signal uses, kept local
// because the author profile is built in this file and a shared helper would
// couple two surfaces that are allowed to tune independently.
func idfOf(total, df float64) float64 {
	if total <= 1 {
		return 1
	}
	if df <= 0 {
		return math.Log(total)
	}
	if df >= total {
		return 0
	}
	return math.Log(total / df)
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func moreSuffix(len, shown int) string {
	if len > shown {
		return fmt.Sprintf(" and %d more", len-shown)
	}
	return ""
}

// likeEscape escapes the LIKE metacharacters. The author page has the same
// helper in package web; duplicating three lines is better than exporting a
// SQL-escaping detail from a page package into the engine.
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
