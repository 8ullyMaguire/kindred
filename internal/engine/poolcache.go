package engine

// The pool query is the engine's hottest path, and it is the same query over and
// over.
//
// Measured with a CPU profile on the live mirror (thinkcentre, 2026-10-23): one
// `GET /api/v1/recommend?seed=ao3_work:17249318` spent 87% of its 2.6 s inside
// `_sqlite3VdbeExec`, all of it in the single tag-pool statement — 1.44 s per
// call, against 0.47 s for the byte-identical SQL in the sqlite3 CLI. The gap is
// the pure-Go driver (modernc.org/sqlite): the same work, several times slower,
// spent on ~99k random row lookups into `works` to evaluate the standing
// filters.
//
// Two things make that affordable to cache rather than to rewrite:
//
//   - The statement is a pure function of its inputs. Nothing in it depends on
//     the clock (the date window is resolved by SQLite from one bound integer),
//     the reader, or the previous request.
//   - The inputs repeat. A tag page seeds from "the tag's five most-kudoed
//     works", so every reader of tag 29 issues the SAME pool query: same tag
//     ids, same standing filters, same budget. The same is true of the API's
//     common case (one seed, default filters).
//
// So: a small bounded cache in front of the tag-pool query, keyed on everything
// that can change its result and nothing that cannot.
//
// # What is deliberately NOT cached
//
// A request carrying `seen` ids (the reader's own already-read works) is not
// cached at all. Those sets are per-reader and unbounded, so including them in
// the key would fill the cache with entries nobody asks for twice, and omitting
// them would serve one reader's exclusions to another — a wrong answer that
// looks like a fast one.
//
// The collab and embedding halves of the pool are not cached here either: collab
// is an in-memory walk (microseconds), and the embedding index is already loaded
// once per process. This caches the SQL, which is what costs.

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// poolCacheKey is every input that can change the tag-pool statement's result.
//
// A struct rather than a formatted string so a field added later cannot be
// forgotten in the key without the struct comparison noticing: adding a filter
// to `Filter` and not to `key()` leaves the field zero, which is the value a
// request without that filter has — and the test that varies that filter fails.
type poolCacheKey struct {
	tags    string
	seeds   string
	blocked string
	budget  int
	exclude bool

	minWords             int64
	maxWords             int64
	minKudos             int64
	complete             TriBool
	ratings              string
	languages            string
	notUpdatedWithinDays int64
}

// PoolCache is a bounded, TTL'd cache of tag-pool results.
//
// It is safe for concurrent use: the engine serves requests from many
// goroutines, and a cache that raced would be a source of nondeterministic
// rankings rather than a performance feature.
type PoolCache struct {
	mu      sync.Mutex
	entries map[poolCacheKey]*poolCacheEntry
	// order is LRU order, oldest first. A slice rather than a linked list
	// because the bound is small (hundreds) and a linear scan of 256 keys is
	// cheaper than the pointer chasing it would replace.
	order []poolCacheKey
	max   int
	ttl   time.Duration
	// now is a seam for the expiry test; nil means time.Now.
	now func() time.Time

	hits, misses, evictions, bypassed, expired uint64
}

type poolCacheEntry struct {
	ids     []int64
	expires time.Time
}

// The deployment's cache size and lifetime.
//
// 512 entries is 512 pools of up to 1000 ids — a few megabytes, against a service
// capped at 512 MB — and comfortably more than the number of distinct tag pages a
// session walks through. Ten minutes is short enough that a rebuilt corpus is not
// served stale for long and long enough that a reader moving between pages keeps
// hitting; a deploy restarts the process and clears it outright.
const (
	DefaultPoolCacheEntries = 512
	DefaultPoolCacheTTL     = 10 * time.Minute
)

// NewPoolCache builds a cache holding up to max entries for ttl.
//
// A max below 1 disables the cache (every look-up misses), which is what a
// caller who wants the old behaviour should ask for rather than passing nil
// and getting a nil-pointer panic in the middle of a request.
func NewPoolCache(max int, ttl time.Duration) *PoolCache {
	if max < 1 {
		max = 1
	}
	return &PoolCache{
		entries: make(map[poolCacheKey]*poolCacheEntry, max),
		order:   make([]poolCacheKey, 0, max),
		max:     max,
		ttl:     ttl,
	}
}

