package main

// Regression tests for Nexus debt T002 (TECH_DEBT_MAP-TOKEN-FLOOD, P1).
//
// Operator burned ~$47 across 477 calls in one workspace before this
// gate landed because handleTechDebtMap had zero caching. Repeat
// callers re-rendered the full hotspot table + scatter on every hit.
//
// We can't easily unit-test handleTechDebtMap end-to-end (it pulls
// from telemetry + cpg + wal), but we CAN unit-test the cache layer
// itself — that's where the TTL bug would land if anyone breaks the
// invariants later.

import (
	"sync"
	"testing"
	"time"
)

func TestTechDebtMapCache_PutThenGet_HitsWithinTTL(t *testing.T) {
	// Reset cache for test isolation.
	techDebtMapCacheMu.Lock()
	techDebtMapCache = make(map[string]techDebtMapCacheEntry)
	techDebtMapCacheMu.Unlock()

	key := techDebtMapCacheKey("/tmp/ws", 10, "")
	techDebtMapCachePut(key, "rendered body")

	got, ok := techDebtMapCacheGet(key)
	if !ok {
		t.Fatal("expected cache hit immediately after put")
	}
	if got != "rendered body" {
		t.Errorf("body=%q want %q", got, "rendered body")
	}
}

func TestTechDebtMapCache_DistinctKeysIsolate(t *testing.T) {
	techDebtMapCacheMu.Lock()
	techDebtMapCache = make(map[string]techDebtMapCacheEntry)
	techDebtMapCacheMu.Unlock()

	keyLocal := techDebtMapCacheKey("/tmp/ws", 10, "")
	keyProject := techDebtMapCacheKey("/tmp/ws", 10, "project")

	techDebtMapCachePut(keyLocal, "local body")
	techDebtMapCachePut(keyProject, "project body")

	if got, _ := techDebtMapCacheGet(keyLocal); got != "local body" {
		t.Errorf("local key returned %q", got)
	}
	if got, _ := techDebtMapCacheGet(keyProject); got != "project body" {
		t.Errorf("project key returned %q", got)
	}
}

func TestTechDebtMapCache_DifferentLimitDistinctKeys(t *testing.T) {
	// limit:5 and limit:20 must NOT share — different render output
	// (different rank cutoff). Cache key includes limit on purpose.
	techDebtMapCacheMu.Lock()
	techDebtMapCache = make(map[string]techDebtMapCacheEntry)
	techDebtMapCacheMu.Unlock()

	k5 := techDebtMapCacheKey("/tmp/ws", 5, "")
	k20 := techDebtMapCacheKey("/tmp/ws", 20, "")
	if k5 == k20 {
		t.Fatalf("cache keys collide for limit=5 vs limit=20: %q", k5)
	}
	techDebtMapCachePut(k5, "top-5")
	techDebtMapCachePut(k20, "top-20")
	if got, _ := techDebtMapCacheGet(k5); got != "top-5" {
		t.Errorf("k5 got %q", got)
	}
	if got, _ := techDebtMapCacheGet(k20); got != "top-20" {
		t.Errorf("k20 got %q", got)
	}
}

func TestTechDebtMapCache_ExpiredEntryNotReturned(t *testing.T) {
	// Inject an entry with explicit past expiry to force the stale
	// branch in techDebtMapCacheGet.
	techDebtMapCacheMu.Lock()
	techDebtMapCache = map[string]techDebtMapCacheEntry{
		"stale": {body: "old body", expires: time.Now().Add(-1 * time.Minute)},
	}
	techDebtMapCacheMu.Unlock()

	if _, ok := techDebtMapCacheGet("stale"); ok {
		t.Errorf("expected miss on stale entry")
	}
}

func TestTechDebtMapCache_RaceFreeUnderConcurrentReadWrite(t *testing.T) {
	// Hammer the cache from many goroutines to surface lock holes.
	techDebtMapCacheMu.Lock()
	techDebtMapCache = make(map[string]techDebtMapCacheEntry)
	techDebtMapCacheMu.Unlock()

	const goroutines = 20
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			key := techDebtMapCacheKey("ws", id%5, "")
			for j := 0; j < iterations; j++ {
				techDebtMapCachePut(key, "body")
				_, _ = techDebtMapCacheGet(key)
			}
		}(i)
	}
	wg.Wait()
	// If we reach here without -race firing, the lock invariants hold.
}
