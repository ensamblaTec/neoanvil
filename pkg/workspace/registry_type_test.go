package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRegistryTypeDetection verifies Add() sets Type="project" when .neo-project/neo.yaml
// exists, and Type="workspace" for regular directories. [Épica 266.C]
func TestRegistryTypeDetection(t *testing.T) {
	dir := t.TempDir()

	// Plain workspace — no .neo-project/
	r := &Registry{filePath: filepath.Join(dir, "ws.json")}
	wsDir := filepath.Join(dir, "my-workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	e, err := r.Add(wsDir)
	if err != nil {
		t.Fatalf("Add workspace: %v", err)
	}
	if e.Type != "workspace" {
		t.Errorf("plain dir Type = %q, want %q", e.Type, "workspace")
	}

	// Project root — has .neo-project/neo.yaml
	projDir := filepath.Join(dir, "my-project")
	neoProjectDir := filepath.Join(projDir, ".neo-project")
	if err := os.MkdirAll(neoProjectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll neo-project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(neoProjectDir, "neo.yaml"), []byte("project_name: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile neo.yaml: %v", err)
	}
	pe, err := r.Add(projDir)
	if err != nil {
		t.Fatalf("Add project: %v", err)
	}
	if pe.Type != "project" {
		t.Errorf("project dir Type = %q, want %q", pe.Type, "project")
	}
}

// TestRegistryTypeIdempotent verifies that re-adding an already-registered path
// preserves the original Type (no re-detection on second Add). [Épica 266.C]
func TestRegistryTypeIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{filePath: filepath.Join(dir, "ws.json")}

	// Register as plain workspace first.
	e1, err := r.Add(dir)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if e1.Type != "workspace" {
		t.Errorf("initial Type = %q, want workspace", e1.Type)
	}

	// Add again — should return same entry, same Type.
	e2, err := r.Add(dir)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if e2.ID != e1.ID {
		t.Errorf("idempotent Add changed ID: %q → %q", e1.ID, e2.ID)
	}
	if e2.Type != "workspace" {
		t.Errorf("Type changed on re-Add: %q", e2.Type)
	}
}
