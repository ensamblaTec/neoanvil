package rag

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQueryCache_RoundTripPersist(t *testing.T) {
	// Populate a cache, save to disk, load into a fresh cache, verify
	// that Top() ordering + result slices + hit counts survived.
	c := NewQueryCache(16)
	// Match handleSemanticCode's "SEMANTIC_CODE|" prefix so LoadSnapshot
	// can reconstruct the same key on reload.
	for i := 0; i < 4; i++ {
		target := "target-" + string(rune('a'+i))
		k := NewQueryCacheKey("SEMANTIC_CODE|"+target, 5)
		c.PutAnnotated(k, []uint32{uint32(i), uint32(i + 1), uint32(i + 2)}, 1, target)
		// Vary hit counts so Top() ordering is deterministic.
		for j := 0; j < 4-i; j++ {
			_, _ = c.Get(k, 1)
		}
	}

	tmp := t.TempDir()
	path := filepath.Join(tmp, "snapshot.json")
	if err := c.SaveSnapshot(path, 10); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// Fresh cache, load, verify entries.
	fresh := NewQueryCache(16)
	n, err := fresh.LoadSnapshot(path, 1)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if n != 4 {
		t.Errorf("loaded %d entries, want 4", n)
	}

	// Top-ordering preserved by hit counts.
	top := fresh.Top(4)
	if len(top) != 4 {
		t.Fatalf("Top returned %d, want 4", len(top))
	}
	// First entry should be the one with most hits (target-a with 4).
	if top[0].Target != "target-a" || top[0].HitCount != 4 {
		t.Errorf("top[0] = %+v, want target-a/hit_count=4", top[0])
	}

	// Slice payload intact (loader reconstructs with SEMANTIC_CODE| prefix).
	got, ok := fresh.Get(NewQueryCacheKey("SEMANTIC_CODE|target-a", 5), 1)
	if !ok || len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Errorf("payload mismatch: ok=%v got=%v", ok, got)
	}
}

func TestTextCache_RoundTripPersist(t *testing.T) {
	c := NewTextCache(16)
	c.PutAnnotated(NewTextCacheKey("BLAST_RADIUS", "pkg/rag/x.go", 0), "# blast body A", 1, "BLAST_RADIUS", "pkg/rag/x.go", 0)
	c.PutAnnotated(NewTextCacheKey("GRAPH_WALK", "handleFoo", 100), "# graph body B", 1, "GRAPH_WALK", "handleFoo", 100)
	// Hit-count divergence for ordering.
	for i := 0; i < 5; i++ {
		_, _ = c.Get(NewTextCacheKey("BLAST_RADIUS", "pkg/rag/x.go", 0), 1)
	}
	_, _ = c.Get(NewTextCacheKey("GRAPH_WALK", "handleFoo", 100), 1)

	tmp := t.TempDir()
	path := filepath.Join(tmp, "text_snapshot.json")
	if err := c.SaveSnapshot(path, 5); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	fresh := NewTextCache(16)
	n, err := fresh.LoadSnapshot(path, 1)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if n != 2 {
		t.Errorf("loaded %d entries, want 2", n)
	}

	top := fresh.Top(5)
	if len(top) != 2 {
		t.Fatalf("Top returned %d, want 2", len(top))
	}
	// BLAST_RADIUS had 5 hits vs 1 — must come first.
	if top[0].Handler != "BLAST_RADIUS" {
		t.Errorf("top[0].Handler = %q, want BLAST_RADIUS", top[0].Handler)
	}

	// Payload survives.
	got, ok := fresh.Get(NewTextCacheKey("GRAPH_WALK", "handleFoo", 100), 1)
	if !ok || got != "# graph body B" {
		t.Errorf("text mismatch: ok=%v got=%q", ok, got)
	}
}

func TestLoadSnapshot_MissingFileIsNoOp(t *testing.T) {
	c := NewQueryCache(4)
	n, err := c.LoadSnapshot("/tmp/never-exists-neo-cache-test.json", 1)
	if err != nil {
		t.Errorf("missing file should be non-error, got %v", err)
	}
	if n != 0 {
		t.Errorf("missing file should load 0 entries, got %d", n)
	}
}

func TestLoadSnapshot_CorruptedFileFailsSoftly(t *testing.T) {
	// Write garbage JSON; LoadSnapshot should return an error but not panic.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "corrupt.json")
	if err := os.WriteFile(path, []byte("{garbage"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	c := NewQueryCache(4)
	_, err := c.LoadSnapshot(path, 1)
	if err == nil {
		t.Error("expected decode error on corrupt file")
	}
	// Cache must still be usable.
	_, _, _, size := c.Stats()
	if size != 0 {
		t.Errorf("cache should be empty after failed load, size=%d", size)
	}
}

func TestLoadSnapshot_WrongVersionIsNoOp(t *testing.T) {
	// Schema version mismatch should silently skip, not fail loud.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "v999.json")
	data := `{"version": 999, "entries": []}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	c := NewQueryCache(4)
	n, err := c.LoadSnapshot(path, 1)
	if err != nil {
		t.Errorf("wrong version should fail-open, got err=%v", err)
	}
	if n != 0 {
		t.Errorf("wrong version should skip, loaded %d", n)
	}
}
