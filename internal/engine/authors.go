package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/authorcf"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// Author similarity bounds. Authors are not entities in the mirror: a byline
// is a string on each work, so the profile below is a SAMPLE of the works whose
// byline matches, not a complete bibliography. The constants are named rather
// than inlined because the sample size is a claim about the answer, and the
// evidence string says which sample produced it.
// ErrAuthorProfileEmpty means the author EXISTS but none of their tags is shared widely
// enough to relate them to anyone. It is distinct from corpus.ErrNotFound on purpose: an
// author with 26 works is not missing, and a 404 for them would read as "this person is not
// in the mirror".
var ErrAuthorProfileEmpty = errors.New("no shareable tags")

const (
	similarAuthorSeedWorks  = 500
	similarAuthorCandidates = 5000
	similarAuthorSeedTags   = 24
	// similarAuthorMinDF is the floor on a tag's document frequency before it may join the
	// profile. A tag on one or two works in the whole archive cannot relate two authors: it
	// can connect at most one other work, so a profile built from it has no neighbourhood to
	// search. Measured on the live mirror: `Masterweaver` has 26 works and 196 distinct tags,
	// 37 of them on a SINGLE work -- and because the profile keeps the 24 RAREST tags, all 24
	// were singletons and the endpoint returned an empty list for an author with 61 readers.
	// The rarest-first rule is right; it needs to be rarest-first among tags that are shared.
	similarAuthorMinDF = 3
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
	// Readers is how many of the seed author's readers also read this author (#35). Zero
	// means the co-readership arm did not place them, not that nobody read them.
	Readers int `json:"readers"`
	// ReadShare is Readers over the seed author's own readership, which is what the merged
	// ranking orders by. It is a share of PEOPLE, so it does not reward prolific authors.
	ReadShare float64 `json:"read_share"`
	// Evidence is the "why this author?" line, with one entry per arm that placed them: the
	// shared tags, rarest first, and/or the readers. A similarity number without the reason
	// it was produced is the kind of answer this project exists to avoid.
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
// SimilarAuthors ranks authors near the named author.
//
// ## Two arms, and the measurement that ordered them
//
// The tag-profile arm below answers "who writes alike": it is exact about tags and blind to
// readers. `CoReaders` answers "who do these readers read". Measured on the live mirror with
// `kindred author-eval` (40 seed authors, 5,755 held-out pairs, k=10): recall 0.0217 for the
// tag profile against 0.0412 for co-readership, co-readership better on 32 seeds and worse on
// 1 (sign test z=5.4). So when a co-readership index is loaded the two are MERGED, ranked by
// the share of the seed's readers who read the candidate, and each row carries the evidence
// of whichever arm placed it.
//
// The candidate set in that measurement is defined by co-readership, which advantages that
// arm -- disclosed in docs/HANDOFF-2026-10-24k-authorcf.md rather than adjusted away.
func (e *Engine) SimilarAuthors(ctx context.Context, author string, limit int) ([]SimilarAuthor, error) {
	author = strings.TrimSpace(author)
	if author == "" {
		return nil, fmt.Errorf("similar authors: an author name is required")
	}
	if limit <= 0 {
		limit = 10
	}
	if e.CoReaders == nil {
		return e.similarAuthorsByTags(ctx, author, limit)
	}
	// Both arms answer over the same candidates as far as they can: the tag arm is asked for
	// its own cap, the co-reader arm for three times the requested depth so the merge has
	// something to interleave.
	tagged, tagErr := e.similarAuthorsByTags(ctx, author, similarAuthorsMaxLimit)
	if tagErr != nil && !errors.Is(tagErr, ErrAuthorProfileEmpty) && !errors.Is(tagErr, corpus.ErrNotFound) {
		return nil, tagErr
	}
	pairs := e.CoReaders.CoReaders(author, limit*3)
	if len(pairs) == 0 && len(tagged) == 0 {
		if tagErr != nil {
			return nil, tagErr
		}
		return []SimilarAuthor{}, nil
	}
	merged := e.mergeAuthors(tagged, pairs, author, limit)
	return merged, nil
}

// similarAuthorsMaxLimit is the deepest list the tag arm will produce (its own cap).
const similarAuthorsMaxLimit = 50

// mergeAuthors interleaves the two arms.
//
// Order: share of the seed's readers who read the candidate, then tag score, then name. A
// candidate on the tag arm only keeps its cosine and ranks after every co-read candidate
// with a reader -- one shared reader is weak evidence, but a tag-only match is no evidence
// about readers at all, and the measured question is what readers do.
func (e *Engine) mergeAuthors(tagged []SimilarAuthor, pairs []authorcf.Pair, seed string, limit int) []SimilarAuthor {
	seedReaders := e.CoReaders.Readers(seed)
	byKey := make(map[string]*SimilarAuthor, len(tagged))
	out := make([]SimilarAuthor, 0, len(tagged)+len(pairs))
	for _, t := range tagged {
		t := t
		byKey[authorcf.Key(t.Name)] = &t
	}
	for _, p := range pairs {
		item := SimilarAuthor{Name: p.Author, Readers: p.Readers}
		if seedReaders > 0 {
			item.ReadShare = float64(p.Readers) / float64(seedReaders)
		}
		item.Evidence = append(item.Evidence, rank.Evidence{
			Signal: "author_co_readers",
			Value:  item.ReadShare,
			Reason: fmt.Sprintf("%d of %q's %d readers also read this author (%.0f%%)",
				p.Readers, seed, seedReaders, 100*item.ReadShare),
		})
		if t, ok := byKey[p.Key]; ok {
			item.Score = t.Score
			item.SharedWorks = t.SharedWorks
			item.Evidence = append(item.Evidence, t.Evidence...)
			delete(byKey, p.Key)
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ReadShare != out[j].ReadShare {
			return out[i].ReadShare > out[j].ReadShare
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	rest := make([]SimilarAuthor, 0, len(byKey))
	for _, t := range byKey {
		rest = append(rest, *t)
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].Score != rest[j].Score {
			return rest[i].Score > rest[j].Score
		}
		return rest[i].Name < rest[j].Name
	})
	out = append(out, rest...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SimilarAuthorsByTags ranks by the tag profile only -- the ordering this endpoint shipped
// before #35. It stays exported so `?by=tags` can reproduce that ordering: a ranking the
// project replaced should remain reachable while anyone might want to compare.
func (e *Engine) SimilarAuthorsByTags(ctx context.Context, author string, limit int) ([]SimilarAuthor, error) {
	return e.similarAuthorsByTags(ctx, author, limit)
}

// similarAuthorsByTags is the tag-profile arm: the original implementation, unchanged.
func (e *Engine) similarAuthorsByTags(ctx context.Context, author string, limit int) ([]SimilarAuthor, error) {
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
	//
	// A tag on fewer than similarAuthorMinDF works is dropped rather than prized: it is the
	// rarest kind of tag, so rarest-first would keep it, and it can relate this author to at
	// most one other work -- which is not a neighbourhood. The floor is applied BEFORE the
	// idf weight, because the weight is exactly what made singletons win.
	freq, total := e.docFrequenciesForTagIDs(ctx, profile)
	floor := float64(e.AuthorMinDF)
	if floor <= 0 {
		floor = similarAuthorMinDF
	}
	// NO TABLE, NO FLOOR. Without the document frequencies every tag reads as df=0, and a
	// floor applied to that number deletes the whole profile -- a mirror that has not been
	// ingested would answer "no author shares these tags" for every author. The floor is a
	// statement about a table, so it only applies when there is one.
	if total <= 1 {
		floor = 0
	}
	shared := 0
	for id := range profile {
		if freq[id] < floor {
			delete(profile, id)
			continue
		}
		profile[id] *= idfOf(total, freq[id])
		if profile[id] <= 0 {
			delete(profile, id)
			continue
		}
		shared++
	}
	if shared == 0 {
		return []SimilarAuthor{}, fmt.Errorf("%w: %q's tags are all on fewer than %.0f works in this mirror, so no author can share them",
			ErrAuthorProfileEmpty, author, floor)
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

// docFrequenciesForTagIDs returns the document-frequency table for the
// author-similarity profile.
//
// It reads the LOADED table, not a query: the query cost 12.5 s per request
// on a real mirror, and the author page would have been slower still. A nil
// table returns empty, and the caller degrades to a plain tag count, which
// is worse but not a wrong answer.
func (e *Engine) docFrequenciesForTagIDs(_ context.Context, _ map[int32]float64) (map[int32]float64, float64) {
	if e == nil || e.DocFreq == nil {
		return map[int32]float64{}, 0
	}
	return e.DocFreq.ByTag, e.DocFreq.Total
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
