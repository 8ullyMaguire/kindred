package engine

// #79-adjacent, but this file's subject is the pool cache: the hot query's
// memoisation must be invisible.
//
// A cache in front of a ranking is only acceptable if a hit is indistinguishable
// from a miss. These tests pin that, and they pin the ways it could stop being
// true: a key that ignores an input, a stored slice handed out by reference, an
// entry that outlives its TTL, a per-reader set that sneaks into the key space.

import (
	"testing"
	"time"
)

func testFilter() Filter {
	return Filter{MinWords: 20000, Languages: []string{"English"}, NotUpdatedWithinDays: 122}
}

func TestTheSameRequestGetsTheSameIdsFromTheCache(t *testing.T) {
	c := NewPoolCache(4, time.Minute)
	tags := []int32{15, 29, 240}
	seeds := []int64{17249318}
	blocked := map[int64]bool{563: true}
	key := keyFor(tags, seeds, blocked, 1000, true, testFilter())

	if _, ok := c.get(key); ok {
		t.Fatal("an empty cache answered a hit")
	}
	c.put(key, []int64{7, 8, 9})

	got, ok := c.get(key)
	if !ok {
		t.Fatal("the entry just stored was not found")
	}
	if len(got) != 3 || got[0] != 7 || got[1] != 8 || got[2] != 9 {
		t.Fatalf("cached ids = %v, want [7 8 9]", got)
	}
	if st := c.Stats(); st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("stats = %+v, want 1 hit and 1 miss", st)
	}
}

func TestAChangedFilterIsADifferentEntry(t *testing.T) {
	c := NewPoolCache(4, time.Minute)
	tags := []int32{15}
	seeds := []int64{1}
	c.put(keyFor(tags, seeds, nil, 1000, true, Filter{MinWords: 20000}), []int64{7})

	// Every field a filter can carry, varied one at a time: a key that drops
	// any of them serves a pool built under different filters, which is a
	// wrong answer that looks like a fast one.
	variants := map[string]Filter{
		"min words":     {MinWords: 30000},
		"max words":     {MaxWords: 50000},
		"min kudos":     {MinKudos: 10},
		"complete only": {Complete: CompleteOnly},
		"complete wip":  {Complete: CompleteWIP},
		"rating":        {Ratings: []string{"Mature"}},
		"language":      {Languages: []string{"Spanish"}},
		"last updated":  {NotUpdatedWithinDays: 30},
	}
	for name, f := range variants {
		if _, ok := c.get(keyFor(tags, seeds, nil, 1000, true, f)); ok {
			t.Errorf("%s: a filter change hit the entry built without it", name)
		}
	}
}

func TestAChangedBlockSetOrBudgetIsADifferentEntry(t *testing.T) {
	c := NewPoolCache(4, time.Minute)
	tags := []int32{15}
	seeds := []int64{1}
	f := testFilter()
	c.put(keyFor(tags, seeds, map[int64]bool{563: true}, 1000, true, f), []int64{7})

	if _, ok := c.get(keyFor(tags, seeds, map[int64]bool{563: true, 564: true}, 1000, true, f)); ok {
		t.Error("adding a blocked tag hit the entry without it")
	}
	if _, ok := c.get(keyFor(tags, seeds, map[int64]bool{563: true}, 500, true, f)); ok {
		t.Error("a different budget hit the entry for 1000")
	}
	if _, ok := c.get(keyFor(tags, seeds, map[int64]bool{563: true}, 1000, false, f)); ok {
		t.Error("exclude=false hit the entry built with exclude=true")
	}
	if _, ok := c.get(keyFor([]int32{15, 16}, seeds, map[int64]bool{563: true}, 1000, true, f)); ok {
		t.Error("a different tag list hit the entry")
	}
	if _, ok := c.get(keyFor(tags, []int64{2}, map[int64]bool{563: true}, 1000, true, f)); ok {
		t.Error("a different seed hit the entry")
	}
}

// The blocked set is a MAP, and Go randomises map iteration. A key built by
// walking the map would produce a different key per request for the same set of
// tags, so the cache would never hit — the failure mode that looks like "the
// cache does not help" rather than "the key is wrong".
func TestTheBlockSetOrderDoesNotChangeTheKey(t *testing.T) {
	tags := []int32{15}
	seeds := []int64{1}
	f := testFilter()
	a := keyFor(tags, seeds, map[int64]bool{900: true, 3: true, 77: true}, 1000, true, f)
	for i := 0; i < 20; i++ {
		b := keyFor(tags, seeds, map[int64]bool{77: true, 900: true, 3: true}, 1000, true, f)
		if a != b {
			t.Fatalf("the same block set produced two keys:\n%+v\n%+v", a, b)
		}
	}
}

