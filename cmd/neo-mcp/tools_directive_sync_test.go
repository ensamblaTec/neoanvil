package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// newTestWALInTempDir opens a fresh WAL in a temp directory. The caller owns
// the workspace path and is responsible for cleanup via t.Cleanup.
func newTestWALInTempDir(t *testing.T) (*rag.WAL, string) {
	t.Helper()
	workspace := t.TempDir()
	dbDir := filepath.Join(workspace, ".neo", "db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	wal, err := rag.OpenWAL(filepath.Join(dbDir, "brain.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	return wal, workspace
}

// TestLearnDirective_DiskSync_HappyPath verifies that handleAddDirective
// writes the directive to .claude/rules/neo-synced-directives.md and the file
// mtime advances after the call. This is the regression test for the silent
// sync failure observed 2026-04-30 in strategosia-frontend session. [141.3]
func TestLearnDirective_DiskSync_HappyPath(t *testing.T) {
	wal, workspace := newTestWALInTempDir(t)
	tool := NewLearnDirectiveTool(wal, workspace)

	syncPath := filepath.Join(workspace, ".claude", "rules", "neo-synced-directives.md")

	// Pre-state: file may or may not exist (don't care). Capture mtime if so.
	var preMtime time.Time
	if st, err := os.Stat(syncPath); err == nil {
		preMtime = st.ModTime()
	}
	// Tiny sleep to ensure mtime resolution is exceeded by the next write.
	time.Sleep(10 * time.Millisecond)

	resp, err := tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": "[TEST-141.3] verify dual-layer sync writes file",
	})
	if err != nil {
		t.Fatalf("Execute add: %v", err)
	}

	// Response must be a success message containing "(sync ok)".
	respText := extractRespText(t, resp)
	if !strings.Contains(respText, "Directiva añadida") {
		t.Errorf("expected 'Directiva añadida' in response, got %q", respText)
	}
	if !strings.Contains(respText, "(sync ok)") {
		t.Errorf("expected '(sync ok)' suffix in response (visibility of disk sync), got %q", respText)
	}
	if strings.Contains(respText, "SYNC FAILED") {
		t.Errorf("unexpected SYNC FAILED in happy path, got %q", respText)
	}

	// File must exist now.
	st, err := os.Stat(syncPath)
	if err != nil {
		t.Fatalf("expected sync file to exist post-add, got %v", err)
	}
	if !st.ModTime().After(preMtime) {
		t.Errorf("sync file mtime did not advance: pre=%v post=%v", preMtime, st.ModTime())
	}

	// Content must contain the directive text.
	body, err := os.ReadFile(syncPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "[TEST-141.3] verify dual-layer sync writes file") {
		t.Errorf("expected directive in file body, got:\n%s", body)
	}
}

// TestLearnDirective_DiskSync_FailureSurfaces verifies that when the disk write
// fails (read-only target dir), the response carries "SYNC FAILED" so the caller
// can detect the regression. The BoltDB write still succeeds — sync is best-effort
// but visibly so. [141.4]
func TestLearnDirective_DiskSync_FailureSurfaces(t *testing.T) {
	wal, workspace := newTestWALInTempDir(t)

	// Create .claude/rules/ as read-only so SyncDirectivesToDisk fails on Write.
	rulesDir := filepath.Join(workspace, ".claude", "rules")
	if err := os.MkdirAll(rulesDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-write a file to lock its existence, then chmod the parent to ro.
	syncPath := filepath.Join(rulesDir, "neo-synced-directives.md")
	if err := os.WriteFile(syncPath, []byte("# placeholder\n"), 0444); err != nil {
		t.Fatal(err)
	}
	// Make parent dir + file read-only. os.WriteFile on the file should fail.
	if err := os.Chmod(syncPath, 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rulesDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Restore writable so t.TempDir cleanup works.
		_ = os.Chmod(rulesDir, 0755)
		_ = os.Chmod(syncPath, 0644)
	})

	tool := NewLearnDirectiveTool(wal, workspace)
	resp, err := tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": "[TEST-141.4] write should fail on read-only file",
	})
	if err != nil {
		t.Fatalf("Execute should NOT propagate sync error (BoltDB ok), got %v", err)
	}

	respText := extractRespText(t, resp)
	if !strings.Contains(respText, "Directiva añadida") {
		t.Errorf("BoltDB write succeeded — expected 'Directiva añadida' regardless, got %q", respText)
	}
	if !strings.Contains(respText, "SYNC FAILED") {
		t.Errorf("expected 'SYNC FAILED' in response (operator must see failure), got %q", respText)
	}
	if strings.Contains(respText, "(sync ok)") {
		t.Errorf("unexpected '(sync ok)' when sync should have failed, got %q", respText)
	}
}

// === ÉPICA 374.F — Directive auto-governance tests ===

func TestDirective_CharLimit_Reject(t *testing.T) {
	wal, workspace := newTestWALInTempDir(t)
	cfg := &config.NeoConfig{}
	cfg.SRE.MaxDirectiveChars = 50
	cfg.SRE.MaxDirectives = 100
	tool := NewLearnDirectiveTool(wal, workspace).WithConfig(func() *config.NeoConfig { return cfg })

	_, err := tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": strings.Repeat("x", 51),
	})
	if err == nil {
		t.Fatal("expected error for directive > 50 chars")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("expected 'too long' in error, got %q", err.Error())
	}
}

