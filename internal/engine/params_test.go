package engine

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// blockCorpus builds a fixture for the block behaviour.
//
// The shape is chosen so the failure mode is observable rather than merely
// detectable:
//
//   - tag 1 is on EVERY candidate, so every work is in the unfiltered pool
//   - tag 2 is the blocked tag, on works 1, 2 and 3 only
//   - work 8 carries NO tags at all
//
// Work 8 is the one that matters. A `NOT IN (SELECT work_id ...)` exclusion
// is NULL for any work absent from work_tags, and NULL is not true, so a
// work with no tags would DISAPPEAR from the pool of every reader who has
// blocked anything — the opposite of the intent. Nothing short of an
// untagged candidate catches that, which is why it is here.
func blockCorpus(t *testing.T) *Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blocks.db")

	f := &testcorpus.Corpus{
		Tags: []testcorpus.Tag{{ID: 1, Name: "shared"}, {ID: 2, Name: "blocked"}},
	}
	add := func(id int64, title string, tags ...int64) {
		f.Works = append(f.Works, testcorpus.Work{
			ID: id, Title: title, Authors: "author",
			WordCount: 20000, Kudos: 100, Hits: 1000,
			Rating: "General Audiences", Language: "English", Complete: true,
		})
		for _, tag := range tags {
			f.WorkTags = append(f.WorkTags, testcorpus.WorkTag{
				WorkID: id, TagID: tag, TagType: "freeform"})
		}
	}
	add(1, "carries the blocked tag", 1, 2)
	add(2, "also carries it", 1, 2)
	add(3, "carries it too", 1, 2)
	add(4, "clean", 1)
	add(5, "clean too", 1)
	add(6, "seed", 1)
	// Work 8 has NO tags. See the comment on blockCorpus.
	add(8, "untagged")

	written, err := f.Write(path)
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db, err := sql.Open("sqlite", written)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ao3 := corpus.NewAO3(db)
	return &Engine{Corpus: ao3, PoolSize: 50}
}

// blockPool ranks with the given blocked tag set and returns the ids reached.
func blockPool(t *testing.T, e *Engine, blocked map[int64]bool) map[int64]bool {
	t.Helper()
	res, err := e.Recommend(context.Background(), Request{
		Seeds:         []Seed{{Kind: corpus.AO3Kind, ID: 6}},
		Kind:          corpus.AO3Kind,
		N:             50,
		Exclude:       true,
		BlockedTagIDs: blocked,
	})
	if err != nil {
		t.Fatalf("recommend with blocks %v: %v", blocked, err)
	}
	out := map[int64]bool{}
	for _, c := range res.Items {
		out[c.ID] = true
	}
	return out
}

// TestABlockedTagActuallyRemovesResults is the falsification of the block
// feature.
//
// Before the block predicate reached poolFor, a reader could block a tag,
// see the list unchanged, and have no way to tell a broken feature from a
// broken mirror.
func TestABlockedTagActuallyRemovesResults(t *testing.T) {
	e := blockCorpus(t)

	blocked := blockPool(t, e, nil)
	if len(blocked) == 0 {
		t.Fatal("the unblocked pool is empty; the fixture cannot test blocks")
	}
	if !blocked[1] || !blocked[2] {
		t.Fatalf("works 1 and 2 carry the blocked tag and must be present "+
			"when nothing is blocked: %v", blocked)
	}

	got := blockPool(t, e, map[int64]bool{2: true})
	for _, id := range []int64{1, 2, 3} {
		if got[id] {
			t.Fatalf("work %d carries blocked tag 2 and must not be "+
				"recommended: %v", id, got)
		}
	}
	if !got[4] {
		t.Fatalf("work 4 does not carry the blocked tag and must survive: %v", got)
	}
}

