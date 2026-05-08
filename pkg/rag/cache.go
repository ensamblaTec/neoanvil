// Package rag — Cache[T any] generic LRU with generation invalidation.
// [PILAR-XXV/198]
//
// This is the canonical shape future cache consumers should adopt. Two
// concrete caches already ship — QueryCache (([]uint32)) and TextCache
// (string) — both predate generics in this package and are kept stable
// for the existing callers (handleSemanticCode / handleBlastRadius /
// handleProjectDigest / handleGraphWalk). A third consumer (typically
// an embedding cache keyed by query text → []float32) should build on
// Cache[T] rather than duplicate the LRU plumbing a third time.
//
// Feature parity with the battle-tested concrete caches:
//   - LRU + size cap (Resize/Capacity for runtime tuning)
//   - Generation-stamp invalidation (stale on Gen mismatch)
//   - Hit/miss/eviction counters + windowed hit ratio
//   - Per-entry hit counter + Top(n) observability
//   - Capacity=0 short-circuit (disabled mode)
//   - Zero-copy not guaranteed: callers receive the raw T on Get. If T
//     is a slice and mutation by the caller would be harmful, the
//     caller must copy. This differs from QueryCache.Get which always
//     copies — a deliberate tradeoff: generics can't assume slice vs
//     value semantics, so we leave copying to the caller.

package rag

import (
	"container/list"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// CacheKey is a hash + variant pair identical in spirit to
// QueryCacheKey / TextCacheKey. Discriminator is a free-form int so
// callers can fold topK, depth, mode, etc. into the key.
type CacheKey struct {
	Hash    uint64
	Variant int
}

// NewCacheKey derives a key from a natural-language or path identifier
// plus a variant. Uses the same FNV-1a digest as QueryCacheKey so keys
// computed via either constructor interleave cleanly if a future
// refactor merges the buckets.
func NewCacheKey(target string, variant int) CacheKey {
	q := NewQueryCacheKey(target, variant)
	return CacheKey{Hash: q.QueryHash, Variant: q.TopK}
}

type cacheItem[T any] struct {
	key      CacheKey
	value    T
	gen      uint64
	target   string
	hitCount uint64
}

// Cache is a generic LRU. Zero-value is not usable — construct via
// NewCache. T is typically a slice or struct; callers must not mutate
// returned values without copying first.
type Cache[T any] struct {
	mu       sync.Mutex
	capacity int
	items    map[CacheKey]*list.Element
	ll       *list.List
	hits     atomic.Uint64
	misses   atomic.Uint64
	evicts   atomic.Uint64
	window   *cacheWindow
	missRing *missRing
}

// NewCache constructs a generic LRU of the given capacity. Capacity ≤ 0
// disables the cache — Get always misses, Put is a no-op.
func NewCache[T any](capacity int) *Cache[T] {
	return &Cache[T]{
		capacity: capacity,
		items:    make(map[CacheKey]*list.Element, capacity),
		ll:       list.New(),
		window:   newCacheWindow(),
		missRing: newMissRing(),
	}
}

// Get returns the value and a hit flag. Generation mismatch evicts
// lazily and reports a miss.
func (c *Cache[T]) Get(key CacheKey, currentGen uint64) (T, bool) {
	var zero T
	if c == nil || c.capacity <= 0 {
		return zero, false
	}
	c.mu.Lock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		c.mu.Unlock()
		c.window.record(false)
		return zero, false
	}
	item := el.Value.(*cacheItem[T])
	if item.gen != currentGen {
		c.ll.Remove(el)
		delete(c.items, key)
		c.misses.Add(1)
		c.mu.Unlock()
		c.window.record(false)
		return zero, false
	}
	c.ll.MoveToFront(el)
	item.hitCount++
	c.hits.Add(1)
	value := item.value
	c.mu.Unlock()
	c.window.record(true)
	return value, true
}

