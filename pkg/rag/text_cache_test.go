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

// TestTextCache_MtimeFallback covers [Phase 1 MV / Speed-First]: an entry
// stamped with both a graph generation AND a file mtime stays valid when
// graph.Gen bumps (because of an UNRELATED file's edit) as long as the
// target file's mtime is unchanged. The fix decouples file-scoped cache
// validity from the global gen counter — a BLAST_RADIUS result for
// pkg/foo/x.go is no longer invalidated when someone edits pkg/bar/y.go.
func TestTextCache_MtimeFallback(t *testing.T) {
	t.Run("gen match wins", func(t *testing.T) {
		c := NewTextCache(4)
		key := NewTextCacheKey("BLAST_RADIUS", "pkg/foo.go", 0)
		c.PutWithMtime(key, "result-v1", 42, 1000, "BLAST_RADIUS", "pkg/foo.go", 0)
		got, hit := c.GetWithMtimeFallback(key, 42, 9999) // gen matches, mtime doesn't
		if !hit {
			t.Fatal("gen match alone should yield a hit")
		}
		if got != "result-v1" {
			t.Errorf("got %q want result-v1", got)
		}
	})

	t.Run("mtime fallback when gen bumped", func(t *testing.T) {
		// The headline fix: gen has bumped (some other file got edited),
		// but the target file's mtime is unchanged → still a hit.
		c := NewTextCache(4)
		key := NewTextCacheKey("BLAST_RADIUS", "pkg/foo.go", 0)
		c.PutWithMtime(key, "result-v1", 42, 1000, "BLAST_RADIUS", "pkg/foo.go", 0)
		got, hit := c.GetWithMtimeFallback(key, 99, 1000) // gen mismatch, mtime match
		if !hit {
			t.Fatal("mtime fallback should yield a hit on gen mismatch")
		}
		if got != "result-v1" {
			t.Errorf("got %q want result-v1", got)
		}
	})

	t.Run("mtime miss when target file changed", func(t *testing.T) {
		// The target file actually changed → cache MUST miss, even if gen
		// hasn't bumped yet (no certify has happened to invalidate via gen).
		c := NewTextCache(4)
		key := NewTextCacheKey("BLAST_RADIUS", "pkg/foo.go", 0)
		c.PutWithMtime(key, "result-v1", 42, 1000, "BLAST_RADIUS", "pkg/foo.go", 0)
		_, hit := c.GetWithMtimeFallback(key, 99, 2000) // both mismatch
		if hit {
			t.Error("both gen and mtime mismatched — must miss")
		}
	})

	t.Run("zero mtime opts out (legacy path)", func(t *testing.T) {
		// PutAnnotated calls PutWithMtime with mtime=0. Such entries fall
		// back to gen-only semantics — currentMtime=anything cannot save
		// them if gen mismatches.
		c := NewTextCache(4)
		key := NewTextCacheKey("BLAST_RADIUS", "pkg/foo.go", 0)
		c.PutAnnotated(key, "result-v1", 42, "BLAST_RADIUS", "pkg/foo.go", 0)
		_, hit := c.GetWithMtimeFallback(key, 99, 1000)
		if hit {
			t.Error("entry stamped with mtime=0 must not get mtime fallback")
		}
	})

	t.Run("zero currentMtime forces gen-only check", func(t *testing.T) {
		// Caller couldn't stat the target (currentMtime=0) → falls back to
		// gen-only check regardless of what mtime the entry was stamped with.
		c := NewTextCache(4)
		key := NewTextCacheKey("BLAST_RADIUS", "pkg/foo.go", 0)
		c.PutWithMtime(key, "result-v1", 42, 1000, "BLAST_RADIUS", "pkg/foo.go", 0)
		_, hit := c.GetWithMtimeFallback(key, 99, 0)
		if hit {
			t.Error("currentMtime=0 must disable the mtime fallback path")
		}
	})

	t.Run("repeat hit on same entry increments hitCount", func(t *testing.T) {
		c := NewTextCache(4)
		key := NewTextCacheKey("BLAST_RADIUS", "pkg/foo.go", 0)
		c.PutWithMtime(key, "result-v1", 42, 1000, "BLAST_RADIUS", "pkg/foo.go", 0)
		for i := 0; i < 5; i++ {
			c.GetWithMtimeFallback(key, 99, 1000)
		}
		top := c.Top(1)
		if len(top) != 1 {
			t.Fatalf("Top(1) returned %d entries", len(top))
		}
		if top[0].HitCount != 5 {
			t.Errorf("hitCount = %d, want 5 (mtime hits must bump the counter)", top[0].HitCount)
		}
	})
}
