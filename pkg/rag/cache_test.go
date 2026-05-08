package rag

import (
	"testing"
	"time"
)

// TestGenericCache_IntegerValue exercises the generic primitive with
// an int value type — stand-in for simple structs or scalars.
func TestGenericCache_IntegerValue(t *testing.T) {
	c := NewCache[int](4)
	k := NewCacheKey("answer", 0)
	c.PutAnnotated(k, 42, 1, "answer")

	got, ok := c.Get(k, 1)
	if !ok || got != 42 {
		t.Errorf("Get returned ok=%v got=%d want 42", ok, got)
	}
}

// TestGenericCache_SliceValue — the typical use case: cache []float32
// (embeddings) or similar.
func TestGenericCache_SliceValue(t *testing.T) {
	c := NewCache[[]float32](4)
	k := NewCacheKey("vec", 0)
	original := []float32{1.0, 2.0, 3.0}
	c.PutAnnotated(k, original, 1, "vec")

	got, ok := c.Get(k, 1)
	if !ok || len(got) != 3 || got[2] != 3.0 {
		t.Errorf("slice round-trip failed: ok=%v got=%v", ok, got)
	}
}

func TestGenericCache_GenerationInvalidation(t *testing.T) {
	c := NewCache[string](4)
	k := NewCacheKey("x", 0)
	c.PutAnnotated(k, "v", 1, "x")
	if _, ok := c.Get(k, 2); ok {
		t.Error("stale gen must miss")
	}
	_, _, _, size := c.Stats()
	if size != 0 {
		t.Errorf("stale entry should evict, size=%d", size)
	}
}

func TestGenericCache_LRUEviction(t *testing.T) {
	c := NewCache[string](2)
	k1 := NewCacheKey("a", 0)
	k2 := NewCacheKey("b", 0)
	k3 := NewCacheKey("c", 0)
	c.PutAnnotated(k1, "a", 1, "a")
	c.PutAnnotated(k2, "b", 1, "b")
	_, _ = c.Get(k1, 1) // promote k1
	c.PutAnnotated(k3, "c", 1, "c")
	if _, ok := c.Get(k2, 1); ok {
		t.Error("k2 should be evicted (LRU)")
	}
}

func TestGenericCache_ResizeShrink(t *testing.T) {
	c := NewCache[int](4)
	for i := 0; i < 4; i++ {
		k := NewCacheKey("k", i)
		c.PutAnnotated(k, i, 1, "k")
	}
	c.Resize(2)
	if c.Capacity() != 2 {
		t.Errorf("capacity = %d, want 2", c.Capacity())
	}
	_, _, _, size := c.Stats()
	if size != 2 {
		t.Errorf("size after shrink = %d, want 2", size)
	}
}

func TestGenericCache_WindowedHitRatio(t *testing.T) {
	c := NewCache[string](8)
	k := NewCacheKey("q", 0)
	c.PutAnnotated(k, "body", 1, "q")
	// Record a hit then check the window.
	_, _ = c.Get(k, 1)
	_, _ = c.Get(k, 1)
	h, _, r := c.WindowedHitRatio(1 * time.Second)
	if h != 2 || r < 0.99 {
		t.Errorf("windowed ratio wrong: hits=%d ratio=%v", h, r)
	}
}

func TestGenericCache_Top(t *testing.T) {
	c := NewCache[string](8)
	ka := NewCacheKey("a", 0)
	kb := NewCacheKey("b", 0)
	c.PutAnnotated(ka, "a", 1, "a")
	c.PutAnnotated(kb, "b", 1, "b")
	for i := 0; i < 5; i++ {
		_, _ = c.Get(ka, 1)
	}
	for i := 0; i < 2; i++ {
		_, _ = c.Get(kb, 1)
	}
	top := c.Top(2)
	if len(top) != 2 || top[0].Target != "a" || top[0].HitCount != 5 {
		t.Errorf("Top ordering wrong: %+v", top)
	}
}

func TestGenericCache_RecentMisses(t *testing.T) {
	c := NewCache[int](4)
	// Don't populate — just record misses.
	c.RecordMiss("query one")
	c.RecordMiss("query two")
	c.RecordMiss("query one") // duplicate
	recent := c.RecentMissTargets(3)
	// DISTINCT: should return ["query one", "query two"] or ["query two", "query one"].
	if len(recent) != 2 {
		t.Errorf("expected 2 distinct, got %d: %v", len(recent), recent)
	}
}

func TestGenericCache_DisabledWithZeroCapacity(t *testing.T) {
	c := NewCache[int](0)
	k := NewCacheKey("x", 0)
	c.PutAnnotated(k, 42, 1, "x")
	if _, ok := c.Get(k, 1); ok {
		t.Error("zero-capacity cache must always miss")
	}
}
