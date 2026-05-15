package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectConfigMerge verifies MergeConfigs applies project overrides correctly. [Épica 258.D]
// Semantic update 2026-05-15: workspace.DominantLang explicit wins over
// project.DominantLang. Previously project beat workspace, which broke
// polyglot projects (e.g. strategos backend in Go + strategosia-frontend
// in TypeScript under one project): the project's "go" forced IndexCoverage
// to count .go files in a TypeScript workspace → permanent RAG 0% false
// alarm. Now project.DominantLang is a DEFAULT for workspaces that don't
// set their own.
func TestProjectConfigMerge(t *testing.T) {
	workspace := NeoConfig{}
	workspace.Workspace.IgnoreDirs = []string{".git", "vendor"}
	workspace.Workspace.DominantLang = "go"

	project := &ProjectConfig{
		ProjectName:   "test-project",
		DominantLang:  "python",
		IgnoreDirsAdd: []string{"runs", "data"},
	}

	result := MergeConfigs(workspace, project, nil)

	// New semantic: workspace explicit DominantLang ("go") wins over project's "python".
	if result.Workspace.DominantLang != "go" {
		t.Errorf("explicit workspace DominantLang must win over project default: got %q, want go", result.Workspace.DominantLang)
	}

	// IgnoreDirs must contain both workspace and project additions, deduplicated.
	dirSet := make(map[string]bool)
	for _, d := range result.Workspace.IgnoreDirs {
		dirSet[d] = true
	}
	for _, want := range []string{".git", "vendor", "runs", "data"} {
		if !dirSet[want] {
			t.Errorf("IgnoreDirs missing %q: %v", want, result.Workspace.IgnoreDirs)
		}
	}

	// Project pointer must be set.
	if result.Project == nil {
		t.Error("Project field not set after MergeConfigs")
	}
}

// TestProjectConfigMerge_EmptyWorkspaceLangGetsProjectDefault is the
// companion to the polyglot semantic fix: when a workspace doesn't set
// its own DominantLang, the project's value fills in (default-provider
// behavior, the legitimate use-case for project-tier DominantLang).
func TestProjectConfigMerge_EmptyWorkspaceLangGetsProjectDefault(t *testing.T) {
	workspace := NeoConfig{} // .Workspace.DominantLang = "" (unset)

	project := &ProjectConfig{
		ProjectName:  "polyglot-project",
		DominantLang: "rust",
	}

	result := MergeConfigs(workspace, project, nil)

	if result.Workspace.DominantLang != "rust" {
		t.Errorf("empty workspace DominantLang must inherit from project: got %q, want rust", result.Workspace.DominantLang)
	}
}

// TestProjectConfigLoadMissing verifies LoadProjectConfig returns nil,nil when absent. [Épica 258.A]
func TestProjectConfigLoadMissing(t *testing.T) {
	dir := t.TempDir()
	pc, err := LoadProjectConfig(dir)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if pc != nil {
		t.Errorf("expected nil ProjectConfig, got %+v", pc)
	}
}

// TestProjectConfigRoundTrip verifies write + load round-trip. [Épica 258.A / 259.B]
func TestProjectConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &ProjectConfig{
		ProjectName:      "vision-link",
		MemberWorkspaces: []string{"go-backend", "python-ml"},
		DominantLang:     "go",
	}
	if err := WriteProjectConfig(dir, original); err != nil {
		t.Fatalf("WriteProjectConfig: %v", err)
	}

	// Verify the file exists.
	if _, err := os.Stat(filepath.Join(dir, ".neo-project", "neo.yaml")); err != nil {
		t.Fatalf("project config file missing: %v", err)
	}

	loaded, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("LoadProjectConfig: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected loaded config, got nil")
	}
	if loaded.ProjectName != "vision-link" {
		t.Errorf("ProjectName = %q, want vision-link", loaded.ProjectName)
	}
	if len(loaded.MemberWorkspaces) != 2 {
		t.Errorf("MemberWorkspaces = %v, want 2 entries", loaded.MemberWorkspaces)
	}
}

// TestProjectConfigSharedRAGField verifies the SharedRAGEnabled flag round-trips
// through yaml with omitempty semantics (absent when false). [330.I]
func TestProjectConfigSharedRAGField(t *testing.T) {
	dir := t.TempDir()
	original := &ProjectConfig{
		ProjectName:      "p",
		MemberWorkspaces: []string{"ws-a"},
		SharedRAGEnabled: true,
	}
	if err := WriteProjectConfig(dir, original); err != nil {
		t.Fatalf("WriteProjectConfig: %v", err)
	}
	loaded, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("LoadProjectConfig: %v", err)
	}
	if loaded == nil || !loaded.SharedRAGEnabled {
		t.Errorf("SharedRAGEnabled round-trip failed: loaded=%+v", loaded)
	}

	// omitempty: a default-false project should not serialize the field.
	defaultProj := &ProjectConfig{
		ProjectName:      "q",
		MemberWorkspaces: []string{"ws-a"},
	}
	dir2 := t.TempDir()
	if err := WriteProjectConfig(dir2, defaultProj); err != nil {
		t.Fatalf("WriteProjectConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir2, ".neo-project", "neo.yaml"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "shared_rag_enabled") {
		t.Errorf("shared_rag_enabled should be omitted when false, got: %s", data)
	}
}