// Peek returns the cached value without bumping any counter. Debug
// tooling must not distort the observability metrics. [Épica 227]
func (c *Cache[T]) Peek(key CacheKey, currentGen uint64) (T, bool) {
	var zero T
	if c == nil || c.capacity <= 0 {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	item := el.Value.(*cacheItem[T])
	if item.gen != currentGen {
		return zero, false
	}
	return item.value, true
}

// Put inserts or updates the entry. Kept for legacy parity; new code
// should call PutAnnotated to populate target for Top() observability.
func (c *Cache[T]) Put(key CacheKey, value T, currentGen uint64) {
	c.PutAnnotated(key, value, currentGen, "")
}

// PutAnnotated stores value with the original target string attached.
// When the cap would be exceeded, the LRU tail evicts.
func (c *Cache[T]) PutAnnotated(key CacheKey, value T, currentGen uint64, target string) {
	if c == nil || c.capacity <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		item := el.Value.(*cacheItem[T])
		item.value = value
		item.gen = currentGen
		if target != "" {
			item.target = target
		}
		c.ll.MoveToFront(el)
		return
	}
	item := &cacheItem[T]{key: key, value: value, gen: currentGen, target: target}
	el := c.ll.PushFront(item)
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*cacheItem[T]).key)
			c.evicts.Add(1)
		}
	}
}

// RecordMiss annotates a miss with the original target for RecentMissTargets.
func (c *Cache[T]) RecordMiss(target string) {
	if c == nil {
		return
	}
	c.missRing.record(target)
}

// RecentMissTargets returns up to n unique recent miss targets.
func (c *Cache[T]) RecentMissTargets(n int) []string {
	if c == nil {
		return nil
	}
	return c.missRing.recent(n)
}

// Stats returns the lifetime counters.
func (c *Cache[T]) Stats() (hits, misses, evicts uint64, size int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	size = c.ll.Len()
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), c.evicts.Load(), size
}

// HitRatio returns hits / (hits + misses) or 0 before the first request.
func (c *Cache[T]) HitRatio() float64 {
	h, m, _, _ := c.Stats()
	total := h + m
	if total == 0 {
		return 0
	}
	return float64(h) / float64(total)
}

// WindowedHitRatio returns (hits, misses, ratio) over the last d.
func (c *Cache[T]) WindowedHitRatio(d time.Duration) (hits, misses uint64, ratio float64) {
	if c == nil {
		return 0, 0, 0
	}
	return c.window.windowed(d)
}

// Resize changes the LRU capacity. Shrink evicts the LRU tail; grow is O(1).
func (c *Cache[T]) Resize(newCap int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capacity = newCap
	if newCap <= 0 {
		return
	}
	for c.ll.Len() > newCap {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheItem[T]).key)
		c.evicts.Add(1)
	}
}

// Capacity returns the current configured capacity.
func (c *Cache[T]) Capacity() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity
}

// CacheTopEntry describes one hot entry for Top().
type CacheTopEntry struct {
	Target   string
	Variant  int
	HitCount uint64
}

// TopWithValue is Top(n) extended with the actual cached value. Used by
// SaveSnapshotJSON so generic-cache persistence can stream the full
// entry without a second locked scan. [Épica 210]
type TopWithValue[T any] struct {
	Target   string
	Variant  int
	HitCount uint64
	Value    T
}

// TopValues mirrors Top(n) but also emits the cached T. Keeps the lock
// critical section as small as Top()'s by copying into a local slice
// before sorting.
func (c *Cache[T]) TopValues(n int) []TopWithValue[T] {
	if c == nil || n <= 0 {
		return nil
	}
	c.mu.Lock()
	out := make([]TopWithValue[T], 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		item := el.Value.(*cacheItem[T])
		target := item.target
		if target == "" {
			target = "(opaque)"
		}
		out = append(out, TopWithValue[T]{
			Target: target, Variant: item.key.Variant,
			HitCount: item.hitCount, Value: item.value,
		})
	}
	c.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].HitCount > out[j].HitCount })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Top returns up to n entries sorted by hit count descending. Stable
// sort preserves LRU order for ties.
func (c *Cache[T]) Top(n int) []CacheTopEntry {
	if c == nil || n <= 0 {
		return nil
	}
	c.mu.Lock()
	out := make([]CacheTopEntry, 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		item := el.Value.(*cacheItem[T])
		target := item.target
		if target == "" {
			target = "(opaque)"
		}
		out = append(out, CacheTopEntry{Target: target, Variant: item.key.Variant, HitCount: item.hitCount})
	}
	c.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].HitCount > out[j].HitCount })
	if len(out) > n {
		out = out[:n]
	}
	return out
}
