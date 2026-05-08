// Package testharness provides a deterministic E2E test harness for
// cross-workspace federation scenarios. It spawns ephemeral workspace
// directories in t.TempDir(), wires up KnowledgeStore instances, and
// starts an in-memory Nexus mock server so tests need neither Ollama
// nor a real neo-mcp process. Tests run in <10s total. [358.A]
package testharness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

// TestWorkspace is an ephemeral workspace created by SpawnEphemeralFederation.
type TestWorkspace struct {
	Name  string
	Dir   string
	Store *knowledge.KnowledgeStore // local knowledge.db for this workspace
}

// TestFed is the federation harness: N workspaces + shared project dir + Nexus mock.
type TestFed struct {
	t          *testing.T
	workspaces map[string]*TestWorkspace // name → workspace
	NexusMock  *httptest.Server         // in-memory Nexus (implements /internal/* and /api/v1/* stubs)
	ProjectDir string                   // .neo-project/ directory
}

// SpawnEphemeralFederation creates an ephemeral multi-workspace federation.
// names specifies the workspace basenames (e.g. ["ws-a", "ws-b"]).
// All resources are cleaned up automatically via t.Cleanup. [358.A]
func SpawnEphemeralFederation(t *testing.T, names []string) *TestFed {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("SpawnEphemeralFederation: at least one workspace name required")
	}

	root := t.TempDir()
	fed := &TestFed{
		t:          t,
		workspaces: make(map[string]*TestWorkspace, len(names)),
	}

	// Create workspace directories + knowledge stores.
	var memberPaths []string
	for _, name := range names {
		wsDir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(wsDir, ".neo", "db"), 0750); err != nil {
			t.Fatalf("SpawnEphemeralFederation: mkdir %s: %v", wsDir, err)
		}
		storePath := filepath.Join(wsDir, ".neo", "db", "knowledge.db")
		ks, err := knowledge.Open(storePath)
		if err != nil {
			t.Fatalf("SpawnEphemeralFederation: knowledge.Open(%s): %v", storePath, err)
		}
		fed.workspaces[name] = &TestWorkspace{Name: name, Dir: wsDir, Store: ks}
		memberPaths = append(memberPaths, wsDir)
	}

	// Write .neo-project/neo.yaml with member_workspaces.
	projDir := filepath.Join(root, ".neo-project")
	if err := os.MkdirAll(filepath.Join(projDir, "db"), 0750); err != nil {
		t.Fatalf("SpawnEphemeralFederation: mkdir .neo-project: %v", err)
	}
	fed.ProjectDir = projDir
	if err := writeProjectConfig(projDir, names[0], memberPaths); err != nil {
		t.Fatalf("SpawnEphemeralFederation: write project config: %v", err)
	}

	// Start in-memory Nexus mock server.
	fed.NexusMock = httptest.NewServer(nexusMockHandler(fed))
	t.Cleanup(func() {
		fed.NexusMock.Close()
		for _, ws := range fed.workspaces {
			_ = ws.Store.Close()
		}
	})

	return fed
}

// Workspace returns the TestWorkspace for the given name. Fatals if not found.
func (f *TestFed) Workspace(name string) *TestWorkspace {
	f.t.Helper()
	ws, ok := f.workspaces[name]
	if !ok {
		f.t.Fatalf("TestFed.Workspace: %q not in federation (available: %v)", name, f.workspaceNames())
	}
	return ws
}

// SendInbox delivers a message from workspace `from` to workspace `to` with the
// given body. Uses the recipient's local KnowledgeStore (simulates Nexus routing). [358.A]
func (f *TestFed) SendInbox(from, to, body string) error {
	recipient := f.workspaces[to]
	if recipient == nil {
		return fmt.Errorf("SendInbox: workspace %q not found", to)
	}
	// Key format: "to-<recipient>-<topic>" per [331.A] ValidateInboxKey.
	topic := sanitizeTopic(body)
	key := fmt.Sprintf("to-%s-%s", to, topic)
	return recipient.Store.PutInbox(from, key, body, "normal", 0)
}

