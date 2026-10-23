// Package eval measures a recommender against held-out bookmarks.
//
// ## Why this package exists
//
// The arena asks readers which of two works is better, which measures a
// ranking one pair at a time and only for readers who visit the arena. The
// tagged/weighted signals, on the other hand, change on every tune and every
// corpus rebuild, and before this package nothing could say whether a change
// made a ranking better or merely different. That is the failure mode this
// project keeps finding: a feature that is wired, plausible, and unmeasured.
//
// The measure is deliberately the simplest one that is honest. For each
// reader in the mirror's `user_work_interactions`, hold out a fraction of
// what they bookmarked, rank from the rest, and ask whether the held-out
// works appear near the top. Recall@K says "did we find what they chose";
// NDCG@K says "and were the finds near the top". Both are reported, because
// either alone is easy to flatter: returning every work in the corpus scores
// 1.0 recall and a terrible NDCG, while returning one lucky guess scores a
// good NDCG for one reader and nothing for the rest.
//
// ## What this is NOT
//
// A bookmark is not a rating. The mirror records what a reader chose to keep,
// not how much they liked it, so this measures agreement with observed
// behaviour and cannot tell a reread from a hoard. The report says so and
// does not dress the number up as preference.
//
// The recommender is an interface rather than the engine itself so this
// package stays a measurement of a RANKER, not of the graph, the corpus
// loader, or the SQLite driver. That is what lets the arithmetic be tested
// with a fake that returns a known order; the engine-backed adapter lives in
// cmd/kindred/eval.go.
package eval

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

// Recommender returns up to n entity ids for a set of seed ids.
//
// It must not return the seeds themselves. Exclusion is the recommender's
// job because only it knows what its pool contains; an evaluation that
// silently counted a seed as a hit would report the recommender as perfect
// at the one task it has not done.
type Recommender interface {
	Recommend(ctx context.Context, seeds []int64, n int) ([]int64, error)
}

// BooksToHold is the default hold-out ratio the CLI exposes as --ratio.
//
// A fifth is large enough that most readers with five or more bookmarks
// contribute at least one test item, and small enough that the remaining
// four fifths still describe the reader. Holding out half would measure a
// recommender given half a reader's taste, which is a different and easier
// question than the one production answers.
const DefaultRatio = 0.2

// DefaultK is the depth the metrics are reported at, matching the CLI's
// --n default. Ten is the size of the lists this project actually renders.
const DefaultK = 10

// Split is one deterministic train/test partition of the mirror's
// bookmarks.
type Split struct {
	// Train is what the recommender may see.
	Train map[int64][]int64
	// Test is what it must find. Disjoint from Train for every user.
	Test map[int64][]int64
	// Users is the number of readers with at least one bookmark, and
	// Bookmarks the number of distinct (user, work) pairs read.
	Users     int
	Bookmarks int
	// TooFew is the number of readers who could not be split at all: fewer
	// than two distinct bookmarks. They are reported rather than silently
	// dropped, because a corpus of one-bookmark readers would otherwise
	// produce an empty report that looks like a clean run.
	TooFew int
}

// LoadBookmarks reads distinct bookmarked works per user.
//
// Two filters are load-bearing:
//
//   - interaction_type = 'bookmarked'. The mirror also carries
//     'bookmarker', a row ABOUT a work carrying a user id, and counting it
//     attributes one reader's history from another's action.
//   - SELECT DISTINCT. The real table carries duplicate rows (measured: 7
//     rows for 4 distinct works), so a COUNT(*) over it is wrong by a large
//     factor and a hold-out that could pick the same work twice would make
//     the split lie about its size.
func LoadBookmarks(ctx context.Context, db *sql.DB) (map[int64][]int64, int, error) {
	if db == nil {
		return nil, 0, fmt.Errorf("load bookmarks: no corpus database")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT user_id, work_id
		FROM user_work_interactions
		WHERE interaction_type = 'bookmarked'
		ORDER BY user_id, work_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("load bookmarks: %w", err)
	}
	defer rows.Close()

	out := map[int64][]int64{}
	users := 0
	for rows.Next() {
		var user, work int64
		if err := rows.Scan(&user, &work); err != nil {
			return nil, 0, fmt.Errorf("scan bookmark: %w", err)
		}
		if _, seen := out[user]; !seen {
			users++
		}
		out[user] = append(out[user], work)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read bookmarks: %w", err)
	}
	return out, users, nil
}

