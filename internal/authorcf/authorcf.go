// Package authorcf answers the question `/api/v1/authors/similar` gets only half of (#35):
// which authors do the same PEOPLE read, as opposed to which authors write the same tags.
//
// The mirror has no author table -- a byline is a string on `works.authors` -- and no
// ratings. What it has is 180,677 bookmarks by 6,261 readers, the only evidence in the
// archive of anyone CHOOSING anything. `engine.SimilarAuthors` ranks authors by
// IDF-weighted tag-profile cosine: a good proxy, still a proxy, because it can only relate
// authors whose works are tagged alike. Two writers of the same fandom with different tag
// habits are invisible to it; two writers of different fandoms who share a tag vocabulary
// are indistinguishable.
//
// Co-readership is the other half, and it needs no tags at all: two authors are related
// when the same readers read both. It can only relate authors somebody actually read.
//
// THIS PACKAGE IS BUILT TO BE MEASURED BEFORE IT IS WIRED. `Measure` runs the experiment
// the row needs: hold out the last author each reader picked up, rank candidates two ways
// from the reader's most-read other author, and report recall@k for both. A signal that
// cannot beat the proxy on held-out data is a signal that should not be wired, and
// measuring first exists here because "obviously better" has been wrong before (the walk,
// ALS).
package authorcf

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// notAPerson holds bylines the mirror uses for works with no author. They are real rows and
// real readership -- orphan_account has 1,889 works read by 1,585 people, Anonymous 1,650 --
// so no data rule excludes them, and they would rank near the top of almost every list. An
// author recommendation that says "orphan_account" is not a recommendation, so they are
// excluded as CANDIDATES and allowed as seeds: asking who reads the orphan account is a fair
// question, answering "the orphan account" is not.
var notAPerson = map[string]struct{}{
	"orphan_account": {},
	"anonymous":      {},
}

func isAPerson(key string) bool {
	_, no := notAPerson[key]
	return !no
}

// Pair is one co-readership observation: how many readers read both authors.
type Pair struct {
	// Author is the corpus's own spelling.
	Author string
	// Key is the normalised name, for lookups.
	Key string
	// Readers is the number of distinct readers who bookmarked at least one work by each
	// author. It counts PEOPLE, not works: a prolific writer does not score higher for
	// having more works unless more people read them.
	Readers int
}

// Index is the co-readership table for one mirror, in both directions.
//
// Author keys are `arena.NormalizeAuthorName`'d -- the same rule `/author` and
// `SimilarAuthors` use, so the three surfaces cannot disagree about who an author is. The
// documented cost of that rule applies: two people sharing a name collide.
type Index struct {
	byAuthor map[string]map[int64]struct{}
	byReader map[int64]map[string]struct{}
	display  map[string]string
}

