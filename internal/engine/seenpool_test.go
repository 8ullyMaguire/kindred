package engine

import (
	"context"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
)

// TestSeenSQLIsDeterministic: the predicate is built from a MAP, so map
// iteration order decides the placeholder order and therefore the SQL text.
//
// That is not cosmetic. SQLite caches query plans keyed on the SQL string, so
// a predicate whose text changes on every call compiles a fresh plan each
// time — on the hottest query in the request path, with the largest possible
// exclusion set. A reader with a long history pays that cost on every page
// load, invisibly.
func TestSeenSQLIsDeterministic(t *testing.T) {
	seen := map[string]bool{
		"ao3_work:3": true, "ao3_work:1": true, "ao3_work:2000": true,
		"ao3_work:17": true, "ao3_work:999": true, "ao3_work:2": true,
	}
	var first string
	for i := 0; i < 25; i++ {
		var args []any
		got := seenSQL(seen, &args)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("seenSQL differs between identical calls:\n  %s\n  %s",
				first, got)
		}
	}
}

// TestSeenSQLAppendsItsOwnArgs: a predicate that emits placeholders without
// appending the values shifts every later argument, and the query fails at
// runtime with a type error that points nowhere near the real defect.
func TestSeenSQLAppendsItsOwnArgs(t *testing.T) {
	seen := map[string]bool{"ao3_work:7": true, "ao3_work:8": true}

	var args []any
	sql := seenSQL(seen, &args)
	if len(args) != 2 {
		t.Fatalf("2 excluded ids but %d args appended: %v", len(args), args)
	}
	// One placeholder per excluded id, and they must be comma-joined with no
	// gap: `?, ?` would bind three values to two placeholders.
	if want := "NOT IN (?,?)"; !strings.Contains(sql, want) {
		t.Fatalf("predicate is not a NOT IN over %d placeholders: %q",
			len(args), sql)
	}
	if got := strings.Count(sql, "?"); got != len(args) {
		t.Fatalf("%d placeholders in %q but %d args appended: every ? must "+
			"consume exactly one arg", got, sql, len(args))
	}
	if args[0].(string) != "ao3_work:7" || args[1].(string) != "ao3_work:8" {
		t.Fatalf("args are not sorted alongside the placeholders: %v", args)
	}
}

// TestSeenSQLEmitsNothingWhenThereIsNothingToExclude: an unconditional
// `AND ... NOT IN ()` is a syntax error, and an empty predicate that still
// costs a scan is worse than no predicate.
func TestSeenSQLEmitsNothingWhenThereIsNothingToExclude(t *testing.T) {
	for name, seen := range map[string]map[string]bool{
		"nil":            nil,
		"empty":          {},
		"only empty ids": {"": true, "  ": true},
	} {
		var args []any
		if got := seenSQL(seen, &args); got != "" {
			t.Errorf("%s: seenSQL = %q, want empty", name, got)
		}
		if len(args) != 0 {
			t.Errorf("%s: %d args appended for an empty exclusion set", name, len(args))
		}
	}
}

// TestSeenIDsActuallyLeaveThePool is the behavioural test, and the one that
// matters: the predicate is worthless if the pool still contains the works.
//
// Asserting on the POOL rather than the results is deliberate. Recommend drops
// candidates that score zero, so a work present in the pool but unranked would
// hide in a result-list assertion while still being a real exclusion — and the
// reverse is the bug worth catching.
func TestSeenIDsActuallyLeaveThePool(t *testing.T) {
	e := collabCorpus(t)
	ctx := context.Background()

	seedEnts, err := e.Corpus.CandidateRows(ctx, []int64{10})
	if err != nil {
		t.Fatalf("load seeds: %v", err)
	}
	seeds, err := e.toCandidates(ctx, seedEnts)
	if err != nil {
		t.Fatalf("load seed tags: %v", err)
	}

	pool, _, err := e.poolFor(ctx, seeds, 20, true, []int64{10},
		Request{SeenIDs: map[string]bool{}})
	if err != nil {
		t.Fatalf("poolFor: %v", err)
	}
	if len(pool) < 3 {
		t.Fatalf("baseline pool is %v, too small for the test to mean anything",
			pool)
	}

	// Exclude two works the pool actually contains, so the assertion is about
	// exclusion rather than about ids that were never there.
	var victim int64
	var other int64
	for _, id := range pool {
		if victim == 0 {
			victim = id
			continue
		}
		if other == 0 {
			other = id
		}
	}
	excluded := map[string]bool{
		corpus.AO3Kind + ":" + itoa(victim): true,
		corpus.AO3Kind + ":" + itoa(other):  true,
	}

	after, _, err := e.poolFor(ctx, seeds, 20, true, []int64{10},
		Request{SeenIDs: excluded})
	if err != nil {
		t.Fatalf("poolFor with exclusions: %v", err)
	}
	for _, id := range after {
		if id == victim || id == other {
			t.Fatalf("work %d was excluded but is still in the pool %v",
				id, after)
		}
	}

	// And the pool was refilled rather than merely shrunk: a filter that
	// removes candidates without drawing replacements returns a shorter list,
	// so the reader silently gets fewer recommendations for having read some.
	if len(after) < len(pool) {
		t.Errorf("pool shrank from %d to %d on exclusion: the filter removes "+
			"candidates instead of replacing them, so reading works means "+
			"fewer recommendations", len(pool), len(after))
	}
}

// TestSeenExclusionIsNamespaced pins the "ao3_work:12" format against the
// wrong-but-plausible alternative of bare integers.
//
// With bare ids, a tag id 42 excluded because a work 42 was read — a
// collision that produces no error, just a reader wondering why a whole tag
// vanished.
func TestSeenExclusionIsNamespaced(t *testing.T) {
	e := collabCorpus(t)
	ctx := context.Background()

	seedEnts, err := e.Corpus.CandidateRows(ctx, []int64{10})
	if err != nil {
		t.Fatalf("load seeds: %v", err)
	}
	seeds, err := e.toCandidates(ctx, seedEnts)
	if err != nil {
		t.Fatalf("load seed tags: %v", err)
	}

	// A bare id is not a valid exclusion key and must exclude nothing.
	pool, _, err := e.poolFor(ctx, seeds, 20, true, []int64{10},
		Request{SeenIDs: map[string]bool{"12": true}})
	if err != nil {
		t.Fatalf("poolFor: %v", err)
	}
	for _, id := range pool {
		if id == 12 {
			t.Fatal(`a bare "12" excluded work 12: exclusion keys must be ` +
				`namespaced "ao3_work:12", or a tag id collides with a work id`)
		}
	}
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