func (c *PoolCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// PoolCacheStats is the cache's counters, for /healthz.
type PoolCacheStats struct {
	Entries   int    `json:"entries"`
	Hits      uint64 `json:"hits"`
	Misses    uint64 `json:"misses"`
	Evictions uint64 `json:"evictions"`
	Expired   uint64 `json:"expired"`
	// Bypassed counts requests that were not cacheable at all (a per-reader
	// `seen` set), so a hit rate can be read against the traffic it applies to
	// rather than against all traffic.
	Bypassed uint64 `json:"bypassed"`
}

// Stats reports the counters and the current size.
func (c *PoolCache) Stats() PoolCacheStats {
	if c == nil {
		return PoolCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return PoolCacheStats{
		Entries:   len(c.entries),
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
		Expired:   c.expired,
		Bypassed:  c.bypassed,
	}
}

// get returns the cached ids for a key, moving it to the back of the LRU order.
//
// The returned slice is a COPY. A caller that sorts, truncates or otherwise
// writes into what it got back would otherwise corrupt every later reader of the
// same entry — and the corruption would show up as a ranking that differs
// between two identical requests, which is exactly the property this project
// keeps testing for.
func (c *PoolCache) get(key poolCacheKey) ([]int64, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil, false
	}
	if c.clock().After(e.expires) {
		delete(c.entries, key)
		c.removeFromOrder(key)
		c.expired++
		c.misses++
		return nil, false
	}
	c.touch(key)
	c.hits++
	out := make([]int64, len(e.ids))
	copy(out, e.ids)
	return out, true
}

// put stores ids for a key, evicting the least recently used entry if full.
func (c *PoolCache) put(key poolCacheKey, ids []int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := make([]int64, len(ids))
	copy(stored, ids)
	if _, ok := c.entries[key]; ok {
		c.entries[key] = &poolCacheEntry{ids: stored, expires: c.clock().Add(c.ttl)}
		c.touch(key)
		return
	}
	for len(c.entries) >= c.max && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
		c.evictions++
	}
	c.entries[key] = &poolCacheEntry{ids: stored, expires: c.clock().Add(c.ttl)}
	c.order = append(c.order, key)
}

// bypass records a request the cache could not serve, so the hit rate can be
// read against cacheable traffic.
func (c *PoolCache) bypass() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.bypassed++
	c.mu.Unlock()
}

func (c *PoolCache) touch(key poolCacheKey) {
	for i, k := range c.order {
		if k == key {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), key)
			return
		}
	}
}

func (c *PoolCache) removeFromOrder(key poolCacheKey) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// cacheable says whether a request's inputs can be cached at all.
func cacheable(req Request) bool {
	// A per-reader `seen` set is unbounded and personal; a key containing it
	// would never be hit twice and would crowd out the entries that are.
	return len(req.SeenIDs) == 0
}

// keyFor builds the key. Tag ids arrive sorted (poolFor sorts them for exactly
// this reason: the argument list must not depend on Go's randomised map
// iteration), and the blocked ids are sorted here for the same cause.
func keyFor(tagList []int32, seedIDs []int64, blocked map[int64]bool, budget int, exclude bool, f Filter) poolCacheKey {
	var tb strings.Builder
	for i, id := range tagList {
		if i > 0 {
			tb.WriteByte(',')
		}
		tb.WriteString(strconv.Itoa(int(id)))
	}
	seeds := make([]int64, len(seedIDs))
	copy(seeds, seedIDs)
	sort.Slice(seeds, func(a, b int) bool { return seeds[a] < seeds[b] })
	var sb strings.Builder
	for i, id := range seeds {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	blockedIDs := make([]int64, 0, len(blocked))
	for id := range blocked {
		blockedIDs = append(blockedIDs, id)
	}
	sort.Slice(blockedIDs, func(a, b int) bool { return blockedIDs[a] < blockedIDs[b] })
	var bb strings.Builder
	for i, id := range blockedIDs {
		if i > 0 {
			bb.WriteByte(',')
		}
		bb.WriteString(strconv.FormatInt(id, 10))
	}
	ratings := make([]string, len(f.Ratings))
	copy(ratings, f.Ratings)
	sort.Strings(ratings)
	langs := make([]string, len(f.Languages))
	copy(langs, f.Languages)
	sort.Strings(langs)

	return poolCacheKey{
		tags:                 tb.String(),
		seeds:                sb.String(),
		blocked:              bb.String(),
		budget:               budget,
		exclude:              exclude,
		minWords:             f.MinWords,
		maxWords:             f.MaxWords,
		minKudos:             f.MinKudos,
		complete:             f.Complete,
		ratings:              strings.Join(ratings, "\x00"),
		languages:            strings.Join(langs, "\x00"),
		notUpdatedWithinDays: f.NotUpdatedWithinDays,
	}
}
