package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeOrgConfig(t *testing.T, root string, oc *OrgConfig) {
	t.Helper()
	if err := WriteOrgConfig(root, oc); err != nil {
		t.Fatal(err)
	}
}

// TestLoadOrgConfig_WalkUpFindsOrgRoot verifies discovery from a deeply
// nested workspace locates `.neo-org/neo.yaml` several parents up. [354.A]
func TestLoadOrgConfig_WalkUpFindsOrgRoot(t *testing.T) {
	root := t.TempDir()
	writeOrgConfig(t, root, &OrgConfig{
		OrgName:            "acme-dev",
		Projects:           []string{"/tmp/p1", "/tmp/p2"},
		CoordinatorProject: "p1",
	})

	// Workspace lives 3 levels deep under root.
	deep := filepath.Join(root, "projects", "backend-project", "workspaces", "api")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}

	oc, err := LoadOrgConfig(deep)
	if err != nil {
		t.Fatalf("LoadOrgConfig: %v", err)
	}
	if oc == nil {
		t.Fatal("expected org config, got nil")
	}
	if oc.OrgName != "acme-dev" {
		t.Errorf("OrgName = %q, want acme-dev", oc.OrgName)
	}
	if len(oc.Projects) != 2 {
		t.Errorf("Projects = %v, want 2 entries", oc.Projects)
	}
	if oc.CoordinatorTakeoverSeconds != 120 {
		t.Errorf("CoordinatorTakeoverSeconds = %d, want 120 (default)", oc.CoordinatorTakeoverSeconds)
	}
}

// TestLoadOrgConfig_MissingReturnsNil verifies (nil, nil) when no `.neo-org/`
// is found. [354.A]
func TestLoadOrgConfig_MissingReturnsNil(t *testing.T) {
	oc, err := LoadOrgConfig(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oc != nil {
		t.Errorf("expected nil, got %+v", oc)
	}
}

// TestFindNeoOrgDir_WalkUp verifies the directory-finding variant. [354.A]
func TestFindNeoOrgDir_WalkUp(t *testing.T) {
	root := t.TempDir()
	orgDir := filepath.Join(root, ".neo-org")
	if err := os.MkdirAll(orgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	_ = os.MkdirAll(deep, 0o750)

	got, ok := FindNeoOrgDir(deep)
	if !ok {
		t.Fatal("expected to find .neo-org via walk-up")
	}
	if got != orgDir {
		t.Errorf("FindNeoOrgDir = %q, want %q", got, orgDir)
	}

	// Absent case.
	if _, ok := FindNeoOrgDir(t.TempDir()); ok {
		t.Error("expected not found on empty dir")
	}
}

// TestIsCoordinatorProject_Matches verifies exact + basename matching. [354.A]
func TestIsCoordinatorProject_Matches(t *testing.T) {
	org := &OrgConfig{CoordinatorProject: "strategos-project"}
	if !IsCoordinatorProject("/home/user/projects/my-project", org) {
		t.Error("basename match failed")
	}
	if IsCoordinatorProject("/path/to/project-project", org) {
		t.Error("non-match should return false")
	}

	// Absolute-path match.
	absOrg := &OrgConfig{CoordinatorProject: "/abs/path/project-x"}
	if !IsCoordinatorProject("/abs/path/project-x", absOrg) {
		t.Error("exact abs match failed")
	}

	// Legacy mode: no coordinator → everyone claims.
	legacy := &OrgConfig{CoordinatorProject: ""}
	if !IsCoordinatorProject("/anything", legacy) {
		t.Error("legacy mode should return true for any project")
	}

	// Nil org → coordinator (edge case — short-circuit).
	if !IsCoordinatorProject("/anything", nil) {
		t.Error("nil org should return true")
	}
}

// TestMergeConfigsWithOrg_OrgFillsGap verifies org LLMOverrides apply only
// when the corresponding workspace field is empty AND project didn't set. [354.A]
func TestMergeConfigsWithOrg_OrgFillsGap(t *testing.T) {
	workspace := NeoConfig{}
	// workspace.AI left empty — org should fill it
	org := &OrgConfig{
		OrgName: "org-x",
		LLMOverrides: &LLMOverrides{
			EmbeddingModel: "nomic-embed-text",
			InferenceMode:  "hybrid",
		},
	}

	merged := MergeConfigsWithOrg(workspace, nil, org, nil)
	if merged.AI.EmbeddingModel != "nomic-embed-text" {
		t.Errorf("org LLM did not fill gap: %q", merged.AI.EmbeddingModel)
	}
	if merged.Inference.Mode != "hybrid" {
		t.Errorf("org inference mode did not fill gap: %q", merged.Inference.Mode)
	}
	if merged.Org == nil || merged.Org.OrgName != "org-x" {
		t.Errorf("Org field not set on merged: %+v", merged.Org)
	}
}

// TestMergeConfigsWithOrg_ProjectWinsOverOrg verifies project LLMOverrides
// take precedence when both project and org set the same field. [354.A]
func TestMergeConfigsWithOrg_ProjectWinsOverOrg(t *testing.T) {
	workspace := NeoConfig{}
	project := &ProjectConfig{
		ProjectName: "p",
		LLMOverrides: &LLMOverrides{
			EmbeddingModel: "project-model",
		},
	}
	org := &OrgConfig{
		LLMOverrides: &LLMOverrides{
			EmbeddingModel: "org-model",
			InferenceMode:  "cloud",
		},
	}
	merged := MergeConfigsWithOrg(workspace, project, org, nil)
	if merged.AI.EmbeddingModel != "project-model" {
		t.Errorf("project did not win over org: %q", merged.AI.EmbeddingModel)
	}
	// Org's InferenceMode fills gap (project didn't set).
	if merged.Inference.Mode != "cloud" {
		t.Errorf("org failed to fill gap where project didn't override: %q", merged.Inference.Mode)
	}
}

// TestMergeConfigsWithOrg_NilOrgNoOp verifies nil org doesn't break the merge. [354.A]
func TestMergeConfigsWithOrg_NilOrgNoOp(t *testing.T) {
	workspace := NeoConfig{}
	workspace.AI.EmbeddingModel = "ws-model"
	merged := MergeConfigsWithOrg(workspace, nil, nil, nil)
	if merged.AI.EmbeddingModel != "ws-model" {
		t.Errorf("nil org should not modify workspace")
	}
	if merged.Org != nil {
		t.Error("merged.Org should be nil when no org provided")
	}
}

// TestWriteOrgConfig_RoundTrip verifies persist → load retains fields. [354.A]
func TestWriteOrgConfig_RoundTrip(t *testing.T) {
	root := t.TempDir()
	original := &OrgConfig{
		OrgName:                    "round-trip",
		Projects:                   []string{"/p/a", "/p/b"},
		CoordinatorProject:         "a",
		CoordinatorTakeoverSeconds: 60,
		DirectivesPath:             ".neo-org/CUSTOM_DIRECTIVES.md",
	}
	if err := WriteOrgConfig(root, original); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadOrgConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OrgName != original.OrgName ||
		len(loaded.Projects) != 2 ||
		loaded.CoordinatorProject != "a" ||
		loaded.CoordinatorTakeoverSeconds != 60 ||
		loaded.DirectivesPath != ".neo-org/CUSTOM_DIRECTIVES.md" {
		t.Errorf("round-trip mismatch: %+v", loaded)
	}
}
