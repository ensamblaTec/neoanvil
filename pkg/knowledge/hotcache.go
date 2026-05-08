// pkg/knowledge/hotcache.go — In-process LRU cache for hot entries. [PILAR XXXIX / Épica 295.A]
//
// HotCache holds KnowledgeEntry records marked hot:true in RAM for O(1) lookup.
// Loaded once at boot from KnowledgeStore; updated inline by store/drop operations.
package knowledge

import "sync"

// HotCache is an in-memory map of hot KnowledgeEntries keyed by "namespace:key". [295.A]
type HotCache struct {
	mu      sync.RWMutex
	entries map[string]*KnowledgeEntry
}

// NewHotCache returns an empty HotCache.
func NewHotCache() *HotCache {
	return &HotCache{entries: make(map[string]*KnowledgeEntry)}
}

// cacheKey computes the map key.
func cacheKey(ns, key string) string { return ns + ":" + key }

// Load populates the cache from the store — iterates all namespaces and caches Hot:true entries.
// Called once at boot. O(M) where M is total entry count. [295.A]
func (hc *HotCache) Load(ks *KnowledgeStore) error {
	nss, err := ks.ListNamespaces()
	if err != nil {
		return err
	}
	hc.mu.Lock()
	defer hc.mu.Unlock()
	for _, ns := range nss {
		entries, err := ks.List(ns, "")
		if err != nil {
			continue
		}
		for i := range entries {
			if entries[i].Hot {
				cp := entries[i]
				hc.entries[cacheKey(ns, cp.Key)] = &cp
			}
		}
	}
	return nil
}

// Set upserts an entry in the hot cache. [295.A]
// Only stores the entry when e.Hot == true; removes it otherwise.
func (hc *HotCache) Set(e KnowledgeEntry) {
	k := cacheKey(e.Namespace, e.Key)
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if e.Hot {
		cp := e
		hc.entries[k] = &cp
	} else {
		delete(hc.entries, k)
	}
}

// Get returns the cached entry or (nil, false) on miss. [295.A]
func (hc *HotCache) Get(ns, key string) (*KnowledgeEntry, bool) {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	e, ok := hc.entries[cacheKey(ns, key)]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// Delete removes an entry from the cache. [295.A]
func (hc *HotCache) Delete(ns, key string) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	delete(hc.entries, cacheKey(ns, key))
}

// Stats returns (hot, total) counts. [295.A]
func (hc *HotCache) Stats() (hot, total int) {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return len(hc.entries), len(hc.entries)
}