// SplitBookmarks partitions each reader's bookmarks deterministically.
//
// The shuffle is seeded from the user id, not from the clock, for the same
// reason the test corpus is deterministic: two runs of the same corpus must
// produce the same number, or a tune comparison is measuring the random
// number generator. Every user keeps at least one training item and at least
// one test item; a reader with fewer than two distinct bookmarks is counted
// in TooFew instead of contributing a vacuous perfect score.
func SplitBookmarks(byUser map[int64][]int64, ratio float64) Split {
	if ratio <= 0 || ratio >= 1 {
		ratio = DefaultRatio
	}
	split := Split{
		Train: map[int64][]int64{},
		Test:  map[int64][]int64{},
		Users: len(byUser),
	}
	for user, works := range byUser {
		uniq := dedupeSorted(works)
		split.Bookmarks += len(uniq)
		if len(uniq) < 2 {
			split.TooFew++
			continue
		}
		held := int(math.Round(float64(len(uniq)) * ratio))
		if held < 1 {
			held = 1
		}
		if held >= len(uniq) {
			held = len(uniq) - 1
		}
		// PCG with two fixed constants: one varies the stream per user, the
		// other names this package so a future split with a different
		// purpose cannot accidentally reproduce this one and be mistaken
		// for it.
		rng := rand.New(rand.NewPCG(uint64(user), 0x6b696e64726564))
		rng.Shuffle(len(uniq), func(i, j int) { uniq[i], uniq[j] = uniq[j], uniq[i] })
		split.Test[user] = uniq[:held]
		split.Train[user] = uniq[held:]
	}
	return split
}

// Report is the evaluation's answer.
type Report struct {
	// K is the depth every metric was computed at.
	K int
	// Users and Bookmarks describe the split that was evaluated.
	Users     int `json:"users"`
	Bookmarks int `json:"bookmarks"`
	TooFew    int `json:"too_few_bookmarks"`
	// Evaluated is the number of readers whose recommendation succeeded and
	// carried at least one held-out work to find.
	Evaluated int `json:"evaluated"`
	// Failures counts readers whose recommendation returned an error. They
	// are not counted as misses: a broken request and a bad ranking are
	// different facts, and folding them together would let an outage look
	// like a tune regression.
	Failures int      `json:"failures"`
	Errors   []string `json:"errors,omitempty"`
	// Hits is the number of held-out works found across all evaluated
	// readers; HeldOut is how many there were to find.
	Hits    int `json:"hits"`
	HeldOut int `json:"held_out"`
	// RecallAtK is the mean per-user recall, not Hits/HeldOut: averaging
	// per user keeps a prolific reader from dominating the number.
	RecallAtK float64 `json:"recall_at_k"`
	// NDCGAtK is the mean per-user NDCG with binary relevance.
	NDCGAtK float64 `json:"ndcg_at_k"`
	// Coverage is the share of evaluated readers for whom at least one
	// held-out work was found. A recommender can have weak recall and still
	// cover most readers, or the reverse, and the two are different
	// products.
	Coverage float64 `json:"coverage"`
	// Placements is where each held-out work landed, for the per-group
	// slices. It is the only per-work detail the report carries, and it is
	// emitted in JSON only when asked, because a full run has one row per
	// held-out bookmark.
	Placements []Placement `json:"placements,omitempty"`
}

// Placement is one held-out work and where the recommender put it.
//
// The aggregate metrics are computed per reader, because averaging per user
// keeps a prolific reader from dominating. A slice has the opposite shape: it
// asks "of the held-out works in this fandom, how many were found", which is a
// question about WORKS. So each work travels with its own rank and the slices
// are grouped from these rows — deriving them from the per-user sums after the
// fact is not possible, and recomputing the ranking per group would measure
// the recommender several times and call it one run.
type Placement struct {
	// Work is the held-out work's id.
	Work int64 `json:"work"`
	// Rank is 1-based; 0 means the recommender did not return it in the top K.
	Rank int `json:"rank"`
}

// Slice is one group's share of the held-out works.
//
// HitRate is hits/N over WORKS, so it is not the mean per-user recall and must
// not be printed beside it as though the two were the same statistic. It
// answers "how often does this fandom's held-out work get found".
type Slice struct {
	Axis    string  `json:"axis"`
	Name    string  `json:"name"`
	N       int     `json:"n"`
	Hits    int     `json:"hits"`
	HitRate float64 `json:"hit_rate"`
}