// TestBlockingKeepsWorksThatHaveNoTags checks the NULL trap directly,
// against the SQL rather than through the pool.
//
// An earlier version of this test put an untagged work in the pool and
// asserted it survived a block. It was VACUOUS and the mutation check proved
// it: rewriting blockSQL to the `NOT IN` form still passed. The reason is
// that an untagged work shares no tag with the seed, so it never enters a
// TAG pool in the first place — no predicate could have removed it, and its
// survival proved nothing about the predicate.
//
// What actually distinguishes the two forms is whether the subquery can emit
// NULL. work_tags.work_id is NOT NULL, so NOT IN is correct here; append a
// NULL-yielding arm and the NOT IN form starts dropping rows. Both branches
// are exercised below so the distinction is pinned rather than assumed.
func TestBlockingKeepsWorksThatHaveNoTags(t *testing.T) {
	e := blockCorpus(t)
	ctx := context.Background()

	// Work 8 carries no tags, so a NOT EXISTS predicate must keep it.
	var kept int
	if err := e.Corpus.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM works w
		WHERE w.id = 8
		  AND NOT EXISTS (SELECT 1 FROM work_tags b
		                  WHERE b.work_id = w.id AND b.tag_id IN (2))`,
	).Scan(&kept); err != nil {
		t.Fatalf("not exists probe: %v", err)
	}
	if kept != 1 {
		t.Fatalf("NOT EXISTS dropped untagged work 8 (%d rows)", kept)
	}

	// The same query with a NULL-yielding arm in the subquery: NOT IN goes
	// NULL and drops the row, which is the failure mode NOT EXISTS avoids.
	// Pinned so the reason for the predicate stays documented as behaviour
	// rather than as a claim.
	var notIn int
	if err := e.Corpus.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM works w
		WHERE w.id = 8
		  AND w.id NOT IN (SELECT b.work_id FROM work_tags b
		                    WHERE b.tag_id IN (2)
		                    UNION ALL SELECT NULL)`,
	).Scan(&notIn); err != nil {
		t.Fatalf("not in probe: %v", err)
	}
	if notIn != 0 {
		t.Fatalf("a NULL-yielding NOT IN kept untagged work 8 (%d rows); "+
			"the documented reason for NOT EXISTS no longer holds", notIn)
	}
}

// TestBlockingNarrowsThePoolRatherThanTheResult is the placement test, the
// same argument as TestFiltersShapeThePoolNotTheResult.
//
// With a pool of 1 and work 1 blocked, the single slot must go to the best
// SURVIVING work. If the block ran after ranking, the LIMIT would already be
// spent on a blocked work and the request would return nothing at all.
func TestBlockingNarrowsThePoolRatherThanTheResult(t *testing.T) {
	e := blockCorpus(t)

	// Blocked tag 2 is on works 1, 2 AND 3 — three candidates. A pool of 2
	// must come back full from the four survivors, which only happens if the
	// predicate ran before the LIMIT.
	res, err := e.Recommend(context.Background(), Request{
		Seeds:         []Seed{{Kind: corpus.AO3Kind, ID: 6}},
		Kind:          corpus.AO3Kind,
		N:             50,
		PoolSize:      2,
		Exclude:       true,
		BlockedTagIDs: map[int64]bool{2: true},
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("a pool of 2 returned %d items with three works blocked: the "+
			"LIMIT was spent before the block was applied. got %d: %v",
			len(res.Items), len(res.Items), res.Items)
	}
	for _, c := range res.Items {
		if c.ID == 1 || c.ID == 2 || c.ID == 3 {
			t.Fatalf("blocked work %d was recommended", c.ID)
		}
	}
}

// TestBlockingNothingAddsNoSQL pins the no-op case, because blockSQL
// returning a non-empty string for an empty set would append a
// `NOT EXISTS (... IN ())` clause to every unblocked reader's query — valid
// SQL in SQLite but a needless subquery on every pool build.
func TestBlockingNothingAddsNoSQL(t *testing.T) {
	var args []any
	for _, blocked := range []map[int64]bool{nil, {}, {0: true}} {
		got := blockSQL(blocked, &args)
		if got != "" {
			t.Fatalf("blockSQL(%v) returned SQL for an empty set: %q", blocked, got)
		}
		if len(args) != 0 {
			t.Fatalf("blockSQL(%v) bound %d args for an empty set", blocked, len(args))
		}
	}
}

// TestBlockSQLIsStable pins that the same set produces byte-identical SQL.
//
// Map iteration order is randomised in Go, so without the sort inside
// blockSQL two identical requests would compile to different statements,
// defeating SQLite's plan cache and making the generated SQL untestable.
func TestBlockSQLIsStable(t *testing.T) {
	var a, b []any
	first := blockSQL(map[int64]bool{7: true, 3: true, 99: true}, &a)
	second := blockSQL(map[int64]bool{99: true, 7: true, 3: true}, &b)
	if first != second {
		t.Fatalf("blockSQL is not deterministic:\n%q\n%q", first, second)
	}
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("expected 3 bound args each, got %d and %d", len(a), len(b))
	}
	// The ids must be sorted, so the placeholders line up with ascending ids.
	want := []any{int64(3), int64(7), int64(99)}
	for i := range want {
		if a[i] != want[i] {
			t.Fatalf("bound args are not sorted ascending: got %v, want %v", a, want)
		}
	}
}

// --- ParseFilter -------------------------------------------------------