// Certify simulates a successful certify pass for the given files in workspace wsName.
// Writes a stamp to <wsDir>/.neo/db/certified_state.lock so the pre-commit hook sees
// the files as certified. No actual compilation is performed. [358.A]
func (f *TestFed) Certify(wsName string, files []string) error {
	ws := f.workspaces[wsName]
	if ws == nil {
		return fmt.Errorf("Certify: workspace %q not found", wsName)
	}
	lockPath := filepath.Join(ws.Dir, ".neo", "db", "certified_state.lock")
	stamps := make(map[string]int64, len(files))
	now := time.Now().Unix()
	for _, f := range files {
		stamps[f] = now
	}
	data, err := json.Marshal(stamps)
	if err != nil {
		return err
	}
	return os.WriteFile(lockPath, data, 0600) //nolint:gosec // G304-WORKSPACE-CANON: wsDir is test-internal TempDir
}

// workspaceNames returns a sorted list of workspace names for error messages.
func (f *TestFed) workspaceNames() []string {
	names := make([]string, 0, len(f.workspaces))
	for n := range f.workspaces {
		names = append(names, n)
	}
	return names
}

// writeProjectConfig writes a minimal .neo-project/neo.yaml. [358.A]
func writeProjectConfig(projDir, coordinator string, memberPaths []string) error {
	var sb strings.Builder
	sb.WriteString("project_name: test-federation\n")
	fmt.Fprintf(&sb, "coordinator_workspace: %s\n", coordinator)
	sb.WriteString("dominant_lang: go\n")
	sb.WriteString("member_workspaces:\n")
	for _, p := range memberPaths {
		fmt.Fprintf(&sb, "  - %s\n", p)
	}
	return os.WriteFile(
		filepath.Join(projDir, "neo.yaml"),
		[]byte(sb.String()),
		0600, //nolint:gosec // G306: project config, no security concern in test-only dir
	)
}

// sanitizeTopic returns a lowercase, alphanum+dash topic fragment from body (max 32 chars).
func sanitizeTopic(body string) string {
	words := strings.Fields(body)
	if len(words) > 3 {
		words = words[:3]
	}
	topic := strings.ToLower(strings.Join(words, "-"))
	var out []byte
	for _, c := range []byte(topic) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			out = append(out, c)
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	if len(out) == 0 {
		out = []byte("msg")
	}
	return string(out)
}

// nexusMockHandler returns an http.Handler that stubs the internal Nexus endpoints
// required by BRIEFING, debt polling, and presence. All responses are minimal valid
// JSON so the callers don't 404. [358.A]
func nexusMockHandler(fed *TestFed) http.Handler {
	mux := http.NewServeMux()

	// Workspace list for /api/v1/workspaces.
	mux.HandleFunc("/api/v1/workspaces", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		type wsEntry struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		var entries []wsEntry
		for name, ws := range fed.workspaces {
			entries = append(entries, wsEntry{ID: name, Name: ws.Name, Status: "running"})
		}
		_ = json.NewEncoder(w).Encode(entries)
	})

	// Presence stub — empty array.
	mux.HandleFunc("/api/v1/presence", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "[]")
	})

	// Nexus debt stub — empty.
	mux.HandleFunc("/internal/nexus/debt/affecting", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"events":[]}`)
	})

	// Nexus shared knowledge store — minimal in-memory KV for cross-workspace ops.
	sharedKV := make(map[string]string)
	mux.HandleFunc("/api/v1/shared/nexus/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/api/v1/shared/nexus/")
		switch r.Method {
		case http.MethodGet:
			if v, ok := sharedKV[key]; ok {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, v)
			} else {
				http.NotFound(w, r)
			}
		case http.MethodPut, http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			sharedKV[key] = string(raw)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	// Health endpoint.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	// Catch-all: return empty JSON object for any unknown endpoint.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	})

	return mux
}
