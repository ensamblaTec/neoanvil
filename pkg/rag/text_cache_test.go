package rag

import (
	"sync"
	"testing"
)

func TestTextCache_HitMiss(t *testing.T) {
	c := NewTextCache(16)
	k := NewTextCacheKey("BLAST_RADIUS", "pkg/rag/hsnw.go", 0)
	if _, ok := c.Get(k, 1); ok {
		t.Fatal("empty cache should miss")
	}
	c.Put(k, "## Blast Radius\n...", 1)
	got, ok := c.Get(k, 1)
	if !ok {
		t.Fatal("expected hit after Put")
	}
	if got == "" {
		t.Error("expected non-empty hit")
	}
}

func TestTextCache_GenerationInvalidation(t *testing.T) {
	c := NewTextCache(16)
	k := NewTextCacheKey("BLAST_RADIUS", "pkg/rag/hsnw.go", 0)
	c.Put(k, "body", 1)
	if _, ok := c.Get(k, 2); ok {
		t.Fatal("stale generation must miss")
	}
	_, _, _, size := c.Stats()
	if size != 0 {
		t.Errorf("stale entry should evict, size=%d", size)
	}
}

func TestTextCache_Variants(t *testing.T) {
	// Different variant int → distinct slot.
	c := NewTextCache(4)
	k1 := NewTextCacheKey("BLAST_RADIUS", "file.go", 0)
	k2 := NewTextCacheKey("BLAST_RADIUS", "file.go", 1)
	c.Put(k1, "body-0", 1)
	c.Put(k2, "body-1", 1)
	if v, _ := c.Get(k1, 1); v != "body-0" {
		t.Errorf("variant 0 mismatch: %q", v)
	}
	if v, _ := c.Get(k2, 1); v != "body-1" {
		t.Errorf("variant 1 mismatch: %q", v)
	}
}

func TestTextCache_LRUEviction(t *testing.T) {
	c := NewTextCache(2)
	k1 := NewTextCacheKey("t", "a", 0)
	k2 := NewTextCacheKey("t", "b", 0)
	k3 := NewTextCacheKey("t", "c", 0)
	c.Put(k1, "a", 1)
	c.Put(k2, "b", 1)
	_, _ = c.Get(k1, 1) // promote
	c.Put(k3, "c", 1)
	if _, ok := c.Get(k2, 1); ok {
		t.Error("k2 should have been evicted (LRU)")
	}
}

func TestTextCache_DisabledWithZeroCapacity(t *testing.T) {
	c := NewTextCache(0)
	k := NewTextCacheKey("x", "y", 0)
	c.Put(k, "body", 1)
	if _, ok := c.Get(k, 1); ok {
		t.Error("zero-capacity cache must always miss")
	}
}

func TestTextCache_ConcurrentTraffic(t *testing.T) {
	c := NewTextCache(64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				k := NewTextCacheKey("BR", "target", shard*500+j)
				if _, ok := c.Get(k, 1); !ok {
					c.Put(k, "some markdown body", 1)
				}
			}
		}(i)
	}
	wg.Wait()
	_, _, _, size := c.Stats()
	if size > 64 {
		t.Errorf("size exceeded capacity: %d > 64", size)
	}
}

func BenchmarkTextCache_Hit(b *testing.B) {
	c := NewTextCache(128)
	k := NewTextCacheKey("BR", "file.go", 0)
	c.Put(k, "## body\n... several KB of markdown ...", 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(k, 1)
	}
}

func BenchmarkTextCache_Miss(b *testing.B) {
	c := NewTextCache(128)
	k := NewTextCacheKey("BR", "never", 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(k, 1)
	}
}