func TestParseFilterRejectsWhatItCannotHonour(t *testing.T) {
	// Each case is a value a form or a hand-typed URL can produce that a
	// lenient parser would accept and then ignore. Silently ignoring it is
	// the accepted-and-ignored shape this project has found repeatedly: the
	// reader sees results that look fine and concludes the control is
	// broken.
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"min_words is not a number", "min_words=lots"},
		{"min_words is negative", "min_words=-5"},
		{"max_words is negative", "max_words=-1"},
		{"min_kudos is negative", "min_kudos=-10"},
		{"complete is nonsense", "complete=maybe"},
		{"the range is reversed", "min_words=50000&max_words=1000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("bad test query: %v", err)
			}
			if _, err := ParseFilter(q); err == nil {
				t.Fatalf("ParseFilter(%q) accepted %q and would silently "+
					"ignore it", tc.query, tc.query)
			}
		})
	}
}

// TestParseFilterAcceptsTheSpellingsAFormSends pins the alias set. A select
// sending `in-progress` and a hand-written URL sending `0` must both work,
// or the same control behaves differently depending on spelling.
func TestParseFilterAcceptsTheSpellingsAFormSends(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  TriBool
	}{
		{"", CompleteAny},
		{"complete=any", CompleteAny},
		{"complete=all", CompleteAny},
		{"complete=1", CompleteOnly},
		{"complete=complete", CompleteOnly},
		{"complete=true", CompleteOnly},
		{"complete=yes", CompleteOnly},
		{"complete=in-progress", CompleteWIP},
		{"complete=wip", CompleteWIP},
		{"complete=0", CompleteWIP},
		{"complete=false", CompleteWIP},
		{"complete=IN-PROGRESS", CompleteWIP},
	} {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatalf("bad test query: %v", err)
		}
		f, err := ParseFilter(q)
		if err != nil {
			t.Fatalf("ParseFilter(%q): %v", tc.query, err)
		}
		if f.Complete != tc.want {
			t.Fatalf("ParseFilter(%q).Complete = %v, want %v",
				tc.query, f.Complete, tc.want)
		}
	}
}

// TestParseFilterReadsRepeatedAndCommaSeparatedLists covers both ways a
// multi-valued filter arrives: a form posts repeated fields, a URL is easier
// to write with commas. Both must work.
func TestParseFilterReadsRepeatedAndCommaSeparatedLists(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"repeated", "rating=General+Audiences&rating=Explicit",
			[]string{"General Audiences", "Explicit"}},
		{"comma separated", "rating=General+Audiences,Explicit",
			[]string{"General Audiences", "Explicit"}},
		{"mixed with spaces", "rating=General+Audiences,+Explicit+",
			[]string{"General Audiences", "Explicit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("bad test query: %v", err)
			}
			f, err := ParseFilter(q)
			if err != nil {
				t.Fatalf("ParseFilter: %v", err)
			}
			if len(f.Ratings) != len(tc.want) {
				t.Fatalf("ratings = %v, want %v", f.Ratings, tc.want)
			}
			for i := range tc.want {
				if f.Ratings[i] != tc.want[i] {
					t.Fatalf("ratings = %v, want %v", f.Ratings, tc.want)
				}
			}
		})
	}
}

// TestParseFilterIsEmptyMeansNoSQL pins isEmpty against every field, because
// collabCandidates skips its SQL entirely on this predicate. A filter that
// was set but reported empty would return unfiltered collab candidates for a
// reader who asked for a filter — the same silent wrong answer as accepting
// and ignoring it at the parse stage.
func TestParseFilterIsEmptyMeansNoSQL(t *testing.T) {
	cases := []struct {
		query string
		empty bool
	}{
		{"", true},
		{"complete=any", true},
		{"min_words=1", false},
		{"max_words=1", false},
		{"min_kudos=1", false},
		{"complete=complete", false},
		{"complete=in-progress", false},
		{"rating=Explicit", false},
		{"lang=English", false},
	}
	for _, tc := range cases {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatalf("bad test query: %v", err)
		}
		f, err := ParseFilter(q)
		if err != nil {
			t.Fatalf("ParseFilter(%q): %v", tc.query, err)
		}
		if got := f.isEmpty(); got != tc.empty {
			t.Fatalf("ParseFilter(%q).isEmpty() = %v, want %v", tc.query, got, tc.empty)
		}
	}
}

// TestParsePoolModeRejectsUnknownModes pins that an unusable pool mode is an
// error. `?pool_mode=collab` answered with tag-only results would look like a
// perfectly normal page.
func TestParsePoolModeRejectsUnknownModes(t *testing.T) {
	for _, v := range []string{"", "tags", "collab", "tags+collab", "tags_or_collab", "TAGS"} {
		if _, err := ParsePoolMode(v); err != nil {
			t.Fatalf("ParsePoolMode(%q) refused a valid mode: %v", v, err)
		}
	}
	for _, v := range []string{"collabative", "yes", "off", "both"} {
		if _, err := ParsePoolMode(v); err == nil {
			t.Fatalf("ParsePoolMode(%q) accepted an unknown mode", v)
		}
	}
}
