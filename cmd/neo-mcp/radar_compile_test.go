package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resetSymbolMapCache clears the package-level cache to a known state
// for each test case. Subtests cannot run in parallel because they all
// share the same package var. [Phase 0.D / Speed-First]
func resetSymbolMapCache(t *testing.T) {
	t.Helper()
	symbolMapCacheMu.Lock()
	symbolMapCache = map[string]map[string]int{}
	symbolMapCacheMu.Unlock()
}

// TestSymbolMapSnapshot_RoundTrip is the Phase 0.D regression: write the
// in-memory cache to disk, wipe it, load it back, and assert the contents
// match. Without persistence, COMPILE_AUDIT pays the ~50ms go/ast parse on
// every package after each `make rebuild-restart`.
func TestSymbolMapSnapshot_RoundTrip(t *testing.T) {
	resetSymbolMapCache(t)
	defer resetSymbolMapCache(t)

	path := filepath.Join(t.TempDir(), "symbol_map.snapshot.json")

	// Seed two packages, three symbols total.
	symbolMapCacheMu.Lock()
	symbolMapCache["/abs/pkg/foo@123@true"] = map[string]int{"Bar": 10, "Baz": 20}
	symbolMapCache["/abs/pkg/qux@456@false"] = map[string]int{"Quux": 30}
	symbolMapCacheMu.Unlock()

	saved, err := saveSymbolMapSnapshot(path)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved != 2 {
		t.Errorf("saved=%d want 2 packages", saved)
	}
	if info, statErr := os.Stat(path); statErr != nil {
		t.Errorf("snapshot file missing on disk: %v", statErr)
	} else if info.Size() == 0 {
		t.Error("snapshot file empty")
	}

	// Wipe the in-memory cache so the load is the only source of truth.
	resetSymbolMapCache(t)

	loaded, err := loadSymbolMapSnapshot(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded != 2 {
		t.Errorf("loaded=%d want 2", loaded)
	}

	symbolMapCacheMu.RLock()
	foo := symbolMapCache["/abs/pkg/foo@123@true"]
	qux := symbolMapCache["/abs/pkg/qux@456@false"]
	symbolMapCacheMu.RUnlock()

	if foo == nil || foo["Bar"] != 10 || foo["Baz"] != 20 {
		t.Errorf("pkg/foo after reload = %v, want Bar:10 Baz:20", foo)
	}
	if qux == nil || qux["Quux"] != 30 {
		t.Errorf("pkg/qux after reload = %v, want Quux:30", qux)
	}
}

// TestSymbolMapSnapshot_MissingFile_NoError covers cold boot: the snapshot
// path doesn't exist yet, load must return (0, nil) — not an error — so
// setupCaches' warm-load log line stays quiet on fresh workspaces.
func TestSymbolMapSnapshot_MissingFile_NoError(t *testing.T) {
	resetSymbolMapCache(t)
	defer resetSymbolMapCache(t)

	n, err := loadSymbolMapSnapshot(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0 on missing file", n)
	}
}

// TestSymbolMapSnapshot_VersionMismatch_NoCrash protects future schema bumps:
// if the on-disk snapshot has a version we don't recognise, load is a no-op
// (the next save will overwrite with the current version) — boot must never
// fail on a stale snapshot left over from a prior binary.
func TestSymbolMapSnapshot_VersionMismatch_NoCrash(t *testing.T) {
	resetSymbolMapCache(t)
	defer resetSymbolMapCache(t)

	path := filepath.Join(t.TempDir(), "symbol_map.snapshot.json")
	// version 999 doesn't match symbolMapSnapshotVersion (1).
	if err := os.WriteFile(path, []byte(`{"version":999,"entries":{"x":{"Y":1}}}`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := loadSymbolMapSnapshot(path)
	if err != nil {
		t.Errorf("version mismatch should not error: %v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0 on version mismatch", n)
	}

	symbolMapCacheMu.RLock()
	defer symbolMapCacheMu.RUnlock()
	if _, has := symbolMapCache["x"]; has {
		t.Error("version-mismatched entries must NOT be loaded into the cache")
	}
}

// TestSymbolMapSnapshot_CorruptJSON_ReturnsError ensures we don't silently
// drop a corrupt snapshot — the caller (setupCaches) logs the error and
// continues cold, but we must distinguish corruption from cold-boot.
func TestSymbolMapSnapshot_CorruptJSON_ReturnsError(t *testing.T) {
	resetSymbolMapCache(t)
	defer resetSymbolMapCache(t)

	path := filepath.Join(t.TempDir(), "symbol_map.snapshot.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := loadSymbolMapSnapshot(path)
	if err == nil {
		t.Error("corrupt JSON must surface an error so setupCaches can log it")
	}
	if n != 0 {
		t.Errorf("n=%d want 0 on corrupt JSON", n)
	}
}
