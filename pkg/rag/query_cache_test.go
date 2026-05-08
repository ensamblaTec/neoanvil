package rag

import (
	"sync"
	"testing"
)

func TestQueryCache_HitMiss(t *testing.T) {
	c := NewQueryCache(16)
	k := NewQueryCacheKey("hello world", 5)

	if _, ok := c.Get(k, 1); ok {
		t.Fatal("empty cache should miss")
	}
	c.Put(k, []uint32{1, 2, 3}, 1)
	got, ok := c.Get(k, 1)
	if !ok {
		t.Fatal("expected hit after Put")
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("unexpected result: %v", got)
	}
}

func TestQueryCache_GenerationInvalidation(t *testing.T) {
	c := NewQueryCache(16)
	k := NewQueryCacheKey("hello", 5)
	c.Put(k, []uint32{1, 2, 3}, 1)

	// Bump generation (simulating a new ingest).
	if _, ok := c.Get(k, 2); ok {
		t.Fatal("entry from gen=1 must not satisfy gen=2 lookup")
	}
	// Second lookup with the same stale gen should still miss AND the
	// entry should be evicted — no stale data lingering.
	_, _, _, size := c.Stats()
	if size != 0 {
		t.Errorf("stale entry should be evicted, size = %d", size)
	}
}

func TestQueryCache_LRUEviction(t *testing.T) {
	c := NewQueryCache(2)
	k1 := NewQueryCacheKey("a", 5)
	k2 := NewQueryCacheKey("b", 5)
	k3 := NewQueryCacheKey("c", 5)

	c.Put(k1, []uint32{1}, 1)
	c.Put(k2, []uint32{2}, 1)
	// Accessing k1 promotes it → k2 becomes the eviction candidate.
	_, _ = c.Get(k1, 1)
	c.Put(k3, []uint32{3}, 1)

	if _, ok := c.Get(k2, 1); ok {
		t.Error("k2 should have been evicted as least-recently-used")
	}
	if _, ok := c.Get(k1, 1); !ok {
		t.Error("k1 was recently accessed and should still be present")
	}
	if _, ok := c.Get(k3, 1); !ok {
		t.Error("k3 was just inserted and should still be present")
	}
}

func TestQueryCache_CopyOnRead(t *testing.T) {
	// Callers must not be able to mutate cached results indirectly.
	c := NewQueryCache(4)
	k := NewQueryCacheKey("query", 3)
	c.Put(k, []uint32{10, 20, 30}, 1)

	first, _ := c.Get(k, 1)
	first[0] = 9999 // caller mutation

	second, _ := c.Get(k, 1)
	if second[0] == 9999 {
		t.Error("cache leaks its internal slice — caller mutation leaked")
	}
}

func TestQueryCache_DisabledWithZeroCapacity(t *testing.T) {
	c := NewQueryCache(0)
	k := NewQueryCacheKey("anything", 5)
	c.Put(k, []uint32{1, 2, 3}, 1)
	if _, ok := c.Get(k, 1); ok {
		t.Error("zero-capacity cache must always miss")
	}
}

func TestQueryCache_HitRatio(t *testing.T) {
	c := NewQueryCache(4)
	k := NewQueryCacheKey("q", 5)
	// 3 misses, then a put, then 2 hits → hit ratio = 2/5 = 0.4.
	for i := 0; i < 3; i++ {
		_, _ = c.Get(k, 1)
	}
	c.Put(k, []uint32{7}, 1)
	for i := 0; i < 2; i++ {
		_, _ = c.Get(k, 1)
	}
	ratio := c.HitRatio()
	if ratio < 0.39 || ratio > 0.41 {
		t.Errorf("hit ratio = %v, want ≈0.4", ratio)
	}
}

// TestQueryCache_ConcurrentGetAndPut stresses the mutex under parallel
// access. A race failure here is serious — Get and Put both mutate the
// LRU structure even for read paths.
func TestQueryCache_ConcurrentGetAndPut(t *testing.T) {
	c := NewQueryCache(64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				k := NewQueryCacheKey("query", shard*1000+j)
				if _, ok := c.Get(k, 1); !ok {
					c.Put(k, []uint32{uint32(j)}, 1)
				}
			}
		}(i)
	}
	wg.Wait()
	// Size must not exceed capacity despite the concurrent traffic.
	_, _, _, size := c.Stats()
	if size > 64 {
		t.Errorf("size exceeded capacity: %d > 64", size)
	}
}

func BenchmarkQueryCache_Hit(b *testing.B) {
	c := NewQueryCache(128)
	k := NewQueryCacheKey("frequent query", 5)
	c.Put(k, []uint32{1, 2, 3, 4, 5}, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(k, 1)
	}
}

func BenchmarkQueryCache_Miss(b *testing.B) {
	c := NewQueryCache(128)
	k := NewQueryCacheKey("never-present", 5)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(k, 1)
	}
}
