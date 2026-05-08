package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeedProjectArtifacts_WritesSkeleton verifies that initProject creates
// SHARED_DEBT.md + knowledge/.gitkeep when absent, idempotent on re-run. [363.A]
func TestSeedProjectArtifacts_WritesSkeleton(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".neo-project"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := seedProjectArtifacts(root, "my-federation"); err != nil {
		t.Fatalf("seedProjectArtifacts: %v", err)
	}

	debt, err := os.ReadFile(filepath.Join(root, ".neo-project", "SHARED_DEBT.md"))
	if err != nil {
		t.Fatalf("SHARED_DEBT.md not created: %v", err)
	}
	if !strings.Contains(string(debt), "my-federation") {
		t.Errorf("SHARED_DEBT.md missing project name: %s", debt)
	}
	if !strings.Contains(string(debt), "## P0 — Blocker") {
		t.Errorf("SHARED_DEBT.md missing priority sections: %s", debt)
	}
	if _, err := os.Stat(filepath.Join(root, ".neo-project", "knowledge", ".gitkeep")); err != nil {
		t.Errorf("knowledge/.gitkeep not created: %v", err)
	}

	// Idempotency: re-run should not error or modify.
	before, _ := os.ReadFile(filepath.Join(root, ".neo-project", "SHARED_DEBT.md"))
	if err := seedProjectArtifacts(root, "my-federation"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(root, ".neo-project", "SHARED_DEBT.md"))
	if string(before) != string(after) {
		t.Error("seedProjectArtifacts is not idempotent — re-run modified existing file")
	}
}

// TestFindProjectDir_WalkUp verifies the walk-up helper finds .neo-project even
// when called from a nested workspace. [363.A]
func TestFindProjectDir_WalkUp(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, ".neo-project")
	if err := os.MkdirAll(projDir, 0o750); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "workspaces", "service-a")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}

	got := findProjectDir(nested)
	if got != projDir {
		t.Errorf("findProjectDir(%s) = %q, want %q", nested, got, projDir)
	}

	// Empty when no .neo-project exists.
	other := t.TempDir()
	if got := findProjectDir(other); got != "" {
		t.Errorf("expected empty on no-project root, got %q", got)
	}
}
