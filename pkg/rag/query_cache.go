// Package rag — LRU cache for semantic search results. [PILAR-XXV/174]
//
// Repeated queries are the rule, not the exception, in an SRE workflow:
// BLAST_RADIUS on the same file during a debugging loop, SEMANTIC_CODE
// for the same concept across consecutive tool calls, incident search
// that re-hits the same keyword after an on-call rotates. Cache-hit
// turnaround is ~1 µs (map lookup) vs 6-60 µs for the full HNSW walk
// depending on corpus size — a 6-60× speedup on the repeat path.
//
// Design:
//   - Key: 64-bit FNV hash of (query_target + topK). Cheap, deterministic,
//     collision-resistant enough for tens of thousands of concurrent queries.
//   - Value: copy of the result slice ([]uint32) tagged with the graph
//     generation at the time of insertion.
//   - Invalidation: `Graph.Gen` counter bumps on every successful
//     InsertBatch. Cache entries keep their own generation; a lookup whose
//     generation no longer matches is treated as a miss. This makes cache
//     correctness automatic — no manual invalidation calls required.
//   - Eviction: classic LRU via a doubly linked list + map. O(1) Get, O(1)
//     Put, no background goroutine.
//
// The cache is safe for concurrent use under a single RWMutex. Writers
// promote to write lock only when the entry needs to be added or moved;
// concurrent readers on a hot entry are unblocked after the LRU move
// completes.

package rag

import (
	"container/list"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

// QueryCacheKey identifies a cache entry. Separate type so callers do not
// stringify/hash by hand.
type QueryCacheKey struct {
	QueryHash uint64
	TopK      int
}

// NewQueryCacheKey derives a key from the natural-language query and the
// requested top-K. Different target strings or topK values produce distinct
// keys — callers that use Options must fold them into the query string
// themselves to get disjoint cache entries.
func NewQueryCacheKey(target string, topK int) QueryCacheKey {
	h := fnv.New64a()
	_, _ = h.Write([]byte(target))
	return QueryCacheKey{QueryHash: h.Sum64(), TopK: topK}
}

type cacheEntry struct {
	key      QueryCacheKey
	result   []uint32
	gen      uint64
	target   string // [192] original query for Top() observability
	hitCount uint64 // [192] per-entry hit counter
}

// QueryCache is a fixed-capacity LRU cache with graph-generation-based
// invalidation. Zero-value is not usable — construct via NewQueryCache.
type QueryCache struct {
	mu       sync.Mutex
	capacity int
	items    map[QueryCacheKey]*list.Element // map → LRU element holding *cacheEntry
	ll       *list.List                      // front = most recently used
	hits     atomic.Uint64
	misses   atomic.Uint64
	evicts   atomic.Uint64
	window   *cacheWindow // [195] lightweight ring for WindowedHitRatio
	misses_  *missRing    // [196] ring of recent miss targets for warmup suggestions
}

// NewQueryCache returns a cache that holds up to capacity entries. A non-
// positive capacity disables caching (every Get misses, every Put is a
// no-op) — handy for quick A/B comparisons without pulling the type out.
func NewQueryCache(capacity int) *QueryCache {
	return &QueryCache{
		capacity: capacity,
		items:    make(map[QueryCacheKey]*list.Element, capacity),
		ll:       list.New(),
		window:   newCacheWindow(),
		misses_:  newMissRing(),
	}
}

// RecordMiss is exposed so callers with the original target string can
// annotate a miss (Get alone lacks the target — the QueryCacheKey is a
// hash). handleSemanticCode calls this right before Put on a miss path.
// [Épica 196]
func (c *QueryCache) RecordMiss(target string) {
	if c == nil {
		return
	}
	c.misses_.record(target)
}

// RecentMissTargets returns up to n unique recent miss targets. [Épica 196]
func (c *QueryCache) RecentMissTargets(n int) []string {
	if c == nil {
		return nil
	}
	return c.misses_.recent(n)
}

// Get returns the cached result and a hit flag. A false flag covers both
// "not in cache" and "present but invalidated by graph generation bump".
func (c *QueryCache) Get(key QueryCacheKey, currentGen uint64) ([]uint32, bool) {
	if c == nil || c.capacity <= 0 {
		return nil, false
	}
	c.mu.Lock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		c.mu.Unlock()
		c.window.record(false)
		return nil, false
	}
	entry := el.Value.(*cacheEntry)
	if entry.gen != currentGen {
		// Stale — evict lazily on the first miss after a generation bump.
		c.ll.Remove(el)
		delete(c.items, key)
		c.misses.Add(1)
		c.mu.Unlock()
		c.window.record(false)
		return nil, false
	}
	c.ll.MoveToFront(el)
	entry.hitCount++
	c.hits.Add(1)
	// Copy so callers cannot mutate the cached slice.
	out := make([]uint32, len(entry.result))
	copy(out, entry.result)
	c.mu.Unlock()
	c.window.record(true)
	return out, true
}

