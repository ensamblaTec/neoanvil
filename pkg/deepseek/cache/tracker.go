// Package cache provides structural prompt caching for the DeepSeek plugin (PILAR XXIV / 131.C).
//
// Block 1 (static): system prompt + global directives + code files — built once per unique file set.
// Block 2 (dynamic): task-specific content — always fresh.
//
// DeepSeek API does not charge for identical prefix repetition within the cache window.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"sort"
	"sync"
)

// CacheKey is the SHA-256 fingerprint of a sorted set of file contents.
type CacheKey string

// CacheKeyTracker maintains per-file SHA-256 hashes and derives a composite CacheKey
// for a set of file paths. Safe for concurrent use.
type CacheKeyTracker struct {
	mu     sync.RWMutex
	hashes map[string]string // file path → sha256 hex
}

// NewTracker returns an empty CacheKeyTracker.
func NewTracker() *CacheKeyTracker {
	return &CacheKeyTracker{hashes: make(map[string]string)}
}

// Snapshot reads each file, updates the per-file hash cache, and returns a
// CacheKey representing the entire set. Unreadable files are skipped with a log
// warning (fail-open policy).
func (t *CacheKeyTracker) Snapshot(files []string) CacheKey {
	type entry struct {
		path string
		hash string
	}
	entries := make([]entry, 0, len(files))

	for _, path := range files {
		data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: paths from operator config, not external input
		if err != nil {
			log.Printf("[deepseek/cache] Snapshot: cannot read %s: %v (skipping)", path, err)
			t.mu.Lock()
			delete(t.hashes, path)
			t.mu.Unlock()
			continue
		}
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])
		t.mu.Lock()
		t.hashes[path] = h
		t.mu.Unlock()
		entries = append(entries, entry{path, h})
	}

	// Sort by path for determinism regardless of input order.
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	combined := sha256.New()
	for _, e := range entries {
		combined.Write([]byte(e.path))
		combined.Write([]byte(e.hash))
	}
	return CacheKey(hex.EncodeToString(combined.Sum(nil)))
}

// Get returns the cached hash for a single file, or ("", false) if not tracked.
func (t *CacheKeyTracker) Get(path string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	h, ok := t.hashes[path]
	return h, ok
}