// A caller that sorts or truncates what it got back must not corrupt the entry.
func TestTheCachedSliceIsNotAliased(t *testing.T) {
	c := NewPoolCache(4, time.Minute)
	key := keyFor([]int32{15}, []int64{1}, nil, 3, true, testFilter())
	c.put(key, []int64{7, 8, 9})

	first, _ := c.get(key)
	first[0] = 999
	first = append(first, 12345)
	_ = first

	second, ok := c.get(key)
	if !ok {
		t.Fatal("the entry vanished")
	}
	if second[0] != 7 || len(second) != 3 {
		t.Fatalf("the stored entry was mutated through a caller's slice: %v", second)
	}

	// And the other direction: the caller's slice at put time.
	src := []int64{1, 2, 3}
	c.put(key, src)
	src[0] = 42
	third, _ := c.get(key)
	if third[0] != 1 {
		t.Fatalf("mutating the caller's slice after put changed the entry: %v", third)
	}
}

func TestAnExpiredEntryIsNotServed(t *testing.T) {
	c := NewPoolCache(4, 50*time.Millisecond)
	now := time.Now()
	c.now = func() time.Time { return now }
	key := keyFor([]int32{15}, []int64{1}, nil, 10, true, testFilter())
	c.put(key, []int64{7})

	if _, ok := c.get(key); !ok {
		t.Fatal("a fresh entry was not served")
	}
	now = now.Add(time.Second)
	if _, ok := c.get(key); ok {
		t.Fatal("an entry past its TTL was served")
	}
	if st := c.Stats(); st.Expired != 1 || st.Entries != 0 {
		t.Fatalf("stats after expiry = %+v, want one expired and no entries", st)
	}
	// The expired key must be gone, not merely skipped: a stale entry left in
	// the map would be evicted last and would count against the bound.
	if _, ok := c.get(key); ok {
		t.Fatal("the expired entry came back")
	}
}

func TestTheCacheIsBoundedAndEvictsTheOldest(t *testing.T) {
	c := NewPoolCache(2, time.Minute)
	f := testFilter()
	k1 := keyFor([]int32{1}, []int64{1}, nil, 10, true, f)
	k2 := keyFor([]int32{2}, []int64{1}, nil, 10, true, f)
	k3 := keyFor([]int32{3}, []int64{1}, nil, 10, true, f)
	c.put(k1, []int64{1})
	c.put(k2, []int64{2})
	// Touch k1 so k2 becomes the least recently used.
	if _, ok := c.get(k1); !ok {
		t.Fatal("k1 should still be present")
	}
	c.put(k3, []int64{3})

	if _, ok := c.get(k2); ok {
		t.Fatal("the least recently used entry survived eviction")
	}
	if _, ok := c.get(k1); !ok {
		t.Fatal("a recently used entry was evicted instead")
	}
	if st := c.Stats(); st.Evictions != 1 || st.Entries != 2 {
		t.Fatalf("stats = %+v, want 1 eviction and 2 entries", st)
	}
}

// A request carrying the reader's own read set must not enter the cache at all:
// those sets are personal and unbounded, and a key built from them would never
// be hit twice while crowding out the entries that are.
func TestAPerReaderSeenSetIsNotCacheable(t *testing.T) {
	if cacheable(Request{SeenIDs: map[string]bool{"ao3_work:1": true}}) {
		t.Fatal("a request with seen ids was called cacheable")
	}
	if !cacheable(Request{}) {
		t.Fatal("a request with no seen ids was called uncacheable")
	}

	c := NewPoolCache(4, time.Minute)
	c.bypass()
	c.bypass()
	st := c.Stats()
	if st.Bypassed != 2 {
		t.Fatalf("bypassed = %d, want 2", st.Bypassed)
	}
	if st.Entries != 0 {
		t.Fatalf("a bypassed request added %d entries", st.Entries)
	}
}

// A nil cache is the engine's test configuration; every method must be safe on
// it rather than panicking mid-request.
func TestANilCacheIsInert(t *testing.T) {
	var c *PoolCache
	if _, ok := c.get(poolCacheKey{}); ok {
		t.Fatal("a nil cache answered a hit")
	}
	c.put(poolCacheKey{}, []int64{1})
	c.bypass()
	if st := c.Stats(); st.Entries != 0 || st.Hits != 0 {
		t.Fatalf("a nil cache reported %+v", st)
	}
}