// Put stores the result under key. The caller supplies the generation
// stamp — usually Graph.Gen.Load() at the time of the search that
// produced this result. Legacy signature — PutAnnotated is preferred.
func (c *QueryCache) Put(key QueryCacheKey, result []uint32, currentGen uint64) {
	c.PutAnnotated(key, result, currentGen, "")
}

// PutAnnotated is the full-fidelity store — records the original target
// string so Top(n) can show human-readable cache contents. [Épica 192]
func (c *QueryCache) PutAnnotated(key QueryCacheKey, result []uint32, currentGen uint64, target string) {
	if c == nil || c.capacity <= 0 {
		return
	}
	snapshot := make([]uint32, len(result))
	copy(snapshot, result)

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*cacheEntry)
		entry.result = snapshot
		entry.gen = currentGen
		if target != "" {
			entry.target = target
		}
		c.ll.MoveToFront(el)
		return
	}
	entry := &cacheEntry{key: key, result: snapshot, gen: currentGen, target: target}
	el := c.ll.PushFront(entry)
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*cacheEntry).key)
			c.evicts.Add(1)
		}
	}
}

// QueryTopEntry summarises one hot QueryCache entry for Top().
type QueryTopEntry struct {
	Target     string
	TopK       int
	ResultSize int
	HitCount   uint64
}

// Top returns up to n entries sorted by hit count descending. Entries
// without an original target (legacy Put or tests) fall back to
// "(opaque)". Stable insertion-sort — ties preserve LRU order.
func (c *QueryCache) Top(n int) []QueryTopEntry {
	if c == nil || n <= 0 {
		return nil
	}
	c.mu.Lock()
	out := make([]QueryTopEntry, 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		e := el.Value.(*cacheEntry)
		target := e.target
		if target == "" {
			target = "(opaque)"
		}
		out = append(out, QueryTopEntry{
			Target: target, TopK: e.key.TopK,
			ResultSize: len(e.result), HitCount: e.hitCount,
		})
	}
	c.mu.Unlock()
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].HitCount < out[j].HitCount; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Stats reports operational counters for BRIEFING / HUD surfaces.
func (c *QueryCache) Stats() (hits, misses, evicts uint64, size int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	size = c.ll.Len()
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), c.evicts.Load(), size
}

// HitRatio returns hits / (hits + misses), or 0 before the first request.
// Useful one-number summary for BRIEFING compact lines.
func (c *QueryCache) HitRatio() float64 {
	h, m, _, _ := c.Stats()
	total := h + m
	if total == 0 {
		return 0
	}
	return float64(h) / float64(total)
}

// Resize changes the LRU capacity. Growing is O(1). Shrinking evicts
// from the back (least-recently-used) until size fits the new cap.
// Passing ≤ 0 is treated as "disable" — the cache will miss on every
// Get and Put becomes a no-op. [Épica 191]
func (c *QueryCache) Resize(newCap int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capacity = newCap
	if newCap <= 0 {
		// Don't free the underlying map — the capacity check in Get/Put
		// already short-circuits further access, and a nil map would
		// require re-initialisation on the next Resize grow.
		return
	}
	for c.ll.Len() > newCap {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
		c.evicts.Add(1)
	}
}

// Peek returns the value without incrementing any counter — neither the
// atomic hit/miss pair nor the per-entry hit counter, nor the window
// ring. Callers that only want to observe cache state (e.g. a debug
// inspection tool) MUST use this instead of Get so the stats stay
// accurate. [Épica 227]
//
// Returns (nil, false) for missing or generation-stale entries.
// Does NOT promote the entry in LRU order — Peek is a pure read.
func (c *QueryCache) Peek(key QueryCacheKey, currentGen uint64) ([]uint32, bool) {
	if c == nil || c.capacity <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*cacheEntry)
	if entry.gen != currentGen {
		return nil, false
	}
	out := make([]uint32, len(entry.result))
	copy(out, entry.result)
	return out, true
}

// WindowedHitRatio returns (hits, misses, ratio) over the last `d` of
// activity. Gives an actionable "how is the cache performing RIGHT NOW"
// view separate from the lifetime ratio. [Épica 195]
func (c *QueryCache) WindowedHitRatio(d time.Duration) (hits, misses uint64, ratio float64) {
	if c == nil {
		return 0, 0, 0
	}
	return c.window.windowed(d)
}

// Capacity returns the current configured capacity. Mirrors the lock so
// concurrent Resize calls don't read a torn value.
func (c *QueryCache) Capacity() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity
}
