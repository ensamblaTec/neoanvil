// Package rag — LRU cache for full-text tool responses. [PILAR-XXV/179]
//
// Companion to QueryCache. Where QueryCache stores []uint32 node IDs
// (cheap to copy, re-formatting needed), TextCache stores the final
// markdown output of handlers whose post-processing is itself expensive
// (CPG PageRank for BLAST_RADIUS, CodeRank for PROJECT_DIGEST, etc.).
//
// Why two types instead of one generic:
//
//   - QueryCache was committed first, already wired into SEMANTIC_CODE,
//     and locking it into a generic parameter now would ripple through
//     every existing caller for no functional gain.
//   - Duplicating ~80 lines buys zero-risk landing for 179 and keeps the
//     two hot paths independently tunable. If a third consumer ever
//     appears we consolidate to Cache[T any] in a dedicated refactor.
//
// Design identical to QueryCache: FNV64-hash key, LRU via list.List +
// map, generation-stamp invalidation, copy-on-write safety, stats for
// BRIEFING surface.

package rag

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

// TextCacheKey identifies an entry by hash of "tool|target" and an
// arbitrary int discriminator (typically limit/topK/mode). The
// discriminator keeps disjoint variants from aliasing into the same slot.
type TextCacheKey struct {
	Hash  uint64
	Variant int
}

// NewTextCacheKey hashes the (handler, target) pair via the same FNV64
// used by QueryCache so both cache lines keep the same discrimination
// power. Variant is folded in at lookup time.
func NewTextCacheKey(handler, target string, variant int) TextCacheKey {
	// Reuse QueryCacheKey's hashing so invariants stay in one place.
	qk := NewQueryCacheKey(handler+"|"+target, variant)
	return TextCacheKey{Hash: qk.QueryHash, Variant: qk.TopK}
}

type textEntry struct {
	key      TextCacheKey
	text     string
	gen      uint64
	target   string // [190] original query string for observability
	handler  string // [190] which handler cached this entry
	variant  int    // [190] cached variant (topK/limit/depth)
	hitCount uint64 // [190] how many times this entry was served
	// mtime is the os.Stat.ModTime() of the target file when this entry was
	// computed, or 0 if the caller didn't supply one (legacy path). When
	// non-zero, GetWithMtimeFallback treats a matching mtime as a hit even
	// if the graph generation has bumped — file-scoped invalidation that
	// survives the global gen-bump on InsertBatch. [Phase 1 MV / Speed-First]
	mtime int64
}

// TextCache holds the last N full-markdown responses. Safe for concurrent
// use under a single mutex. Capacity ≤ 0 disables the cache.
type TextCache struct {
	mu       sync.Mutex
	capacity int
	items    map[TextCacheKey]*list.Element
	ll       *list.List
	hits     atomic.Uint64
	misses   atomic.Uint64
	evicts   atomic.Uint64
	window   *cacheWindow // [195] windowed hit-ratio tracker
	misses_  *missRing    // [196] ring of recent miss targets
}

// NewTextCache constructs a cache with the given capacity. Passing 0 or
// less disables caching — Get always misses, Put is a no-op.
func NewTextCache(capacity int) *TextCache {
	return &TextCache{
		capacity: capacity,
		items:    make(map[TextCacheKey]*list.Element, capacity),
		ll:       list.New(),
		window:   newCacheWindow(),
		misses_:  newMissRing(),
	}
}

// RecordMiss annotates a miss with the original target for the recent-
// miss ring (so Top-miss can list them for warmup). [Épica 196]
func (c *TextCache) RecordMiss(target string) {
	if c == nil {
		return
	}
	c.misses_.record(target)
}

// RecentMissTargets returns up to n unique recent miss targets. [Épica 196]
func (c *TextCache) RecentMissTargets(n int) []string {
	if c == nil {
		return nil
	}
	return c.misses_.recent(n)
}

// Get returns the cached text and a hit flag. Generation mismatch is
// treated as a miss and evicts the stale entry lazily. Bumps the per-
// entry hit counter so Top() can rank entries by popularity.
func (c *TextCache) Get(key TextCacheKey, currentGen uint64) (string, bool) {
	if c == nil || c.capacity <= 0 {
		return "", false
	}
	c.mu.Lock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		c.mu.Unlock()
		c.window.record(false)
		return "", false
	}
	entry := el.Value.(*textEntry)
	if entry.gen != currentGen {
		c.ll.Remove(el)
		delete(c.items, key)
		c.misses.Add(1)
		c.mu.Unlock()
		c.window.record(false)
		return "", false
	}
	c.ll.MoveToFront(el)
	entry.hitCount++
	c.hits.Add(1)
	text := entry.text
	c.mu.Unlock()
	c.window.record(true)
	return text, true
}

// Put stores text under key. The caller supplies the generation stamp
// (usually Graph.Gen.Load()) captured at the time of the computation
// that produced this text.
func (c *TextCache) Put(key TextCacheKey, text string, currentGen uint64) {
	c.PutAnnotated(key, text, currentGen, "", "", 0)
}

// PutAnnotated is the full-fidelity store — same semantics as Put but
// records the handler+target+variant so Top() can surface human-readable
// cache contents. Callers that want observability should use this form.
func (c *TextCache) PutAnnotated(key TextCacheKey, text string, currentGen uint64, handler, target string, variant int) {
	c.PutWithMtime(key, text, currentGen, 0, handler, target, variant)
}