// Build loads the index from the interaction table.
//
// The whole table, in memory: 180k rows over 6.3k readers. That is the size at which this
// needs no SQL cleverness and no cache invalidation -- built per process, dropped with it.
// BOTH directions are kept because the two questions are different: "who reads this
// author" (a set intersection over readers, for a seed author) and "what else does this
// reader read" (a walk from a reader, which is what makes the query linear rather than
// quadratic).
func Build(ctx context.Context, db *sql.DB) (*Index, error) {
	if db == nil {
		return nil, fmt.Errorf("author co-readership: no corpus database")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT i.user_id, w.id, COALESCE(w.authors, '')
		FROM user_work_interactions i
		JOIN works w ON w.id = i.work_id
		WHERE i.interaction_type = 'bookmarked'`)
	if err != nil {
		return nil, fmt.Errorf("author co-readership: %w", err)
	}
	defer rows.Close()

	idx := &Index{
		byAuthor: map[string]map[int64]struct{}{},
		byReader: map[int64]map[string]struct{}{},
		display:  map[string]string{},
	}
	for rows.Next() {
		var user, work int64
		var byline string
		if err := rows.Scan(&user, &work, &byline); err != nil {
			return nil, fmt.Errorf("author co-readership: scan: %w", err)
		}
		readers := idx.byReader[user]
		if readers == nil {
			readers = map[string]struct{}{}
			idx.byReader[user] = readers
		}
		for _, raw := range arena.SplitAuthors(byline) {
			key := arena.NormalizeAuthorName(raw)
			if key == "" {
				continue
			}
			set := idx.byAuthor[key]
			if set == nil {
				set = map[int64]struct{}{}
				idx.byAuthor[key] = set
				idx.display[key] = arena.NameWithoutPseud(raw)
			}
			set[user] = struct{}{}
			readers[key] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("author co-readership: rows: %w", err)
	}
	return idx, nil
}

// Readers is how many distinct readers the corpus records for an author.
func (i *Index) Readers(author string) int { return len(i.byAuthor[arena.NormalizeAuthorName(author)]) }

// Authors lists every author key with at least one reader, sorted.
func (i *Index) Authors() []string {
	out := make([]string, 0, len(i.byAuthor))
	for key := range i.byAuthor {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Display is the corpus's own spelling of an author key.
func (i *Index) Display(key string) string {
	if s, ok := i.display[key]; ok {
		return s
	}
	return key
}

// ReaderAuthors returns the authors one reader has read.
func (i *Index) ReaderAuthors(user int64) []string {
	set := i.byReader[user]
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// CoReaders ranks authors by how many of the seed author's readers also read them.
//
// The score is a count of PEOPLE and the ranking is deliberately unnormalised: dividing by
// the candidate's own readership would rank a tiny author read by twelve of the seed's
// readers above a large one read by two hundred, which is the wrong answer to "who do these
// people read". `Readers` on the caller's side shows when a pair is a twelfth of a small
// audience.
//
// COST: one walk per reader of the seed author over THAT reader's authors (~30), not a scan
// of the author table per reader. The first version did the latter -- 6,261 readers x
// 63,296 authors of set lookups per request -- which is the shape to avoid.
func (i *Index) CoReaders(seed string, limit int) []Pair {
	key := arena.NormalizeAuthorName(seed)
	seedReaders := i.byAuthor[key]
	if len(seedReaders) == 0 {
		return nil
	}
	counts := make(map[string]int, 256)
	for reader := range seedReaders {
		for author := range i.byReader[reader] {
			if author == key || !isAPerson(author) {
				continue
			}
			counts[author]++
		}
	}
	return i.rank(counts, limit)
}

// CoReadersFrom ranks authors co-read by the readers of ANY of the given authors.
//
// This is what a reader's own history asks ("who else do my authors' readers read"), and
// it is the arm the evaluator compares against the tag profile.
func (i *Index) CoReadersFrom(seeds []string, limit int) []Pair {
	counts := make(map[string]int, 256)
	exclude := make(map[string]struct{}, len(seeds))
	for _, s := range seeds {
		exclude[arena.NormalizeAuthorName(s)] = struct{}{}
	}
	// Each reader votes ONCE per candidate, however many of the seeds they read: a reader
	// who has read six of the seed authors is six readings of the same person, not six
	// people.
	seen := make(map[string]map[int64]struct{}, 256)
	for _, s := range seeds {
		for reader := range i.byAuthor[arena.NormalizeAuthorName(s)] {
			for author := range i.byReader[reader] {
				if _, skip := exclude[author]; skip || !isAPerson(author) {
					continue
				}
				set := seen[author]
				if set == nil {
					set = map[int64]struct{}{}
					seen[author] = set
				}
				set[reader] = struct{}{}
			}
		}
	}
	for author, set := range seen {
		counts[author] = len(set)
	}
	return i.rank(counts, limit)
}

func (i *Index) rank(counts map[string]int, limit int) []Pair {
	out := make([]Pair, 0, len(counts))
	for author, n := range counts {
		out = append(out, Pair{Author: i.Display(author), Key: author, Readers: n})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Readers != out[b].Readers {
			return out[a].Readers > out[b].Readers
		}
		return out[a].Key < out[b].Key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Key normalises an author name the way the index does.
func Key(author string) string { return arena.NormalizeAuthorName(author) }