// TestLLMOverrides_AppliedReversePrecedence verifies that project.llm_overrides
// forces its values over the workspace's AI/Inference config (opposite of the
// normal workspace > project precedence for all other fields). [350.A]
func TestLLMOverrides_AppliedReversePrecedence(t *testing.T) {
	workspace := NeoConfig{}
	workspace.AI.EmbeddingModel = "local-embed"
	workspace.AI.Provider = "ollama"
	workspace.Inference.Mode = "local"

	project := &ProjectConfig{
		ProjectName: "ws-test",
		LLMOverrides: &LLMOverrides{
			EmbeddingModel: "nomic-embed-text",
			Provider:       "ollama",
			InferenceMode:  "hybrid",
		},
	}

	merged := MergeConfigs(workspace, project, nil)
	if merged.AI.EmbeddingModel != "nomic-embed-text" {
		t.Errorf("embedding_model override failed: %q", merged.AI.EmbeddingModel)
	}
	if merged.Inference.Mode != "hybrid" {
		t.Errorf("inference.mode override failed: %q", merged.Inference.Mode)
	}
	// Provider was already "ollama" in workspace — override matches, no log but no error.
	if merged.AI.Provider != "ollama" {
		t.Errorf("provider merged incorrectly: %q", merged.AI.Provider)
	}
}

// TestLLMOverrides_NilProjectOverrides_WorkspaceWins verifies that without
// llm_overrides, workspace AI/Inference config is untouched. [350.A]
func TestLLMOverrides_NilProjectOverrides_WorkspaceWins(t *testing.T) {
	workspace := NeoConfig{}
	workspace.AI.EmbeddingModel = "my-model"
	workspace.Inference.Mode = "local"

	project := &ProjectConfig{ProjectName: "ws-test"} // no LLMOverrides

	merged := MergeConfigs(workspace, project, nil)
	if merged.AI.EmbeddingModel != "my-model" {
		t.Errorf("workspace embedding_model clobbered: %q", merged.AI.EmbeddingModel)
	}
	if merged.Inference.Mode != "local" {
		t.Errorf("workspace inference.mode clobbered: %q", merged.Inference.Mode)
	}
}

// TestLLMOverrides_EmptyFieldsSkipped verifies empty override fields don't
// zero out valid workspace values. [350.A]
func TestLLMOverrides_EmptyFieldsSkipped(t *testing.T) {
	workspace := NeoConfig{}
	workspace.AI.EmbeddingModel = "local-embed"
	workspace.Inference.Mode = "hybrid"

	project := &ProjectConfig{
		LLMOverrides: &LLMOverrides{
			EmbeddingModel: "", // empty — do not override
			InferenceMode:  "cloud",
		},
	}

	merged := MergeConfigs(workspace, project, nil)
	if merged.AI.EmbeddingModel != "local-embed" {
		t.Errorf("empty override overwrote workspace: %q", merged.AI.EmbeddingModel)
	}
	if merged.Inference.Mode != "cloud" {
		t.Errorf("set override did not apply: %q", merged.Inference.Mode)
	}
}

// TestLLMOverrides_RoundTripYaml verifies the field parses correctly from yaml. [350.A]
func TestLLMOverrides_RoundTripYaml(t *testing.T) {
	dir := t.TempDir()
	original := &ProjectConfig{
		ProjectName:      "p",
		MemberWorkspaces: []string{"ws-a"},
		LLMOverrides: &LLMOverrides{
			EmbeddingModel: "nomic-embed-text",
			InferenceMode:  "hybrid",
		},
	}
	if err := WriteProjectConfig(dir, original); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.LLMOverrides == nil {
		t.Fatalf("LLMOverrides lost in round-trip: %+v", loaded)
	}
	if loaded.LLMOverrides.EmbeddingModel != "nomic-embed-text" {
		t.Errorf("embedding_model round-trip: %q", loaded.LLMOverrides.EmbeddingModel)
	}
	if loaded.LLMOverrides.InferenceMode != "hybrid" {
		t.Errorf("inference_mode round-trip: %q", loaded.LLMOverrides.InferenceMode)
	}
}

// TestDedupStrings verifies the helper removes duplicates while preserving order. [Épica 258.B]
func TestDedupStrings(t *testing.T) {
	result := dedupStrings([]string{"a", "b", "a", "c", "b"})
	if len(result) != 3 {
		t.Errorf("expected 3 unique strings, got %d: %v", len(result), result)
	}
	if result[0] != "a" || result[1] != "b" || result[2] != "c" {
		t.Errorf("wrong order: %v", result)
	}
}
