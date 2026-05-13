// pkg/rag/hotfile_cache.go — per-workspace LRU cache for raw file contents
// (Tier 1 A from the LARGE-PROJECT roadmap, 2026-05-13).
//
// Purpose: skip repeat `os.ReadFile()` calls in tool handlers that touch the
// same files many times within a pair-mode session. Typical workflow reads
// master_plan.md / technical_debt.md / router.go etc. dozens of times per
// session — each call is an unnecessary syscall + decode.
//
// Design:
//   - Capacity bound is BYTES (not entry count) so large files don't poison
//     the cache with tiny entries.
//   - Invalidation on every Get via os.Stat — if mtime OR size changed, evict
//     and report miss. The cache never serves stale content.
//   - LRU eviction when total bytes exceeds cap.
//   - Lock-free metrics (atomic) for cheap stats reads.
//
// Non-goals: cross-process sharing (each MCP child has its own cache), TTL
// expiration (mtime invalidation suffices), content compression (would add
// CPU cost without proportional memory benefit at typical workspace size).

package rag

import (
	"container/list"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// HotFileEntry is a cached file content with its mtime + size at the moment
// of caching. Both serve as invalidation keys — a Get() returns hit only if
// the on-disk file's mtime AND size match the cached entry.
type HotFileEntry struct {
	Path    string
	MTime   time.Time
	Size    int64
	Content []byte
	lruElem *list.Element
}

// HotFilesCache is an LRU cache for raw file contents keyed by absolute path.
// Safe for concurrent use; all mutating ops hold a single mutex (the LRU list
// operations are cheap so contention is bounded).
type HotFilesCache struct {
	mu         sync.Mutex // protects entries, lru, totalBytes
	entries    map[string]*HotFileEntry
	lru        *list.List
	capBytes   int64
	totalBytes int64

	// Atomic counters — read-side stats without taking the mutex.
	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64 // LRU evictions (capacity pressure)
	stale     atomic.Int64 // mtime/size mismatch invalidations on Get
}

// NewHotFilesCache constructs an LRU cache bounded by capacityBytes. A
// capacityBytes <= 0 returns a usable cache that immediately evicts every
// Put (effectively disabled but safe for tests / probe usage).
func NewHotFilesCache(capacityBytes int64) *HotFilesCache {
	return &HotFilesCache{
		entries:  make(map[string]*HotFileEntry),
		lru:      list.New(),
		capBytes: capacityBytes,
	}
}

// Get returns the cached content if (path, mtime, size) all match the
// current disk state. Returns (nil, false) on miss, on stale entry (which
// is evicted), or on stat error (entry evicted to avoid serving a
// possibly-deleted file).
func (c *HotFilesCache) Get(path string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[path]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil {
		c.stale.Add(1)
		c.evictLocked(entry)
		return nil, false
	}
	if !info.ModTime().Equal(entry.MTime) || info.Size() != entry.Size {
		c.stale.Add(1)
		c.evictLocked(entry)
		return nil, false
	}
	c.lru.MoveToFront(entry.lruElem)
	c.hits.Add(1)
	return entry.Content, true
}

// Put stores content under path with the file's current mtime + size as
// invalidation keys. If the entry alone exceeds capBytes, it is silently
// skipped (we don't admit + immediately evict — pointless work). If
// totalBytes exceeds cap after admission, oldest entries are evicted.
//
// stat is performed inside Put to capture mtime+size atomically with the
// content the caller passed; callers MUST pass content that was just read
// from disk, NOT mutated downstream content.
func (c *HotFilesCache) Put(path string, content []byte) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	size := int64(len(content))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capBytes <= 0 || size > c.capBytes {
		return
	}
	if old, ok := c.entries[path]; ok {
		c.totalBytes -= old.Size
		c.lru.Remove(old.lruElem)
		delete(c.entries, path)
	}
	entry := &HotFileEntry{
		Path:    path,
		MTime:   info.ModTime(),
		Size:    size,
		Content: content,
	}
	entry.lruElem = c.lru.PushFront(entry)
	c.entries[path] = entry
	c.totalBytes += size
	for c.totalBytes > c.capBytes {
		back := c.lru.Back()
		if back == nil {
			break
		}
		oldest, ok := back.Value.(*HotFileEntry)
		if !ok {
			c.lru.Remove(back)
			continue
		}
		c.evictLocked(oldest)
		c.evictions.Add(1)
	}
}

// Invalidate forcibly removes a path from the cache. Used by handlers that
// know the file was just mutated by the agent (e.g. after a certify cycle).
func (c *HotFilesCache) Invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[path]; ok {
		c.evictLocked(entry)
	}
}

// evictLocked removes an entry from both the map and the LRU list, adjusting
// totalBytes. Caller must hold c.mu.
func (c *HotFilesCache) evictLocked(e *HotFileEntry) {
	delete(c.entries, e.Path)
	c.lru.Remove(e.lruElem)
	c.totalBytes -= e.Size
}

// HotFilesCacheStats is the read-only snapshot returned by Stats().
type HotFilesCacheStats struct {
	EntryCount int     `json:"entry_count"`
	TotalBytes int64   `json:"total_bytes"`
	CapBytes   int64   `json:"capacity_bytes"`
	Hits       int64   `json:"hits"`
	Misses     int64   `json:"misses"`
	Stale      int64   `json:"stale_invalidations"`
	Evictions  int64   `json:"evictions"`
	HitRatio   float64 `json:"hit_ratio"`
}

// Stats returns a coherent snapshot of current cache state and counters.
// Counters are atomic, so the snapshot may show a slightly later count for
// hits/misses than entry counts (acceptable for observability).
func (c *HotFilesCache) Stats() HotFilesCacheStats {
	c.mu.Lock()
	entryCount := len(c.entries)
	totalBytes := c.totalBytes
	c.mu.Unlock()
	hits := c.hits.Load()
	misses := c.misses.Load()
	total := hits + misses
	ratio := 0.0
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return HotFilesCacheStats{
		EntryCount: entryCount,
		TotalBytes: totalBytes,
		CapBytes:   c.capBytes,
		Hits:       hits,
		Misses:     misses,
		Stale:      c.stale.Load(),
		Evictions:  c.evictions.Load(),
		HitRatio:   ratio,
	}
}
