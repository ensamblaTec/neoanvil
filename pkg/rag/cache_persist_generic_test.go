package rag

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// seedFloat32Cache populates c with vecs keyed by targets and primes hit
// counters so Top() ordering is deterministic. Extracted so the CC
// budget of the outer test stays under the 15-limit. [Épica 232.C]
func seedFloat32Cache(c *Cache[[]float32], vecs [][]float32, targets []string) {
	for i, v := range vecs {
		key := NewCacheKey("EMBED|"+targets[i], 0)
		c.PutAnnotated(key, v, 1, targets[i])
		for j := 0; j < i+2; j++ {
			_, _ = c.Get(key, 1)
		}
	}
}

// assertVectorsBitEqual compares each got[]float32 to the original vec by
// raw bit pattern (math.Float32bits) so +0/-0/NaN round-trip correctly.
// [Épica 232.C]
func assertVectorsBitEqual(t *testing.T, fresh *Cache[[]float32], vecs [][]float32, targets []string) {
	t.Helper()
	for i, v := range vecs {
		got, ok := fresh.Get(NewCacheKey("EMBED|"+targets[i], 0), 1)
		if !ok {
			t.Errorf("missing key %q on round-trip", targets[i])
			continue
		}
		if len(got) != len(v) {
			t.Errorf("length mismatch for %q: got %d want %d", targets[i], len(got), len(v))
			continue
		}
		for j := range v {
			if math.Float32bits(got[j]) != math.Float32bits(v[j]) {
				t.Errorf("float32 mismatch at %q[%d]: got %v want %v", targets[i], j, got[j], v[j])
			}
		}
	}
}

// TestGenericCache_RoundTripFloat32Slice is the workload that 210 ships:
// embedding vectors as []float32. Ensures JSON encode + decode preserves
// bit-level equality on a representative vector.
func TestGenericCache_RoundTripFloat32Slice(t *testing.T) {
	c := NewCache[[]float32](8)
	vecs := [][]float32{
		{0.5, -0.25, 0.001, 1e-6, -1.0},
		{0.123456789, 0.0, -0.987654321},
	}
	targets := []string{"query one", "query two"}
	seedFloat32Cache(c, vecs, targets)

	tmp := t.TempDir()
	path := filepath.Join(tmp, "emb.json")
	if err := c.SaveSnapshotJSON(path, 10); err != nil {
		t.Fatalf("SaveSnapshotJSON: %v", err)
	}
	if fi, statErr := os.Stat(path); statErr != nil || fi.Size() == 0 {
		t.Fatalf("snapshot file missing or empty: %v", statErr)
	}

	fresh := NewCache[[]float32](8)
	n, err := fresh.LoadSnapshotJSON(path, 1, "EMBED|")
	if err != nil {
		t.Fatalf("LoadSnapshotJSON: %v", err)
	}
	if n != 2 {
		t.Errorf("loaded %d, want 2", n)
	}

	assertVectorsBitEqual(t, fresh, vecs, targets)

	// Top-ordering preserved — "query two" had 3 hits before save, "query one"
	// had 2. After load we did ONE extra Get per target in assertVectorsBitEqual,
	// so final hit counts are 3 and 4 respectively with "query two" still on top.
	top := fresh.Top(5)
	if len(top) != 2 || top[0].Target != "query two" || top[0].HitCount < 3 {
		t.Errorf("top ordering wrong: %+v", top)
	}
}

func TestGenericCache_LoadFailOpen(t *testing.T) {
	// Missing file must not error.
	c := NewCache[[]float32](4)
	n, err := c.LoadSnapshotJSON("/tmp/neo-does-not-exist-xyz.json", 1, "EMBED|")
	if err != nil || n != 0 {
		t.Errorf("missing file should return (0, nil), got (%d, %v)", n, err)
	}

	// Wrong version must not error either.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "wrongver.json")
	if wErr := os.WriteFile(path, []byte(`{"version": 999, "entries": []}`), 0600); wErr != nil {
		t.Fatalf("setup: %v", wErr)
	}
	n, err = c.LoadSnapshotJSON(path, 1, "EMBED|")
	if err != nil || n != 0 {
		t.Errorf("wrong version should fail-open, got (%d, %v)", n, err)
	}
}

func TestGenericCache_SaveEmptyCache(t *testing.T) {
	// Saving an empty cache must produce a valid JSON file that round-trips
	// (zero entries loaded, no error).
	c := NewCache[int](4)
	tmp := t.TempDir()
	path := filepath.Join(tmp, "empty.json")
	if err := c.SaveSnapshotJSON(path, 10); err != nil {
		t.Fatalf("SaveSnapshotJSON on empty cache: %v", err)
	}
	fresh := NewCache[int](4)
	n, err := fresh.LoadSnapshotJSON(path, 1, "K|")
	if err != nil || n != 0 {
		t.Errorf("empty round-trip should return (0, nil), got (%d, %v)", n, err)
	}
}