// ByGroup groups placements by every key the key function returns for a work.
//
// A work belongs to several groups (it carries several fandom tags), and it
// counts in each: a fandom's hit rate is about that fandom's works, not about
// a partition of the corpus. Groups smaller than minN are dropped, because a
// fandom with two held-out works has a hit rate of 0, 0.5 or 1 and a table
// that ranks it beside one with 300 is noise wearing a number.
//
// The result is ordered by N descending, then by name, so two runs over the
// same data print the same table in the same order.
func ByGroup(placements []Placement, axis string, minN int, key func(work int64) []string) []Slice {
	type acc struct{ n, hits int }
	groups := map[string]*acc{}
	for _, p := range placements {
		for _, name := range key(p.Work) {
			if name == "" {
				continue
			}
			a := groups[name]
			if a == nil {
				a = &acc{}
				groups[name] = a
			}
			a.n++
			if p.Rank > 0 {
				a.hits++
			}
		}
	}
	out := make([]Slice, 0, len(groups))
	for name, a := range groups {
		if a.n < minN {
			continue
		}
		out = append(out, Slice{
			Axis: axis, Name: name, N: a.n, Hits: a.hits,
			HitRate: float64(a.hits) / float64(a.n),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Run evaluates rec against split.
//
// It returns an error only when the context is cancelled or the split itself
// is unusable. A recommender that fails for one reader is counted in
// Failures with its reason attached, because a single missing work in a
// mirror must not throw away the other hundred readers' measurements -- but
// silently skipping it would make a broken deployment look like a clean
// evaluation.
func Run(ctx context.Context, rec Recommender, split Split, k int) (Report, error) {
	if k <= 0 {
		k = DefaultK
	}
	rep := Report{K: k, Users: split.Users, Bookmarks: split.Bookmarks, TooFew: split.TooFew}

	// Users are evaluated in id order so two runs over the same split do not
	// differ in which failures are reported first (or in floating-point
	// accumulation order).
	users := make([]int64, 0, len(split.Test))
	for user := range split.Test {
		users = append(users, user)
	}
	sort.Slice(users, func(i, j int) bool { return users[i] < users[j] })

	var recallSum, ndcgSum float64
	for _, user := range users {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		test := split.Test[user]
		train := split.Train[user]
		if len(test) == 0 || len(train) == 0 {
			continue
		}
		recs, err := rec.Recommend(ctx, train, k)
		if err != nil {
			rep.Failures++
			if len(rep.Errors) < 5 {
				rep.Errors = append(rep.Errors, fmt.Sprintf("user %d: %v", user, err))
			}
			continue
		}
		rep.Evaluated++
		rep.HeldOut += len(test)

		wanted := make(map[int64]bool, len(test))
		for _, id := range test {
			wanted[id] = true
		}
		hits := 0
		dcg := 0.0
		// The first occurrence of a work is the one that counts. A
		// recommender returning the same id twice must not be paid twice.
		seen := make(map[int64]bool, len(recs))
		rankOf := make(map[int64]int, len(recs))
		for i, id := range recs {
			if i >= k {
				break
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			if wanted[id] {
				hits++
				dcg += 1 / math.Log2(float64(i+2))
			}
			// The FIRST position counts, the same rule the metric uses: a
			// recommender that repeats a work must not get two placements.
			rankOf[id] = i + 1
		}
		// One placement per held-out work, in the split's own order, so a
		// slice is computed over the same set the aggregate was.
		for _, id := range test {
			rep.Placements = append(rep.Placements, Placement{Work: id, Rank: rankOf[id]})
		}
		rep.Hits += hits
		recallSum += float64(hits) / float64(len(test))
		ndcgSum += dcg / idealDCG(len(test), k)
		if hits > 0 {
			rep.Coverage++
		}
	}
	if rep.Evaluated > 0 {
		rep.RecallAtK = recallSum / float64(rep.Evaluated)
		rep.NDCGAtK = ndcgSum / float64(rep.Evaluated)
		rep.Coverage /= float64(rep.Evaluated)
	} else if rep.Failures > 0 {
		return rep, fmt.Errorf("every recommendation failed (%d readers); first: %s",
			rep.Failures, rep.Errors[0])
	}
	return rep, nil
}

// idealDCG is the DCG of the best possible ranking for a reader with n
// held-out works at depth k. Without it NDCG is a raw sum that rewards
// readers with more bookmarks, which is a property of the corpus rather than
// of the ranking.
func idealDCG(n, k int) float64 {
	if n > k {
		n = k
	}
	var d float64
	for i := 1; i <= n; i++ {
		d += 1 / math.Log2(float64(i+1))
	}
	if d == 0 {
		return 1
	}
	return d
}

func dedupeSorted(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 0
	for _, id := range out {
		if n == 0 || out[n-1] != id {
			out[n] = id
			n++
		}
	}
	return out[:n]
}
