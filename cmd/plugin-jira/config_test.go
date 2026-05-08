package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPluginConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jira.json")

	cfg := minimalConfig()
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadPluginConfig(path)
	if err != nil {
		t.Fatalf("loadPluginConfig: %v", err)
	}
	if loaded.ActiveProject != "test" {
		t.Errorf("ActiveProject = %q, want %q", loaded.ActiveProject, "test")
	}
	if loaded.Projects["test"].ProjectKey != "TEST" {
		t.Errorf("ProjectKey = %q, want %q", loaded.Projects["test"].ProjectKey, "TEST")
	}
}

func TestLoadPluginConfig_MissingFile(t *testing.T) {
	_, err := loadPluginConfig("/nonexistent/jira.json")
	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist, got %v", err)
	}
}

func TestValidateConfig_NoProjects(t *testing.T) {
	cfg := &PluginConfig{Version: 1, ActiveProject: "x"}
	if err := validateConfig(cfg); err == nil {
		t.Error("expected error for empty projects")
	}
}

func TestValidateConfig_ActiveProjectNotFound(t *testing.T) {
	cfg := minimalConfig()
	cfg.ActiveProject = "nonexistent"
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "not found in projects") {
		t.Errorf("expected 'not found in projects' error, got %v", err)
	}
}

func TestValidateConfig_MissingAPIKey(t *testing.T) {
	cfg := minimalConfig()
	cfg.Projects["test"].APIKeyRef = "missing"
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "not found in api_keys") {
		t.Errorf("expected 'not found in api_keys' error, got %v", err)
	}
}

func TestValidateConfig_ShortWorkflow(t *testing.T) {
	cfg := minimalConfig()
	cfg.Projects["test"].IssueTypes["epic"].Workflow = []string{"Done"}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "workflow must have >= 2") {
		t.Errorf("expected workflow error, got %v", err)
	}
}

func TestResolveToken_Inline(t *testing.T) {
	tok := "secret123"
	key := &APIKey{Auth: AuthConfig{Token: &tok}}
	got, err := resolveToken(key)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret123" {
		t.Errorf("got %q, want %q", got, "secret123")
	}
}

func TestResolveToken_EnvRef(t *testing.T) {
	t.Setenv("TEST_JIRA_TOK", "fromenv")
	ref := "env:TEST_JIRA_TOK"
	key := &APIKey{Auth: AuthConfig{TokenRef: &ref}}
	got, err := resolveToken(key)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fromenv" {
		t.Errorf("got %q, want %q", got, "fromenv")
	}
}

func TestResolveToken_FileRef(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	os.WriteFile(p, []byte("  filetoken  \n"), 0600)
	ref := "file:" + p
	key := &APIKey{Auth: AuthConfig{TokenRef: &ref}}
	got, err := resolveToken(key)
	if err != nil {
		t.Fatal(err)
	}
	if got != "filetoken" {
		t.Errorf("got %q, want %q", got, "filetoken")
	}
}

func TestResolveToken_NoTokenOrRef(t *testing.T) {
	key := &APIKey{}
	_, err := resolveToken(key)
	if err == nil {
		t.Error("expected error for empty key")
	}
}

func TestResolveProject_DirectMapping(t *testing.T) {
	cfg := minimalConfig()
	cfg.WorkspaceMapping = map[string]string{"ws-1": "test"}
	proj, name, err := cfg.resolveProject("ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if name != "test" || proj.ProjectKey != "TEST" {
		t.Errorf("got name=%q key=%q", name, proj.ProjectKey)
	}
}

func TestResolveProject_DefaultFallback(t *testing.T) {
	cfg := minimalConfig()
	cfg.WorkspaceMapping = map[string]string{"default": "test"}
	proj, name, err := cfg.resolveProject("unknown-ws")
	if err != nil {
		t.Fatal(err)
	}
	if name != "test" || proj == nil {
		t.Error("expected default fallback")
	}
}

func TestResolveProject_ActiveProjectFallback(t *testing.T) {
	cfg := minimalConfig()
	cfg.WorkspaceMapping = map[string]string{}
	proj, name, err := cfg.resolveProject("unknown-ws")
	if err != nil {
		t.Fatal(err)
	}
	if name != "test" || proj == nil {
		t.Error("expected active_project fallback")
	}
}

func TestResolveProject_NormalizesInput(t *testing.T) {
	cfg := minimalConfig()
	cfg.WorkspaceMapping = map[string]string{"ws-1": "test"}
	proj, _, err := cfg.resolveProject("  WS-1  ")
	if err != nil {
		t.Fatal(err)
	}
	if proj == nil {
		t.Error("expected normalized match")
	}
}

func TestResolveProject_NoMatch(t *testing.T) {
	cfg := minimalConfig()
	cfg.WorkspaceMapping = map[string]string{}
	cfg.ActiveProject = "nonexistent"
	cfg.Projects = map[string]*ProjectCfg{
		"test": minimalConfig().Projects["test"],
	}
	_, _, err := cfg.resolveProject("")
	if err == nil {
		t.Error("expected error when nothing matches")
	}
}

func TestExtractCallCtx_Full(t *testing.T) {
	params := map[string]any{
		"_meta": map[string]any{
			"workspace_id": "  neoanvil-95248 ",
			"trace_id":     "abc123",
		},
	}
	cc := extractCallCtx(params)
	if cc.WorkspaceID != "neoanvil-95248" {
		t.Errorf("WorkspaceID = %q, want trimmed", cc.WorkspaceID)
	}
	if cc.TraceID != "abc123" {
		t.Errorf("TraceID = %q", cc.TraceID)
	}
}

func TestExtractCallCtx_NoMeta(t *testing.T) {
	cc := extractCallCtx(map[string]any{})
	if cc.WorkspaceID != "" || cc.TraceID != "" {
		t.Error("expected empty callCtx")
	}
}

func TestConfigHolder_AtomicSwap(t *testing.T) {
	h := &configHolder{}
	cfg1 := minimalConfig()
	h.set(cfg1)
	got := h.get()
	if got != cfg1 {
		t.Error("expected same pointer")
	}
	cfg2 := minimalConfig()
	cfg2.ActiveProject = "other"
	h.set(cfg2)
	got = h.get()
	if got.ActiveProject != "other" {
		t.Errorf("ActiveProject = %q after swap", got.ActiveProject)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func minimalConfig() *PluginConfig {
	tok := "test-token"
	return &PluginConfig{
		Version:       2,
		ActiveProject: "test",
		APIKeys: map[string]*APIKey{
			"testkey": {
				Domain: "test.atlassian.net",
				Email:  "test@test.com",
				Auth:   AuthConfig{Type: "PAT", Token: &tok},
				RateLimit: RateLimit{
					MaxPerMinute:    300,
					Concurrency:     5,
					RetryOn429:      true,
					BackoffStrategy: "exponential",
				},
			},
		},
		WorkspaceMapping: map[string]string{"default": "test"},
		Projects: map[string]*ProjectCfg{
			"test": {
				APIKeyRef:  "testkey",
				ProjectKey: "TEST",
				IssueTypes: map[string]*IssueTypeCfg{
					"epic":  {Workflow: []string{"Backlog", "In Progress", "Done"}},
					"story": {Workflow: []string{"Backlog", "In Progress", "Done"}},
				},
				CustomFields: map[string]string{},
				Templates:    map[string]string{},
				Priorities:   PriorityCfg{Mapping: map[string]string{}},
			},
		},
		TemplateLibrary: map[string]*Template{},
	}
}
