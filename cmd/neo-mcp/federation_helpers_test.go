package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// TestNexusDispatcherBase verifies that nexusDispatcherBase() correctly derives the
// Nexus root URL from NEO_EXTERNAL_URL. [Épica 266.D]
func TestNexusDispatcherBase_Empty(t *testing.T) {
	t.Setenv("NEO_EXTERNAL_URL", "")
	if got := nexusDispatcherBase(); got != "" {
		t.Errorf("expected empty string when NEO_EXTERNAL_URL unset, got %q", got)
	}
}

func TestNexusDispatcherBase_Valid(t *testing.T) {
	t.Setenv("NEO_EXTERNAL_URL", "http://127.0.0.1:9000/workspaces/backend-go-47293")
	got := nexusDispatcherBase()
	if got != "http://127.0.0.1:9000" {
		t.Errorf("expected %q, got %q", "http://127.0.0.1:9000", got)
	}
}

func TestNexusDispatcherBase_NoWorkspacesPath(t *testing.T) {
	// If NEO_EXTERNAL_URL has no /workspaces/ segment, return the value as-is fallback.
	t.Setenv("NEO_EXTERNAL_URL", "http://127.0.0.1:9000")
	got := nexusDispatcherBase()
	// Expect empty because no /workspaces/ found to split on.
	if got != "" {
		t.Errorf("expected empty when no /workspaces/ in URL, got %q", got)
	}
}

func TestNexusDispatcherBase_DifferentPort(t *testing.T) {
	t.Setenv("NEO_EXTERNAL_URL", "http://127.0.0.1:9001/workspaces/my-app-12345")
	got := nexusDispatcherBase()
	if got != "http://127.0.0.1:9001" {
		t.Errorf("expected %q, got %q", "http://127.0.0.1:9001", got)
	}
}

// TestResolveWorkspaceID verifies that resolveWorkspaceID() finds an entry by path. [Épica 266.D]
func TestResolveWorkspaceID_Found(t *testing.T) {
	dir := t.TempDir()
	wsPath := filepath.Join(dir, "my-service")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	reg := &workspace.Registry{}
	reg.Workspaces = []workspace.WorkspaceEntry{
		{ID: "my-service-12345", Path: filepath.Clean(wsPath), Name: "my-service", Type: "workspace"},
	}

	regFile := filepath.Join(dir, "workspaces.json")
	data, _ := json.Marshal(reg)
	if err := os.WriteFile(regFile, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Override HOME so LoadRegistry() reads our mock file.
	t.Setenv("HOME", dir)
	// The default path is ~/.neo/workspaces.json — create the dir.
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .neo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(neoDir, "workspaces.json"), data, 0o600); err != nil {
		t.Fatalf("WriteFile workspaces.json: %v", err)
	}

	id := resolveWorkspaceID(wsPath)
	if id != "my-service-12345" {
		t.Errorf("expected %q, got %q", "my-service-12345", id)
	}
}

func TestResolveWorkspaceID_NotFound(t *testing.T) {
	dir := t.TempDir()
	// Write empty registry.
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	reg := &workspace.Registry{Workspaces: []workspace.WorkspaceEntry{}}
	data, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(neoDir, "workspaces.json"), data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("HOME", dir)

	id := resolveWorkspaceID("/some/nonexistent/path")
	if id != "" {
		t.Errorf("expected empty string for unregistered path, got %q", id)
	}
}