func TestDirective_CharLimit_Accept(t *testing.T) {
	wal, workspace := newTestWALInTempDir(t)
	cfg := &config.NeoConfig{}
	cfg.SRE.MaxDirectiveChars = 50
	cfg.SRE.MaxDirectives = 100
	tool := NewLearnDirectiveTool(wal, workspace).WithConfig(func() *config.NeoConfig { return cfg })

	resp, err := tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": strings.Repeat("x", 50),
	})
	if err != nil {
		t.Fatalf("expected success for directive == 50 chars, got %v", err)
	}
	text := extractRespText(t, resp)
	if !strings.Contains(text, "Directiva añadida") {
		t.Errorf("expected success message, got %q", text)
	}
}

func TestDirective_CountLimit_Reject(t *testing.T) {
	wal, workspace := newTestWALInTempDir(t)
	existing, _ := wal.GetDirectives()
	baseCount := len(existing)

	cfg := &config.NeoConfig{}
	cfg.SRE.MaxDirectiveChars = 500
	cfg.SRE.MaxDirectives = baseCount + 3
	tool := NewLearnDirectiveTool(wal, workspace).WithConfig(func() *config.NeoConfig { return cfg })

	for i := 0; i < 3; i++ {
		_, err := tool.Execute(context.Background(), map[string]any{
			"action":    "add",
			"directive": "[TEST-LIMIT] directive " + strings.Repeat("a", i+1),
		})
		if err != nil {
			t.Fatalf("directive %d should succeed (base=%d, max=%d), got %v", i, baseCount, cfg.SRE.MaxDirectives, err)
		}
	}
	_, err := tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": "[TEST-LIMIT] should be rejected",
	})
	if err == nil {
		t.Fatal("expected error when count limit reached")
	}
	if !strings.Contains(err.Error(), "limit reached") {
		t.Errorf("expected 'limit reached' in error, got %q", err.Error())
	}
}

func TestDirective_CountLimit_Accept(t *testing.T) {
	wal, workspace := newTestWALInTempDir(t)
	existing, _ := wal.GetDirectives()
	baseCount := len(existing)

	cfg := &config.NeoConfig{}
	cfg.SRE.MaxDirectiveChars = 500
	cfg.SRE.MaxDirectives = baseCount + 3
	tool := NewLearnDirectiveTool(wal, workspace).WithConfig(func() *config.NeoConfig { return cfg })

	for i := 0; i < 2; i++ {
		_, _ = tool.Execute(context.Background(), map[string]any{
			"action":    "add",
			"directive": "[TEST-ACCEPT] directive " + strings.Repeat("b", i+1),
		})
	}
	resp, err := tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": "[TEST-ACCEPT] third should work",
	})
	if err != nil {
		t.Fatalf("directive 3/3 should succeed (base=%d, max=%d), got %v", baseCount, cfg.SRE.MaxDirectives, err)
	}
	text := extractRespText(t, resp)
	if !strings.Contains(text, "Directiva añadida") {
		t.Errorf("expected success, got %q", text)
	}
}

func TestDirective_Compact(t *testing.T) {
	wal, workspace := newTestWALInTempDir(t)
	tool := NewLearnDirectiveTool(wal, workspace)

	for i := 0; i < 5; i++ {
		_ = wal.SaveDirective("[TAG-A] content " + strings.Repeat("c", i+1))
	}
	_ = wal.DeprecateDirective(2, 0)
	_ = wal.DeprecateDirective(4, 0)

	resp, err := tool.Execute(context.Background(), map[string]any{
		"action": "compact",
	})
	if err != nil {
		t.Fatalf("compact should succeed, got %v", err)
	}
	text := extractRespText(t, resp)
	if !strings.Contains(text, "Compacted") {
		t.Errorf("expected 'Compacted' in response, got %q", text)
	}
	rules, _ := wal.GetDirectives()
	for _, r := range rules {
		if strings.Contains(r, "~~OBSOLETO~~") {
			t.Errorf("compact should have purged deprecated entry: %q", r)
		}
	}
	if len(rules) != 3 {
		t.Errorf("expected 3 active directives after compact, got %d", len(rules))
	}
}

func TestDirective_ConfigOverride(t *testing.T) {
	wal, workspace := newTestWALInTempDir(t)
	existing, _ := wal.GetDirectives()
	baseCount := len(existing)

	cfg := &config.NeoConfig{}
	cfg.SRE.MaxDirectiveChars = 10
	cfg.SRE.MaxDirectives = baseCount + 1
	tool := NewLearnDirectiveTool(wal, workspace).WithConfig(func() *config.NeoConfig { return cfg })

	_, err := tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": strings.Repeat("z", 11),
	})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected char limit from config override (10), got err=%v", err)
	}

	cfg.SRE.MaxDirectiveChars = 500
	_, err = tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": "[TEST-CFG] first",
	})
	if err != nil {
		t.Fatalf("first directive should succeed, got %v", err)
	}
	_, err = tool.Execute(context.Background(), map[string]any{
		"action":    "add",
		"directive": "[TEST-CFG] second should fail",
	})
	if err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("expected count limit from config override (%d), got err=%v", cfg.SRE.MaxDirectives, err)
	}
}

// extractRespText unwraps the MCP response envelope to get the human-readable
// text. Tools return {"content":[{"type":"text","text":"..."}]} via mcpOK.
func extractRespText(t *testing.T, resp any) string {
	t.Helper()
	m, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("response not a map, got %T", resp)
	}
	content, ok := m["content"].([]map[string]any)
	if !ok {
		t.Fatalf("response.content not []map, got %T", m["content"])
	}
	if len(content) == 0 {
		t.Fatalf("response.content empty")
	}
	text, ok := content[0]["text"].(string)
	if !ok {
		t.Fatalf("content[0].text not string, got %T", content[0]["text"])
	}
	return text
}
