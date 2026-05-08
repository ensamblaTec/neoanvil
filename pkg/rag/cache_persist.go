// Package rag — disk persistence for cache warm-entries. [PILAR-XXV/197]
//
// Keeps the top-N most-hit QueryCache entries in a JSON file under
// .neo/db/query_cache.snapshot.json. Save is explicit (operator calls
// neo_cache_persist before make rebuild-restart). Load is implicit at
// boot so the next session starts warm for the queries that already
// dominated the previous one.
//
// Rationale for JSON + top-N instead of BoltDB + everything:
//   - Forward compat: JSON lets us evolve the schema field-by-field
//     without BoltDB bucket versioning dances.
//   - Small: typical top-32 QueryCache snapshot is <10 KB.
//   - Safe: loading into a fresh empty cache with current Gen is a
//     valid state — if HNSW has mutated between save and load,
//     InsertBatch bumps Gen and the loaded entries evict on first Get.
//   - Privacy: text_cache snapshots could contain a lot of bytes; we
//     keep this to QueryCache for now (lean payloads, node IDs only).

package rag

import (
	"encoding/json"
	"os"
)

// persistedEntry is the on-disk shape of one cached (target → nodeIDs).
// Omits gen intentionally — on load we stamp with the CALLER-supplied
// current generation so the entry is immediately valid for the active
// HNSW state.
type persistedEntry struct {
	Target   string   `json:"target"`
	TopK     int      `json:"top_k"`
	Result   []uint32 `json:"result"`
	HitCount uint64   `json:"hit_count"`
}

// persistedTextEntry mirrors persistedEntry for TextCache payloads.
// Handler + variant round-trip so Top() on load keeps the per-entry
// annotations that feed neo_cache_stats. [Épica 200]
type persistedTextEntry struct {
	Handler  string `json:"handler"`
	Target   string `json:"target"`
	Variant  int    `json:"variant"`
	Text     string `json:"text"`
	HitCount uint64 `json:"hit_count"`
}

type persistedTextSnapshot struct {
	Version int                  `json:"version"`
	Entries []persistedTextEntry `json:"entries"`
}

// persistedSnapshot wraps the entry list with a version tag so future
// format changes stay backwards-compatible at the decoder level.
type persistedSnapshot struct {
	Version int              `json:"version"`
	Entries []persistedEntry `json:"entries"`
}

const snapshotVersion = 1

// SaveSnapshot writes the top-N most-hit entries to path. Overwrites
// any existing file. Safe to call concurrently with Get/Put — the
// Top(n) read takes the cache mutex briefly and then the encode runs
// on a detached slice.
func (c *QueryCache) SaveSnapshot(path string, n int) error {
	if c == nil || n <= 0 {
		return nil
	}
	top := c.Top(n)
	snap := persistedSnapshot{Version: snapshotVersion, Entries: make([]persistedEntry, 0, len(top))}
	// Top() returns QueryTopEntry — we need the Result slice too, so
	// walk the LRU once more under lock to grab it.
	c.mu.Lock()
	for _, e := range top {
		// Look up by target linearly — N is small (default 10-50).
		for el := c.ll.Front(); el != nil; el = el.Next() {
			entry := el.Value.(*cacheEntry)
			if entry.target == e.Target && entry.key.TopK == e.TopK {
				resultCopy := make([]uint32, len(entry.result))
				copy(resultCopy, entry.result)
				snap.Entries = append(snap.Entries, persistedEntry{
					Target:   entry.target,
					TopK:     entry.key.TopK,
					Result:   resultCopy,
					HitCount: entry.hitCount,
				})
				break
			}
		}
	}
	c.mu.Unlock()

	f, err := os.Create(path) //nolint:gosec // G304-WORKSPACE-CANON: path is workspace-relative snapshot file
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

// SaveSnapshot writes TextCache top-N entries (including full bodies)
// to path. Files can grow larger than QueryCache's — cap N accordingly.
// [Épica 200]
func (c *TextCache) SaveSnapshot(path string, n int) error {
	if c == nil || n <= 0 {
		return nil
	}
	top := c.Top(n)
	snap := persistedTextSnapshot{Version: snapshotVersion, Entries: make([]persistedTextEntry, 0, len(top))}
	c.mu.Lock()
	for _, e := range top {
		for el := c.ll.Front(); el != nil; el = el.Next() {
			entry := el.Value.(*textEntry)
			if entry.handler == e.Handler && entry.target == e.Target && entry.variant == e.Variant {
				snap.Entries = append(snap.Entries, persistedTextEntry{
					Handler:  entry.handler,
					Target:   entry.target,
					Variant:  entry.variant,
					Text:     entry.text,
					HitCount: entry.hitCount,
				})
				break
			}
		}
	}
	c.mu.Unlock()

	f, err := os.Create(path) //nolint:gosec // G304-WORKSPACE-CANON: path is workspace-relative snapshot file
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

// LoadSnapshot reads the JSON file and re-populates the TextCache. Same
// semantics as QueryCache.LoadSnapshot. [Épica 200]
func (c *TextCache) LoadSnapshot(path string, currentGen uint64) (int, error) {
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
	var snap persistedTextSnapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return 0, err
	}
	if snap.Version != snapshotVersion {
		return 0, nil
	}
	loaded := 0
	for _, e := range snap.Entries {
		key := NewTextCacheKey(e.Handler, e.Target, e.Variant)
		c.PutAnnotated(key, e.Text, currentGen, e.Handler, e.Target, e.Variant)
		c.mu.Lock()
		if el, ok := c.items[key]; ok {
			el.Value.(*textEntry).hitCount = e.HitCount
		}
		c.mu.Unlock()
		loaded++
	}
	return loaded, nil
}

// LoadSnapshot reads the JSON file and re-populates the cache using the
// supplied currentGen as the generation stamp. Missing files return nil
// (first boot case). Schema-version mismatch also returns nil — we fail
// open rather than fail loud on boot.
func (c *QueryCache) LoadSnapshot(path string, currentGen uint64) (int, error) {
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

	var snap persistedSnapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return 0, err
	}
	if snap.Version != snapshotVersion {
		return 0, nil // silently skip incompatible
	}
	loaded := 0
	for _, e := range snap.Entries {
		key := NewQueryCacheKey("SEMANTIC_CODE|"+e.Target, e.TopK)
		c.PutAnnotated(key, e.Result, currentGen, e.Target)
		// Restore hit count so Top() after a boot still shows real popularity.
		c.mu.Lock()
		if el, ok := c.items[key]; ok {
			el.Value.(*cacheEntry).hitCount = e.HitCount
		}
		c.mu.Unlock()
		loaded++
	}
	return loaded, nil
}
