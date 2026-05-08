package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/auth"
)

// writeContextStore is a test helper that persists a ContextStore to a temp
// HOME so currentContext() reads it back via auth.DefaultContextsPath.
func writeContextStore(t *testing.T, home string, store *auth.ContextStore) {
	t.Helper()
	t.Setenv("HOME", home)
	if err := auth.SaveContexts(store, auth.DefaultContextsPath()); err != nil {
		t.Fatalf("SaveContexts: %v", err)
	}
}

// TestCurrentSpace_CacheHit_NoDiskRead verifies that within the TTL window,
// the helper does NOT re-read contexts.json. We assert this by deleting the
// file after first call and checking that second call still returns the
// cached value. [142.3a]
func TestCurrentSpace_CacheHit_NoDiskRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeContextStore(t, home, &auth.ContextStore{
		Contexts: []auth.Space{{Provider: "jira", SpaceID: "MCPI", SpaceName: "MCP Integration"}},
		Active:   map[string]string{"jira": "MCPI"},
	})

	s := &state{activeSpace: "FALLBACK", activeBoard: "BOARD-FALLBACK"}
	first := s.currentSpace()
	if first != "MCPI" {
		t.Fatalf("first call expected MCPI, got %q", first)
	}

	// Wipe the file to prove next call doesn't touch disk.
	if err := os.Remove(filepath.Join(home, ".neo", "contexts.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	second := s.currentSpace()
	if second != "MCPI" {
		t.Errorf("cache miss: expected MCPI from cache, got %q", second)
	}
}

// TestCurrentSpace_StaleReload_PicksUpChanges verifies that after the TTL,
// the helper re-reads contexts.json — so `neo space use` changes propagate
// without restarting the plugin. [142.3b + 142.3d]
func TestCurrentSpace_StaleReload_PicksUpChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeContextStore(t, home, &auth.ContextStore{
		Contexts: []auth.Space{{Provider: "jira", SpaceID: "ORIGINAL"}},
		Active:   map[string]string{"jira": "ORIGINAL"},
	})

	s := &state{activeSpace: "FALLBACK"}
	if got := s.currentSpace(); got != "ORIGINAL" {
		t.Fatalf("first call expected ORIGINAL, got %q", got)
	}

	// Operator runs `neo space use --provider jira --id NEW`. We simulate
	// by overwriting contexts.json + forcing the cache to be stale by
	// rewinding contextsLoadedAt past the TTL.
	writeContextStore(t, home, &auth.ContextStore{
		Contexts: []auth.Space{{Provider: "jira", SpaceID: "NEW"}},
		Active:   map[string]string{"jira": "NEW"},
	})
	s.mu.Lock()
	s.contextsLoadedAt = time.Now().Add(-2 * contextsCacheTTL)
	s.mu.Unlock()

	if got := s.currentSpace(); got != "NEW" {
		t.Errorf("post-stale reload expected NEW, got %q", got)
	}
}

// TestCurrentSpace_MissingFile_FallsBackToEnv verifies that when
// contexts.json doesn't exist (fresh setup, never ran `neo space use`), the
// helper returns the env-var value captured at boot. [142.3c]
func TestCurrentSpace_MissingFile_FallsBackToEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No contexts.json written — auth.LoadContexts should error or return empty.

	s := &state{activeSpace: "ENV-FALLBACK", activeBoard: "BOARD-ENV"}
	if got := s.currentSpace(); got != "ENV-FALLBACK" {
		t.Errorf("missing file: expected env fallback ENV-FALLBACK, got %q", got)
	}
	if got := s.currentBoard(); got != "BOARD-ENV" {
		t.Errorf("missing file: expected env fallback BOARD-ENV, got %q", got)
	}
}

// TestCurrentBoard_PrefersContextsBoard verifies that when contexts.json has
// both space + board for the active jira context, currentBoard returns the
// stored board (not the env fallback).
func TestCurrentBoard_PrefersContextsBoard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeContextStore(t, home, &auth.ContextStore{
		Contexts: []auth.Space{{Provider: "jira", SpaceID: "MCPI", BoardID: "15"}},
		Active:   map[string]string{"jira": "MCPI"},
	})

	s := &state{activeSpace: "OLDFALL", activeBoard: "OLDBOARD"}
	if got := s.currentBoard(); got != "15" {
		t.Errorf("expected board 15 from contexts, got %q", got)
	}
}

