package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadProjectConfigWalkUp verifies that LoadProjectConfig walks up the directory
// tree to find .neo-project/neo.yaml in a parent directory. [Épica 266.B]
func TestLoadProjectConfigWalkUp(t *testing.T) {
	root := t.TempDir()

	// Create .neo-project/neo.yaml in root.
	neoDir := filepath.Join(root, ".neo-project")
	if err := os.MkdirAll(neoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := WriteProjectConfig(root, &ProjectConfig{
		ProjectName:      "platform",
		MemberWorkspaces: []string{"./services/api", "./services/web"},
		DominantLang:     "go",
	}); err != nil {
		t.Fatalf("WriteProjectConfig: %v", err)
	}

	// Call from root/a/b/c — three levels deep.
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll deep: %v", err)
	}

	pc, err := LoadProjectConfig(deep)
	if err != nil {
		t.Fatalf("LoadProjectConfig from deep: %v", err)
	}
	if pc == nil {
		t.Fatal("expected project config found via walk-up, got nil")
	}
	if pc.ProjectName != "platform" {
		t.Errorf("ProjectName = %q, want platform", pc.ProjectName)
	}
	if len(pc.MemberWorkspaces) != 2 {
		t.Errorf("MemberWorkspaces = %v, want 2", pc.MemberWorkspaces)
	}
}

// TestLoadProjectConfigNotFoundDeep verifies nil,nil when no .neo-project/ exists
// anywhere up to the walk-up limit. [Épica 266.B]
func TestLoadProjectConfigNotFoundDeep(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "x", "y", "z")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	pc, err := LoadProjectConfig(deep)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if pc != nil {
		t.Errorf("expected nil, got %+v", pc)
	}
}

// TestMergeConfigsWorkspaceNotOverriddenByEmpty verifies that a non-empty workspace
// DominantLang is NOT overwritten when project DominantLang is empty. [Épica 266.A]
func TestMergeConfigsWorkspaceNotOverriddenByEmpty(t *testing.T) {
	ws := NeoConfig{}
	ws.Workspace.DominantLang = "rust"
	ws.Workspace.IgnoreDirs = []string{"target"}

	proj := &ProjectConfig{
		ProjectName:   "rust-app",
		DominantLang:  "", // empty — must NOT override
		IgnoreDirsAdd: []string{"tmp"},
	}

	merged := MergeConfigs(ws, proj, nil)
	if merged.Workspace.DominantLang != "rust" {
		t.Errorf("DominantLang should remain %q, got %q", "rust", merged.Workspace.DominantLang)
	}
	// IgnoreDirs must include both original + addition.
	found := false
	for _, d := range merged.Workspace.IgnoreDirs {
		if d == "tmp" {
			found = true
		}
	}
	if !found {
		t.Errorf("IgnoreDirs missing project addition 'tmp': %v", merged.Workspace.IgnoreDirs)
	}
}
