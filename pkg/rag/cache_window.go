// Package rag — windowed hit ratio tracker shared by both cache layers.
// [PILAR-XXV/195]
//
// Lifetime hit_ratio (QueryCache.HitRatio() / TextCache.HitRatio()) is
// polluted by boot warmup and long-running sessions — a cache that
// started cold and then ran at 90% hit rate for an hour reports
// something like 60% because the first ~1k misses dominate the mean.
//
// The windowed variant keeps a circular buffer of the last N events,
// each a 16-byte record (time + 1-byte flag). A 5-minute window at
// 10 tool calls/sec means ~3 000 events in flight; we cap the ring at
// 4 096 to bound memory. Operations are O(1) amortized — the buffer
// is a plain slice, the head index wraps with modulo, and the query
// scans forward from the newest entry until it exits the window.

package rag

import (
	"sync"
	"time"
)

// cacheWindow is embedded inside QueryCache and TextCache. 4096-entry
// ring + single mutex. Allocations: one slice at construction, zero on
// the hot path.
type cacheWindow struct {
	mu    sync.Mutex
	ring  []windowEvent
	pos   int // next write index
	size  int // number of filled slots (capped at len(ring))
}

type windowEvent struct {
	t   time.Time
	hit bool
}

const windowCap = 4096

// missRingCap caps the recent-miss string buffer. 64 is plenty to surface
// top candidates for neo_cache_warmup without holding much memory.
const missRingCap = 64

// missRing is a per-cache circular buffer of the most recent miss targets,
// held as plain strings for easy operator consumption. [Épica 196]
type missRing struct {
	mu   sync.Mutex
	ring []string
	pos  int
	size int
}

func newMissRing() *missRing {
	return &missRing{ring: make([]string, missRingCap)}
}

// record appends a miss target. Empty strings are ignored so legacy
// callers that don't have the original query can opt out safely.
func (r *missRing) record(target string) {
	if r == nil || target == "" {
		return
	}
	r.mu.Lock()
	r.ring[r.pos] = target
	r.pos = (r.pos + 1) % len(r.ring)
	if r.size < len(r.ring) {
		r.size++
	}
	r.mu.Unlock()
}

// recent returns up to n most-recent DISTINCT miss targets, newest first.
// Distinct because consecutive misses on the same target during a cold
// start would bias the top-N; operators want coverage, not frequency.
func (r *missRing) recent(n int) []string {
	if r == nil || n <= 0 || r.size == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{}, n)
	out := make([]string, 0, n)
	for i := 0; i < r.size && len(out) < n; i++ {
		idx := (r.pos - 1 - i + len(r.ring)) % len(r.ring)
		t := r.ring[idx]
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func newCacheWindow() *cacheWindow {
	return &cacheWindow{ring: make([]windowEvent, windowCap)}
}

// record appends a hit or miss event. Concurrent-safe.
func (w *cacheWindow) record(hit bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.ring[w.pos] = windowEvent{t: time.Now(), hit: hit}
	w.pos = (w.pos + 1) % len(w.ring)
	if w.size < len(w.ring) {
		w.size++
	}
	w.mu.Unlock()
}

// windowed returns (hits, misses, hit_ratio) over events in the last
// `d` duration. Ratio is 0 when no events fall in the window.
func (w *cacheWindow) windowed(d time.Duration) (hits, misses uint64, ratio float64) {
	if w == nil || d <= 0 {
		return 0, 0, 0
	}
	cutoff := time.Now().Add(-d)
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := 0; i < w.size; i++ {
		idx := (w.pos - 1 - i + len(w.ring)) % len(w.ring)
		ev := w.ring[idx]
		if ev.t.Before(cutoff) {
			break // events in the ring are ordered; we can stop scanning
		}
		if ev.hit {
			hits++
		} else {
			misses++
		}
	}
	total := hits + misses
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return
}
