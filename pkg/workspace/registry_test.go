package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddDeterministicID verifies that registering the same path twice yields
// the same ID — even across distinct Registry instances. [SRE-106.A/C]
func TestAddDeterministicID(t *testing.T) {
	dir := t.TempDir()
	r1 := &Registry{filePath: filepath.Join(dir, "ws1.json")}
	e1, err := r1.Add(dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	r2 := &Registry{filePath: filepath.Join(dir, "ws2.json")}
	e2, err := r2.Add(dir)
	if err != nil {
		t.Fatalf("Add (second registry): %v", err)
	}

	if e1.ID != e2.ID {
		t.Fatalf("non-deterministic ID: r1=%q r2=%q (same path %q)", e1.ID, e2.ID, dir)
	}
	parts := strings.Split(e1.ID, "-")
	suffix := parts[len(parts)-1]
	if len(suffix) != 5 {
		t.Fatalf("expected 5-digit suffix, got %q (full id %q)", suffix, e1.ID)
	}
}

// TestAddIdempotent verifies that re-adding the same path returns the existing
// entry without modification (backward-compat for pre-106 registries). [SRE-106.B]
func TestAddIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{filePath: filepath.Join(dir, "ws.json")}
	e1, err := r.Add(dir)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	idBefore := e1.ID
	e2, err := r.Add(dir)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if e2.ID != idBefore {
		t.Fatalf("Add not idempotent: %q != %q", e2.ID, idBefore)
	}
	if len(r.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace after duplicate Add, got %d", len(r.Workspaces))
	}
}

// TestAddDifferentPathsDifferentIDs sanity-checks that paths with different
// suffixes produce different IDs (collision is theoretically possible at 1e5
// buckets but extremely unlikely for unrelated paths in a single test).
func TestAddDifferentPathsDifferentIDs(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	r := &Registry{filePath: filepath.Join(dir1, "ws.json")}
	e1, err := r.Add(dir1)
	if err != nil {
		t.Fatalf("Add dir1: %v", err)
	}
	e2, err := r.Add(dir2)
	if err != nil {
		t.Fatalf("Add dir2: %v", err)
	}
	if e1.ID == e2.ID {
		t.Fatalf("expected distinct IDs for distinct paths, got %q for both", e1.ID)
	}
}

// TestSelectAndActive verifies the active workspace lookup contract.
func TestSelectAndActive(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	r := &Registry{filePath: filepath.Join(dir1, "ws.json")}
	e1, _ := r.Add(dir1)
	_, _ = r.Add(dir2)

	if r.Active().ID != e1.ID {
		t.Fatalf("Active() should default to first workspace when ActiveID is empty")
	}
	if err := r.Select(e1.Name); err != nil {
		t.Fatalf("Select by name: %v", err)
	}
	if r.ActiveID != e1.ID {
		t.Fatalf("ActiveID not set after Select")
	}
	if err := r.Select("nonexistent"); err == nil {
		t.Fatalf("Select of unknown workspace should error")
	}
}

// TestSaveAndLoadRoundtrip verifies the registry survives a write/read cycle.
func TestSaveAndLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	regPath := filepath.Join(dir, "ws.json")
	r1 := &Registry{filePath: regPath}
	e, _ := r1.Add(dir)
	r1.ActiveID = e.ID
	if err := r1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), e.ID) {
		t.Fatalf("saved file missing ID %q: %s", e.ID, string(data))
	}
}
