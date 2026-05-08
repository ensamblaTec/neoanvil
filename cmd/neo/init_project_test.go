package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// TestInitProjectCmdWritesConfig verifies that initProjectCmd creates
// .neo-project/neo.yaml with the given members and project name. [Épica 269.D]
func TestInitProjectCmdWritesConfig(t *testing.T) {
	root := t.TempDir()

	// Create two mock workspace dirs with neo.yaml so detectWorkspaces finds them.
	for _, sub := range []string{"service-a", "service-b"} {
		subDir := filepath.Join(root, sub)
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "neo.yaml"), []byte("ai:\n  provider: ollama\n"), 0o644); err != nil {
			t.Fatalf("WriteFile neo.yaml: %v", err)
		}
	}

	cmd := initProjectCmd()
	cmd.SetArgs([]string{"--dir", root, "--name", "test-platform", "--lang", "go"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("initProjectCmd.Execute(): %v", err)
	}

	// Verify .neo-project/neo.yaml was created.
	yamlPath := filepath.Join(root, ".neo-project", "neo.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf("expected .neo-project/neo.yaml to exist: %v", err)
	}

	// Verify contents via LoadProjectConfig.
	pc, err := config.LoadProjectConfig(root)
	if err != nil {
		t.Fatalf("LoadProjectConfig: %v", err)
	}
	if pc.ProjectName != "test-platform" {
		t.Errorf("project_name = %q, want %q", pc.ProjectName, "test-platform")
	}
	if pc.DominantLang != "go" {
		t.Errorf("dominant_lang = %q, want %q", pc.DominantLang, "go")
	}
	if len(pc.MemberWorkspaces) == 0 {
		t.Error("expected member_workspaces to be non-empty")
	}
}

// TestInitProjectCmdDryRun verifies that --dry-run does not write any files. [Épica 269.D]
func TestInitProjectCmdDryRun(t *testing.T) {
	root := t.TempDir()

	for _, sub := range []string{"svc-x"} {
		subDir := filepath.Join(root, sub)
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "neo.yaml"), []byte("ai:\n  provider: ollama\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	cmd := initProjectCmd()
	cmd.SetArgs([]string{"--dir", root, "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("initProjectCmd.Execute(): %v", err)
	}

	// Verify nothing was written.
	if _, err := os.Stat(filepath.Join(root, ".neo-project", "neo.yaml")); !os.IsNotExist(err) {
		t.Error("--dry-run should not create .neo-project/neo.yaml")
	}
}

// TestInitProjectCmdExplicitMembers verifies --members flag bypasses auto-detection. [Épica 269.D]
func TestInitProjectCmdExplicitMembers(t *testing.T) {
	root := t.TempDir()
	memberDir := filepath.Join(root, "explicit-svc")
	if err := os.MkdirAll(memberDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	cmd := initProjectCmd()
	cmd.SetArgs([]string{
		"--dir", root,
		"--name", "explicit-project",
		"--members", memberDir,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("initProjectCmd.Execute(): %v", err)
	}

	pc, err := config.LoadProjectConfig(root)
	if err != nil {
		t.Fatalf("LoadProjectConfig: %v", err)
	}
	if len(pc.MemberWorkspaces) != 1 {
		t.Errorf("expected 1 member, got %d: %v", len(pc.MemberWorkspaces), pc.MemberWorkspaces)
	}
	// Member should be stored as relative path.
	if !strings.Contains(pc.MemberWorkspaces[0], "explicit-svc") {
		t.Errorf("member path %q should contain 'explicit-svc'", pc.MemberWorkspaces[0])
	}
}
