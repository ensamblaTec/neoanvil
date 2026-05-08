package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
)

// newDebtToolWithNexus wires a DebtTool to a captive httptest Nexus mock so
// the nexus-scope actions can be exercised without a live dispatcher.
func newDebtToolWithNexus(t *testing.T, handler http.Handler) (*DebtTool, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	tool := &DebtTool{
		workspace:   t.TempDir(),
		workspaceID: "ws-under-test",
		nexusURL:    srv.URL,
	}
	return tool, srv
}

func TestDebtTool_ListWorkspace_Empty(t *testing.T) {
	tool := &DebtTool{workspace: t.TempDir()}
	got, err := tool.Execute(context.Background(), map[string]any{"action": "list", "scope": "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, got)
	if !strings.Contains(text, "empty") {
		t.Errorf("expected empty marker, got: %q", text)
	}
}

func TestDebtTool_ListWorkspace_WithFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".neo"), 0o750); err != nil {
		t.Fatal(err)
	}
	content := "## [now] bar\n\n**Prioridad:** P1\n\ndetail\n"
	if err := os.WriteFile(filepath.Join(ws, ".neo", "technical_debt.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &DebtTool{workspace: ws}
	got, err := tool.Execute(context.Background(), map[string]any{"action": "list", "scope": "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, got)
	if !strings.Contains(text, "P1") {
		t.Errorf("expected P1 entry rendered, got: %q", text)
	}
}

func TestDebtTool_AffectingMe(t *testing.T) {
	event := nexus.NexusDebtEvent{
		ID: "ev-1", Priority: "P0", Title: "boot timeout",
		AffectedWorkspaces: []string{"ws-under-test"},
		DetectedAt:         time.Now(),
		OccurrenceCount:    1,
		Recommended:        "kill zombie",
	}
	handler := http.NewServeMux()
	handler.HandleFunc("/internal/nexus/debt/affecting", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("workspace_id") != "ws-under-test" {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode([]nexus.NexusDebtEvent{event})
	})
	tool, srv := newDebtToolWithNexus(t, handler)
	defer srv.Close()

	got, err := tool.Execute(context.Background(), map[string]any{"action": "affecting_me"})
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, got)
	if !strings.Contains(text, "boot timeout") {
		t.Errorf("expected event title, got: %q", text)
	}
	if !strings.Contains(text, "kill zombie") {
		t.Errorf("expected recommended row, got: %q", text)
	}
}

func TestDebtTool_AffectingMe_NoWorkspaceID(t *testing.T) {
	tool := &DebtTool{workspaceID: ""}
	got, err := tool.Execute(context.Background(), map[string]any{"action": "affecting_me"})
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, got)
	if !strings.Contains(text, "workspace_id unknown") {
		t.Errorf("expected degradation notice, got: %q", text)
	}
}

func TestDebtTool_ResolveNexus(t *testing.T) {
	handler := http.NewServeMux()
	called := false
	handler.HandleFunc("/internal/nexus/debt/resolve", func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusBadRequest)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["id"] != "ev-1" {
			t.Errorf("id payload = %q, want ev-1", body["id"])
		}
		w.WriteHeader(http.StatusOK)
	})
	tool, srv := newDebtToolWithNexus(t, handler)
	defer srv.Close()

	_, err := tool.Execute(context.Background(), map[string]any{
		"action": "resolve", "scope": "nexus",
		"id": "ev-1", "resolution": "fixed manually",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("resolve endpoint was not hit")
	}
}

func TestDebtTool_RecordWorkspace(t *testing.T) {
	ws := t.TempDir()
	tool := &DebtTool{workspace: ws}
	_, err := tool.Execute(context.Background(), map[string]any{
		"action":      "record",
		"scope":       "workspace",
		"title":       "func foo: CC=18 (limit 15)",
		"description": "extract helper to reduce branches",
		"priority":    "P1",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".neo", "technical_debt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "func foo") {
		t.Errorf("workspace debt file missing appended entry: %s", data)
	}
}

func TestDebtTool_UnknownAction(t *testing.T) {
	tool := &DebtTool{}
	_, err := tool.Execute(context.Background(), map[string]any{"action": "bogus"})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

// extractText unwraps the MCP content envelope produced by mcpText.
func extractText(t *testing.T, v any) string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", v)
	}
	content, _ := m["content"].([]map[string]any)
	if len(content) == 0 {
		if arr, ok2 := m["content"].([]any); ok2 && len(arr) > 0 {
			if one, ok3 := arr[0].(map[string]any); ok3 {
				s, _ := one["text"].(string)
				return s
			}
		}
		t.Fatalf("missing content in %+v", v)
	}
	s, _ := content[0]["text"].(string)
	return s
}