// PutWithMtime stores `text` with both the graph generation stamp AND the
// target file's mtime nanoseconds. Either stamp alone is enough for
// GetWithMtimeFallback to score a hit — gen matches the live graph (the
// historical invariant) OR mtime matches the live file (the file-scoped
// invariant). Pass mtime=0 to opt out of file-scoped validity (then this
// behaves exactly like PutAnnotated). [Phase 1 MV / Speed-First]
func (c *TextCache) PutWithMtime(key TextCacheKey, text string, currentGen uint64, mtime int64, handler, target string, variant int) {
	if c == nil || c.capacity <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*textEntry)
		entry.text = text
		entry.gen = currentGen
		entry.mtime = mtime
		// Preserve handler/target if this caller didn't supply them; otherwise update.
		if handler != "" {
			entry.handler = handler
		}
		if target != "" {
			entry.target = target
		}
		if variant != 0 {
			entry.variant = variant
		}
		c.ll.MoveToFront(el)
		return
	}
	entry := &textEntry{
		key: key, text: text, gen: currentGen, mtime: mtime,
		handler: handler, target: target, variant: variant,
	}
	el := c.ll.PushFront(entry)
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*textEntry).key)
			c.evicts.Add(1)
		}
	}
}

// GetWithMtimeFallback returns a cached entry if EITHER the graph generation
// still matches OR the entry was stamped with a file mtime and the supplied
// currentMtime matches that stamp. The dual gate decouples cache validity
// from the global graph.Gen counter for file-targeted entries — editing
// pkg/foo/x.go bumps Gen and invalidates SEMANTIC_CODE-style cross-cutting
// entries, but file-targeted BLAST_RADIUS entries for pkg/bar/y.go survive
// as long as pkg/bar/y.go's mtime is unchanged. [Phase 1 MV / Speed-First]
func (c *TextCache) GetWithMtimeFallback(key TextCacheKey, currentGen uint64, currentMtime int64) (string, bool) {
	if c == nil || c.capacity <= 0 {
		return "", false
	}
	c.mu.Lock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		c.mu.Unlock()
		c.window.record(false)
		return "", false
	}
	entry := el.Value.(*textEntry)
	genMatch := entry.gen == currentGen
	mtimeMatch := entry.mtime != 0 && currentMtime != 0 && entry.mtime == currentMtime
	if !genMatch && !mtimeMatch {
		c.ll.Remove(el)
		delete(c.items, key)
		c.misses.Add(1)
		c.mu.Unlock()
		c.window.record(false)
		return "", false
	}
	c.ll.MoveToFront(el)
	entry.hitCount++
	c.hits.Add(1)
	text := entry.text
	c.mu.Unlock()
	c.window.record(true)
	return text, true
}

// TopEntry summarises a single cached entry for Top().
type TopEntry struct {
	Handler  string
	Target   string
	Variant  int
	HitCount uint64
}

// HandlerBreakdown aggregates TextCache entries by their handler tag.
// Returns map[handler]{size, totalHits} so cache_stats can surface
// which handlers dominate cache occupancy. [Épica 213]
type HandlerAggregate struct {
	Size      int    `json:"size"`
	TotalHits uint64 `json:"total_hits"`
}

func (c *TextCache) HandlerBreakdown() map[string]HandlerAggregate {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]HandlerAggregate, 4)
	for el := c.ll.Front(); el != nil; el = el.Next() {
		e := el.Value.(*textEntry)
		handler := e.handler
		if handler == "" {
			handler = "(legacy)"
		}
		agg := out[handler]
		agg.Size++
		agg.TotalHits += e.hitCount
		out[handler] = agg
	}
	return out
}

// Top returns up to n entries sorted by hit count descending. Entries
// without annotations (legacy Put or test inserts) fall back to a
// placeholder label so the operator still sees activity.
func (c *TextCache) Top(n int) []TopEntry {
	if c == nil || n <= 0 {
		return nil
	}
	c.mu.Lock()
	out := make([]TopEntry, 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		e := el.Value.(*textEntry)
		handler := e.handler
		if handler == "" {
			handler = "(legacy)"
		}
		target := e.target
		if target == "" {
			target = "(opaque)"
		}
		out = append(out, TopEntry{Handler: handler, Target: target, Variant: e.variant, HitCount: e.hitCount})
	}
	c.mu.Unlock()
	// Sort by hitCount descending. Stable so same-hit entries preserve
	// LRU order.
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

// Stats reports the same counters as QueryCache.Stats for symmetry on
// the BRIEFING / HUD_STATE surfaces.
func (c *TextCache) Stats() (hits, misses, evicts uint64, size int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	size = c.ll.Len()
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), c.evicts.Load(), size
}

// HitRatio mirrors QueryCache.HitRatio for BRIEFING compact output.
func (c *TextCache) HitRatio() float64 {
	h, m, _, _ := c.Stats()
	total := h + m
	if total == 0 {
		return 0
	}
	return float64(h) / float64(total)
}

// Peek returns the cached text without bumping any counter. Mirrors
// QueryCache.Peek — debugging inspections must not distort the
// observability metrics they're trying to inspect. [Épica 227]
func (c *TextCache) Peek(key TextCacheKey, currentGen uint64) (string, bool) {
	if c == nil || c.capacity <= 0 {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	entry := el.Value.(*textEntry)
	if entry.gen != currentGen {
		return "", false
	}
	return entry.text, true
}

// WindowedHitRatio returns (hits, misses, ratio) over the last `d` of
// activity. Matches QueryCache.WindowedHitRatio for symmetry. [Épica 195]
func (c *TextCache) WindowedHitRatio(d time.Duration) (hits, misses uint64, ratio float64) {
	if c == nil {
		return 0, 0, 0
	}
	return c.window.windowed(d)
}

// Resize changes the LRU capacity. Same semantics as QueryCache.Resize.
// [Épica 191]
func (c *TextCache) Resize(newCap int) {
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
		delete(c.items, oldest.Value.(*textEntry).key)
		c.evicts.Add(1)
	}
}

// Capacity returns the current configured capacity.
func (c *TextCache) Capacity() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity
}
