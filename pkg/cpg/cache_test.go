package cpg_test

import (
	"bytes"
	"encoding/gob"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

func openTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	f := filepath.Join(t.TempDir(), "test.db")
	db, err := bolt.Open(f, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestCachedBuild verifies that the second call returns a cache hit in <10ms.
func TestCachedBuild(t *testing.T) {
	root := repoRoot(t)
	db := openTestDB(t)

	cache, err := cpg.NewGraphCache(db)
	if err != nil {
		t.Fatalf("NewGraphCache: %v", err)
	}

	builder := cpg.NewBuilder(cpg.BuildConfig{Dir: root})
	pkgDir := filepath.Join(root, "cmd", "neo-mcp")
	pkgPattern := "./cmd/neo-mcp"

	// First call — cache miss, builds fresh.
	t.Log("first call (expected miss)…")
	g1, elapsed1, err := builder.BuildCached(pkgPattern, pkgDir, cache)
	if err != nil {
		t.Fatalf("first BuildCached: %v", err)
	}
	t.Logf("  miss: %v — %d nodes, %d edges", elapsed1, len(g1.Nodes), len(g1.Edges))

	// Second call — must be a cache hit.
	t.Log("second call (expected hit)…")
	start2 := time.Now()
	g2, elapsed2, err := builder.BuildCached(pkgPattern, pkgDir, cache)
	wallClock2 := time.Since(start2)
	if err != nil {
		t.Fatalf("second BuildCached: %v", err)
	}
	t.Logf("  hit: elapsed=%v wall=%v — %d nodes, %d edges", elapsed2, wallClock2, len(g2.Nodes), len(g2.Edges))

	// elapsed2 == 0 means it was served from cache (BuildCached returns 0 on hit).
	if elapsed2 != 0 {
		t.Errorf("expected elapsed=0 on cache hit, got %v", elapsed2)
	}
	// Threshold relaxed from 10ms → 100ms because the race detector
	// adds 2-20× overhead — cold rebuild takes ~1s, so 100ms still
	// validates "cached" without `-race` flapping the test.
	if wallClock2 > 100*time.Millisecond {
		t.Errorf("cache hit took %v, expected <100ms", wallClock2)
	}

	if len(g1.Nodes) != len(g2.Nodes) {
		t.Errorf("node count mismatch: miss=%d hit=%d", len(g1.Nodes), len(g2.Nodes))
	}
	if len(g1.Edges) != len(g2.Edges) {
		t.Errorf("edge count mismatch: miss=%d hit=%d", len(g1.Edges), len(g2.Edges))
	}
}

// TestCacheGobRoundtrip verifies gob encode/decode latency stays <20ms.
func TestCacheGobRoundtrip(t *testing.T) {
	root := repoRoot(t)
	builder := cpg.NewBuilder(cpg.BuildConfig{Dir: root})

	db := openTestDB(t)
	cache, _ := cpg.NewGraphCache(db)
	pkgDir := filepath.Join(root, "cmd", "neo-mcp")

	g, _, err := builder.BuildCached("./cmd/neo-mcp", pkgDir, cache)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Measure gob encode into a bytes.Buffer (no pipe, no blocking).
	var buf bytes.Buffer
	encStart := time.Now()
	if err := gob.NewEncoder(&buf).Encode(g); err != nil {
		t.Fatalf("gob encode: %v", err)
	}
	encElapsed := time.Since(encStart)

	// Measure gob decode from the same buffer.
	var g2 cpg.Graph
	decStart := time.Now()
	if err := gob.NewDecoder(&buf).Decode(&g2); err != nil {
		t.Fatalf("gob decode: %v", err)
	}
	decElapsed := time.Since(decStart)

	t.Logf("gob encode: %v  decode: %v  (nodes=%d edges=%d)", encElapsed, decElapsed, len(g.Nodes), len(g.Edges))

	// Threshold relaxed from 20ms → 200ms — same reasoning as
	// TestCachedBuild: race detector adds 2-20× overhead. The
	// purpose is "gob round-trip is fast vs network/disk", not a
	// hard latency target.
	if encElapsed > 200*time.Millisecond {
		t.Errorf("gob encode took %v, expected <200ms", encElapsed)
	}
	if decElapsed > 200*time.Millisecond {
		t.Errorf("gob decode took %v, expected <200ms", decElapsed)
	}
}

// TestCacheInvalidation verifies that a stale mtime triggers a rebuild.
func TestCacheInvalidation(t *testing.T) {
	db := openTestDB(t)
	cache, _ := cpg.NewGraphCache(db)

	// Store a dummy graph under mtime=100.
	dummy := &cpg.Graph{
		Nodes: []cpg.Node{{Name: "dummy", Kind: cpg.NodeFunc}},
	}
	if err := cache.Put("./fake", 100, dummy); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Hit with mtime=100 → should return dummy.
	g, ok := cache.Get("./fake", 100)
	if !ok || len(g.Nodes) == 0 {
		t.Fatal("expected cache hit with mtime=100")
	}

	// Miss with mtime=200 (simulates a file change).
	_, ok = cache.Get("./fake", 200)
	if ok {
		t.Fatal("expected cache miss with stale mtime=200")
	}
}
