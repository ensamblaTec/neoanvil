// Package rag — JSON persistence for Cache[T any]. [PILAR-XXV/210]
//
// Mirrors the QueryCache / TextCache snapshot format in cache_persist.go
// but generic: T must be JSON-serializable (most common types satisfy
// this naturally via reflect-based encoding).
//
// For the EmbeddingCache (Cache[[]float32]) the on-disk shape is a
// JSON array of [0.123, -0.456, ...] per entry — human-readable, big
// (~10 KB per 768-dim vector in printed form) but manageable for the
// small N we persist (default 64). If file size becomes an issue,
// switch the value encoding to base64-packed []byte without touching
// this file's surface.

package rag

import (
	"encoding/json"
	"os"
)

// persistedGenericEntry is the on-disk shape of one cached entry for
// the generic Cache[T]. Value is typed via generics so the decoder
// materialises the right shape straight into the cache.
type persistedGenericEntry[T any] struct {
	Target   string `json:"target"`
	Variant  int    `json:"variant"`
	HitCount uint64 `json:"hit_count"`
	Value    T      `json:"value"`
}

type persistedGenericSnapshot[T any] struct {
	Version int                        `json:"version"`
	Entries []persistedGenericEntry[T] `json:"entries"`
}

// SaveSnapshotJSON serialises the top-N entries to path. Unlike the
// Query/Text variants this one generalises via T — any JSON-encodable
// value works. For Cache[[]float32] (embeddings) each entry takes
// ~10 KB of JSON so keep N modest (default 64).
func (c *Cache[T]) SaveSnapshotJSON(path string, n int) error {
	if c == nil || n <= 0 {
		return nil
	}
	top := c.TopValues(n)
	snap := persistedGenericSnapshot[T]{
		Version: snapshotVersion,
		Entries: make([]persistedGenericEntry[T], 0, len(top)),
	}
	for _, e := range top {
		snap.Entries = append(snap.Entries, persistedGenericEntry[T]{
			Target:   e.Target,
			Variant:  e.Variant,
			HitCount: e.HitCount,
			Value:    e.Value,
		})
	}

	f, err := os.Create(path) //nolint:gosec // G304-WORKSPACE-CANON: path is workspace-relative snapshot file
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

// LoadSnapshotJSON reads the JSON snapshot and populates the cache
// with currentGen as the generation stamp. Same fail-open semantics
// as QueryCache.LoadSnapshot — missing or wrong-version files return
// (0, nil) so boot never stalls on cache warmup.
//
// The caller supplies a keyPrefix that is folded into the hash: use
// "EMBED|" for the embedding cache so keys round-trip the same way
// handleSemanticCode computes them.
func (c *Cache[T]) LoadSnapshotJSON(path string, currentGen uint64, keyPrefix string) (int, error) {
	if c == nil {
		return 0, nil
	}
	f, err := os.Open(path) //nolint:gosec // G304-WORKSPACE-CANON: path is workspace-relative snapshot file
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	var snap persistedGenericSnapshot[T]
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return 0, err
	}
	if snap.Version != snapshotVersion {
		return 0, nil
	}
	loaded := 0
	for _, e := range snap.Entries {
		key := NewCacheKey(keyPrefix+e.Target, e.Variant)
		c.PutAnnotated(key, e.Value, currentGen, e.Target)
		c.mu.Lock()
		if el, ok := c.items[key]; ok {
			el.Value.(*cacheItem[T]).hitCount = e.HitCount
		}
		c.mu.Unlock()
		loaded++
	}
	return loaded, nil
}
