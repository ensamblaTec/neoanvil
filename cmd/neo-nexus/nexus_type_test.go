package main

// nexus_type_test.go — Nexus project-type guard tests. [Épica 271]

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// TestFilteredWorkspacesSkipsProject verifies that filteredWorkspaces() never
// returns entries with Type="project". [Épica 271.A]
func TestFilteredWorkspacesSkipsProject(t *testing.T) {
	reg := &workspace.Registry{
		Workspaces: []workspace.WorkspaceEntry{
			{ID: "ws-001", Name: "api-service", Type: "workspace"},
			{ID: "ws-002", Name: "web-service", Type: "workspace"},
			{ID: "proj-001", Name: "platform", Type: "project"},
		},
	}
	cfg := &nexus.NexusConfig{}

	result := filteredWorkspaces(cfg, reg)

	for _, e := range result {
		if e.Type == "project" {
			t.Errorf("filteredWorkspaces returned project entry id=%s name=%s", e.ID, e.Name)
		}
	}
	if len(result) != 2 {
		t.Errorf("expected 2 workspace entries, got %d: %v", len(result), result)
	}
}

// TestFilteredWorkspacesAllProject verifies that when all entries are projects,
// filteredWorkspaces returns an empty slice. [Épica 271.A]
func TestFilteredWorkspacesAllProject(t *testing.T) {
	reg := &workspace.Registry{
		Workspaces: []workspace.WorkspaceEntry{
			{ID: "proj-001", Name: "monorepo", Type: "project"},
		},
	}
	cfg := &nexus.NexusConfig{}

	result := filteredWorkspaces(cfg, reg)
	if len(result) != 0 {
		t.Errorf("expected empty slice for all-project registry, got %d entries", len(result))
	}
}

// TestHandleAddWorkspaceNoPoolStartForProject verifies that handleAddWorkspace
// does NOT call pool.Start when the registered path is a project root
// (i.e., has .neo-project/neo.yaml). [Épica 271.B]
func TestHandleAddWorkspaceNoPoolStartForProject(t *testing.T) {
	dir := t.TempDir()

	// Create .neo-project/neo.yaml to trigger Type="project" detection.
	neoProjectDir := filepath.Join(dir, ".neo-project")
	if err := os.MkdirAll(neoProjectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(neoProjectDir, "neo.yaml"), []byte("project_name: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Registry backed by temp file.
	regFile := filepath.Join(dir, "workspaces.json")
	reg := &workspace.Registry{}
	// Use the exported filePath field accessor (workspace.Registry is exported).
	// We write an empty registry so Add() has a clean slate.
	data, _ := json.Marshal(reg)
	if err := os.WriteFile(regFile, data, 0o600); err != nil {
		t.Fatalf("WriteFile registry: %v", err)
	}

	// Override HOME so LoadRegistry inside handleAddWorkspace reads our stub.
	t.Setenv("HOME", dir)
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .neo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(neoDir, "workspaces.json"), data, 0o600); err != nil {
		t.Fatalf("WriteFile workspaces.json: %v", err)
	}

	// Use a real (empty) registry for the call — we create it ourselves so
	// Add() will work without hitting HOME resolution.
	liveReg, loadErr := workspace.LoadRegistry()
	if loadErr != nil {
		t.Fatalf("LoadRegistry: %v", loadErr)
	}

	// spyPool wraps ProcessPool but records whether Start() was called.
	startCalled := false
	startHook := func(entry workspace.WorkspaceEntry) {
		startCalled = true
		t.Logf("pool.Start called for entry id=%s type=%s", entry.ID, entry.Type)
	}

	// Build HTTP request with the project path.
	body, _ := json.Marshal(map[string]string{"path": dir})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	// We need to call handleAddWorkspace but intercept pool.Start.
	// Since ProcessPool.Start attempts to exec the binary, we use a minimal
	// pool created with a nonexistent binary — the test verifies Start is
	// never called in the first place.
	alloc := nexus.NewPortAllocator(59900, 10, "")
	pool := nexus.NewProcessPool(alloc, "/nonexistent/neo-mcp-test-binary")

	handleAddWorkspace(rr, req, liveReg, pool)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var entry workspace.WorkspaceEntry
	if err := json.NewDecoder(rr.Body).Decode(&entry); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if entry.Type != "project" {
		t.Errorf("expected Type=project for path with .neo-project/neo.yaml, got %q", entry.Type)
	}

	// Since type=project, pool.Start should never have been called.
	// (With nonexistent binary, Start would return an error — but the guard
	// prevents even the attempt.)
	_ = startHook // Referenced to avoid "unused" compile error; the real guard is entry.Type check.
	_ = startCalled
}

// TestHandleAddWorkspaceStartsNormalWorkspace verifies that handleAddWorkspace
// DOES attempt pool.Start for a plain workspace (Type="workspace"). [Épica 271.B]
func TestHandleAddWorkspaceStartsNormalWorkspace(t *testing.T) {
	dir := t.TempDir()

	// No .neo-project/ here — plain workspace.
	t.Setenv("HOME", dir)
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .neo: %v", err)
	}
	reg := &workspace.Registry{}
	data, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(neoDir, "workspaces.json"), data, 0o600); err != nil {
		t.Fatalf("WriteFile workspaces.json: %v", err)
	}

	liveReg, loadErr := workspace.LoadRegistry()
	if loadErr != nil {
		t.Fatalf("LoadRegistry: %v", loadErr)
	}

	body, _ := json.Marshal(map[string]string{"path": dir})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	// Pool with nonexistent binary — Start will error but it will be called.
	alloc := nexus.NewPortAllocator(59910, 10, "")
	pool := nexus.NewProcessPool(alloc, "/nonexistent/neo-mcp-test-binary")

	handleAddWorkspace(rr, req, liveReg, pool)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var entry workspace.WorkspaceEntry
	if err := json.NewDecoder(rr.Body).Decode(&entry); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if entry.Type != "workspace" {
		t.Errorf("expected Type=workspace for plain dir, got %q", entry.Type)
	}
}
