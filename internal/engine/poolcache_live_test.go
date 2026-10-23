package engine

// The pool cache, exercised against a real corpus rather than a fake map.
//
// The unit tests above pin the key and the bookkeeping. What they cannot show is
// the property that matters: a request answered FROM the cache returns the same
// candidate ids as the request that filled it. That is what makes the speed
// legitimate, and it needs `poolFor` and the actual SQL.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func poolEngine(t *testing.T, cache *PoolCache) *Engine {
	t.Helper()
	dir := t.TempDir()
	path, err := testcorpus.New(90).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Engine{Corpus: corpus.NewAO3(db), PoolSize: 40, Pools: cache}
}

func callPool(t *testing.T, e *Engine, seed int64, f Filter) []int64 {
	t.Helper()
	ctx := context.Background()
	// One seed work, with its tags loaded the way Recommend loads them.
	ents, err := e.Corpus.CandidateRows(ctx, []int64{seed})
	if err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	if len(ents) == 0 {
		t.Fatalf("fixture has no work %d", seed)
	}
	seeds, err := e.toCandidates(ctx, ents)
	if err != nil {
		t.Fatalf("seed tags: %v", err)
	}
	req := Request{Filter: f}
	ids, _, _, err := e.poolFor(ctx, seeds, lanesOf(e), true, []int64{seed}, req)
	if err != nil {
		t.Fatalf("poolFor: %v", err)
	}
	return ids
}

func lanesOf(e *Engine) int {
	if e.PoolSize > 0 {
		return e.PoolSize
	}
	return DefaultPoolSize
}

func TestACachedPoolIsTheSamePool(t *testing.T) {
	c := NewPoolCache(8, time.Minute)
	e := poolEngine(t, c)

	first := callPool(t, e, 1, Filter{})
	second := callPool(t, e, 1, Filter{})

	if len(first) == 0 {
		t.Fatal("the pool is empty, so nothing below is being compared")
	}
	if len(first) != len(second) {
		t.Fatalf("cached pool has %d ids, fresh pool %d", len(second), len(first))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("position %d: cached %d, fresh %d", i, second[i], first[i])
		}
	}
	st := c.Stats()
	if st.Misses != 1 || st.Hits != 1 {
		t.Fatalf("stats = %+v, want one miss then one hit", st)
	}
}

// The same seed under different filters is a different pool, and the cache must
// not conflate them — the failure would be a filtered page showing works the
// filter excludes.
func TestAFilteredPoolDoesNotComeFromTheUnfilteredEntry(t *testing.T) {
	c := NewPoolCache(8, time.Minute)
	e := poolEngine(t, c)

	all := callPool(t, e, 1, Filter{})
	narrow := callPool(t, e, 1, Filter{MinWords: 200000})
	again := callPool(t, e, 1, Filter{})

	if len(narrow) > len(all) {
		t.Fatalf("a min-words filter RETURNED MORE works (%d) than no filter (%d)", len(narrow), len(all))
	}
	if len(again) != len(all) {
		t.Fatalf("re-asking the unfiltered pool returned %d ids, the first asked for %d", len(again), len(all))
	}
	for i := range all {
		if again[i] != all[i] {
			t.Fatalf("the unfiltered pool changed after a filtered request: position %d %d -> %d", i, all[i], again[i])
		}
	}
}

// A store is not needed to call poolFor, but the engine's default must remain
// "no cache": a ranking tested for determinism must not be able to pass because
// a cache answered it.
func TestAnEngineWithoutACacheStillAnswers(t *testing.T) {
	e := poolEngine(t, nil)
	got := callPool(t, e, 1, Filter{})
	if len(got) == 0 {
		t.Fatal("an uncached engine returned an empty pool")
	}
	if e.Pools != nil {
		t.Fatal("the engine grew a cache it was not given")
	}
}

var _ = store.ErrNotFound
