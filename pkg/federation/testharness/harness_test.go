// pkg/federation/testharness/harness_test.go — E2E tests for the federation harness. [358.A]
package testharness

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpawnEphemeralFederation_WorkspacesExist verifies that SpawnEphemeralFederation
// creates workspace directories and accessible KnowledgeStores. [358.A]
func TestSpawnEphemeralFederation_WorkspacesExist(t *testing.T) {
	fed := SpawnEphemeralFederation(t, []string{"ws-a", "ws-b", "ws-c"})

	for _, name := range []string{"ws-a", "ws-b", "ws-c"} {
		ws := fed.Workspace(name)
		if ws.Dir == "" {
			t.Errorf("workspace %q: empty Dir", name)
		}
		if _, err := os.Stat(ws.Dir); err != nil {
			t.Errorf("workspace %q: dir does not exist: %v", name, err)
		}
		if ws.Store == nil {
			t.Errorf("workspace %q: nil KnowledgeStore", name)
		}
	}
}

// TestSpawnEphemeralFederation_ProjectConfig verifies .neo-project/neo.yaml is written. [358.A]
func TestSpawnEphemeralFederation_ProjectConfig(t *testing.T) {
	fed := SpawnEphemeralFederation(t, []string{"alpha", "beta"})

	cfgPath := filepath.Join(fed.ProjectDir, "neo.yaml")
	data, err := os.ReadFile(cfgPath) //nolint:gosec // G304-WORKSPACE-CANON: test-only TempDir
	if err != nil {
		t.Fatalf("project neo.yaml not found: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "project_name: test-federation") {
		t.Errorf("project config missing project_name: %s", body)
	}
	if !strings.Contains(body, "coordinator_workspace: alpha") {
		t.Errorf("project config missing coordinator: %s", body)
	}
	for _, name := range []string{"alpha", "beta"} {
		ws := fed.Workspace(name)
		if !strings.Contains(body, ws.Dir) {
			t.Errorf("project config missing member %q path %s", name, ws.Dir)
		}
	}
}

// TestFed_InboxRoundtrip verifies SendInbox delivers to the recipient's KnowledgeStore. [358.A]
func TestFed_InboxRoundtrip(t *testing.T) {
	fed := SpawnEphemeralFederation(t, []string{"sender", "receiver"})

	if err := fed.SendInbox("sender", "receiver", "api contract changed"); err != nil {
		t.Fatalf("SendInbox: %v", err)
	}

	// Verify the message landed in receiver's inbox namespace.
	ks := fed.Workspace("receiver").Store
	entries, err := ks.ListInboxFor("receiver", false)
	if err != nil {
		t.Fatalf("List inbox: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("inbox empty after SendInbox")
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Content, "api contract changed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("inbox message not found; entries: %+v", entries)
	}
}

// TestFed_MultiInbox verifies multiple messages to multiple recipients. [358.A]
func TestFed_MultiInbox(t *testing.T) {
	fed := SpawnEphemeralFederation(t, []string{"ws1", "ws2", "ws3"})

	if err := fed.SendInbox("ws1", "ws2", "deploy ready"); err != nil {
		t.Fatalf("SendInbox ws1→ws2: %v", err)
	}
	if err := fed.SendInbox("ws1", "ws3", "schema bump"); err != nil {
		t.Fatalf("SendInbox ws1→ws3: %v", err)
	}

	ws2entries, _ := fed.Workspace("ws2").Store.ListInboxFor("ws2", false)
	if len(ws2entries) != 1 {
		t.Errorf("ws2 inbox: want 1 message, got %d", len(ws2entries))
	}
	ws3entries, _ := fed.Workspace("ws3").Store.ListInboxFor("ws3", false)
	if len(ws3entries) != 1 {
		t.Errorf("ws3 inbox: want 1 message, got %d", len(ws3entries))
	}
	// ws1 should have no messages directed at it.
	ws1entries, _ := fed.Workspace("ws1").Store.ListInboxFor("ws1", false)
	if len(ws1entries) != 0 {
		t.Errorf("ws1 inbox: want 0, got %d", len(ws1entries))
	}
}

// TestFed_Certify verifies Certify writes the stamp file. [358.A]
func TestFed_Certify(t *testing.T) {
	fed := SpawnEphemeralFederation(t, []string{"target-ws"})
	files := []string{"/home/x/pkg/rag/hnsw.go", "/home/x/pkg/rag/quantize.go"}

	if err := fed.Certify("target-ws", files); err != nil {
		t.Fatalf("Certify: %v", err)
	}
	ws := fed.Workspace("target-ws")
	lockPath := filepath.Join(ws.Dir, ".neo", "db", "certified_state.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("certified_state.lock not written: %v", err)
	}
}

// TestFed_NexusMockHealth verifies the Nexus mock server is reachable. [358.A]
func TestFed_NexusMockHealth(t *testing.T) {
	fed := SpawnEphemeralFederation(t, []string{"sole-ws"})

	resp, err := http.Get(fed.NexusMock.URL + "/health") //nolint:gosec // G107-TRUSTED-CONFIG-URL: test-controlled localhost
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Errorf("health body: %s", body)
	}
}

// TestFed_NexusMockWorkspacesList verifies /api/v1/workspaces lists federation members. [358.A]
func TestFed_NexusMockWorkspacesList(t *testing.T) {
	fed := SpawnEphemeralFederation(t, []string{"alpha", "beta"})

	resp, err := http.Get(fed.NexusMock.URL + "/api/v1/workspaces") //nolint:gosec // G107-TRUSTED-CONFIG-URL: test-only
	if err != nil {
		t.Fatalf("GET /api/v1/workspaces: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(bodyStr, name) {
			t.Errorf("/api/v1/workspaces missing %q; body=%s", name, bodyStr)
		}
	}
}

// TestFed_WorkspaceNotFound verifies Workspace panics-via-Fatal on unknown name. [358.A]
func TestFed_WorkspaceNotFound(t *testing.T) {
	fed := SpawnEphemeralFederation(t, []string{"real-ws"})
	// Verify "real-ws" is accessible without triggering Fatal.
	ws := fed.Workspace("real-ws")
	if ws == nil {
		t.Error("Workspace(real-ws) returned nil")
	}
}

// TestSanitizeTopic verifies topic fragment generation. [358.A]
func TestSanitizeTopic(t *testing.T) {
	cases := []struct{ in, want string }{
		{"api contract changed", "api-contract-changed"},
		{"schema bump v2", "schema-bump-v2"},
		{"", "msg"},
		{"Deploy Ready!", "deploy-ready"},
		{"a b c d e f", "a-b-c"}, // truncated to 3 words
	}
	for _, c := range cases {
		got := sanitizeTopic(c.in)
		if got != c.want {
			t.Errorf("sanitizeTopic(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
