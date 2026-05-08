package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProjectFederation verifies that a project config with member workspaces
// correctly merges into the workspace config and exposes all members. [Épica 261.A]
func TestProjectFederation(t *testing.T) {
	dir := t.TempDir()

	pc := &ProjectConfig{
		ProjectName:      "acme-platform",
		MemberWorkspaces: []string{"./services/api", "./services/frontend"},
		DominantLang:     "go",
	}
	if err := WriteProjectConfig(dir, pc); err != nil {
		t.Fatalf("WriteProjectConfig: %v", err)
	}

	loaded, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("LoadProjectConfig: %v", err)
	}
	if loaded.ProjectName != "acme-platform" {
		t.Errorf("ProjectName = %q, want %q", loaded.ProjectName, "acme-platform")
	}
	if len(loaded.MemberWorkspaces) != 2 {
		t.Errorf("MemberWorkspaces len = %d, want 2", len(loaded.MemberWorkspaces))
	}

	projFile := filepath.Join(dir, ".neo-project", "neo.yaml")
	if _, statErr := os.Stat(projFile); statErr != nil {
		t.Errorf("expected .neo-project/neo.yaml at %s: %v", projFile, statErr)
	}
}

// TestProjectFederationMerge verifies MergeConfigs picks up project overrides. [Épica 261.A]
func TestProjectFederationMerge(t *testing.T) {
	basePtr := defaultNeoConfig()
	base := *basePtr
	base.Workspace.IgnoreDirs = []string{"vendor"}

	proj := &ProjectConfig{
		ProjectName:      "test-proj",
		MemberWorkspaces: []string{".", "./svc2"},
		DominantLang:     "typescript",
		IgnoreDirsAdd:    []string{"node_modules"},
	}

	merged := MergeConfigs(base, proj, nil)
	if merged.Workspace.DominantLang != "typescript" {
		t.Errorf("DominantLang = %q, want %q", merged.Workspace.DominantLang, "typescript")
	}
	dirs := merged.Workspace.IgnoreDirs
	found := false
	for _, d := range dirs {
		if d == "node_modules" {
			found = true
		}
	}
	if !found {
		t.Errorf("IgnoreDirs after merge = %v, expected node_modules", dirs)
	}
	if merged.Project != proj {
		t.Errorf("Project pointer not set on merged config")
	}
}
