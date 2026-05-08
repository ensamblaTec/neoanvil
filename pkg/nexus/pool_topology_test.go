package nexus

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// --- prefixWriter tests ---

func TestPrefixWriter_Write(t *testing.T) {
	var buf bytes.Buffer
	pw := newPrefixWriter("[test] ", &buf)
	_, err := pw.Write([]byte("hello\nworld\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[test] hello\n") {
		t.Errorf("expected prefixed line, got: %q", out)
	}
	if !strings.Contains(out, "[test] world\n") {
		t.Errorf("expected prefixed second line, got: %q", out)
	}
}

func TestPrefixWriter_NoNewline(t *testing.T) {
	var buf bytes.Buffer
	pw := newPrefixWriter("[pfx]", &buf)
	n, err := pw.Write([]byte("no newline"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("no newline") {
		t.Errorf("expected n=%d, got %d", len("no newline"), n)
	}
	// Buffer accumulates until newline — nothing written to out yet
	if buf.Len() != 0 {
		t.Errorf("expected empty out buffer before newline, got %q", buf.String())
	}
}

// --- NewProcessPool tests ---

func TestNewProcessPool_NotNil(t *testing.T) {
	dir := t.TempDir()
	pa := NewPortAllocator(9100, 200, filepath.Join(dir, "ports.json"))
	pp := NewProcessPool(pa, "bin/neo-mcp")
	if pp == nil {
		t.Fatal("NewProcessPool returned nil")
	}
}

func TestProcessPool_List_Empty(t *testing.T) {
	dir := t.TempDir()
	pa := NewPortAllocator(9100, 200, filepath.Join(dir, "ports.json"))
	pp := NewProcessPool(pa, "bin/neo-mcp")
	if lst := pp.List(); len(lst) != 0 {
		t.Errorf("expected empty list, got %d entries", len(lst))
	}
}

// --- RecordToolCall / GetProjectActivity / UpdateLastMemexSync tests ---

func makePool(t *testing.T) *ProcessPool {
	t.Helper()
	dir := t.TempDir()
	pa := NewPortAllocator(9100, 200, filepath.Join(dir, "ports.json"))
	pp := NewProcessPool(pa, "bin/neo-mcp")
	return pp
}

func injectProcess(pp *ProcessPool, wsID string) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	pp.processes[wsID] = &WorkspaceProcess{
		Entry: workspace.WorkspaceEntry{ID: wsID, Path: "/tmp/" + wsID},
		Status:         StatusRunning,
	}
}

func TestRecordToolCall_Unknown(t *testing.T) {
	pp := makePool(t)
	pp.RecordToolCall("does-not-exist") // must not panic
}

func TestRecordToolCall_KnownWorkspace(t *testing.T) {
	pp := makePool(t)
	injectProcess(pp, "ws-42")

	pp.RecordToolCall("ws-42")

	proc, ok := pp.GetProcess("ws-42")
	if !ok {
		t.Fatal("expected process to exist")
	}
	if proc.ToolCallCount != 1 {
		t.Errorf("expected ToolCallCount=1, got %d", proc.ToolCallCount)
	}
	if proc.LastToolCallUnix == 0 {
		t.Error("LastToolCallUnix should be set after RecordToolCall")
	}
}

func TestGetProjectActivity_NoPrior(t *testing.T) {
	pp := makePool(t)
	ac := pp.GetProjectActivity("proj-x")
	if ac == nil {
		t.Fatal("GetProjectActivity should return non-nil even for unknown project")
	}
	if ac.ToolCallCount != 0 {
		t.Errorf("expected 0 tool calls, got %d", ac.ToolCallCount)
	}
}

func TestGetProjectActivity_AfterRecordToolCall(t *testing.T) {
	pp := makePool(t)
	pp.mu.Lock()
	pp.processes["ws-proj"] = &WorkspaceProcess{
		Entry: workspace.WorkspaceEntry{ID: "ws-proj", Path: "/tmp/ws-proj"},
		Status:         StatusRunning,
		ProjectID:      "proj-y",
	}
	pp.mu.Unlock()

	pp.RecordToolCall("ws-proj")
	pp.RecordToolCall("ws-proj")

	ac := pp.GetProjectActivity("proj-y")
	if ac.ToolCallCount != 2 {
		t.Errorf("expected 2 project tool calls, got %d", ac.ToolCallCount)
	}
}

func TestUpdateLastMemexSync_Unknown(t *testing.T) {
	pp := makePool(t)
	pp.UpdateLastMemexSync("ghost", time.Now().Unix()) // must not panic
}

func TestUpdateLastMemexSync_Known(t *testing.T) {
	pp := makePool(t)
	injectProcess(pp, "ws-memex")

	now := time.Now().Unix()
	pp.UpdateLastMemexSync("ws-memex", now)

	pp.mu.RLock()
	proc := pp.processes["ws-memex"]
	got := atomic.LoadInt64(&proc.LastMemexSyncUnix)
	pp.mu.RUnlock()
	if got != now {
		t.Errorf("expected LastMemexSyncUnix=%d, got %d", now, got)
	}
}

func TestSiblingsByProject_Empty(t *testing.T) {
	pp := makePool(t)
	if got := pp.SiblingsByProject(""); got != nil {
		t.Errorf("empty projectID should return nil, got %v", got)
	}
}

// --- TopologyIndex tests ---

func TestBuildTopology_EmptyRegistry(t *testing.T) {
	reg := &workspace.Registry{}
	topo := BuildTopology(reg)
	if topo == nil {
		t.Fatal("BuildTopology returned nil")
	}
	if p := topo.ProjectForWorkspace("ws-x"); p != "" {
		t.Errorf("expected empty project, got %q", p)
	}
}

func TestTopology_SiblingsOf_Empty(t *testing.T) {
	reg := &workspace.Registry{}
	topo := BuildTopology(reg)
	if got := topo.SiblingsOf("ws-x"); got != nil {
		t.Errorf("expected nil siblings, got %v", got)
	}
}

func TestTopology_ActiveSiblings_NoSiblings(t *testing.T) {
	reg := &workspace.Registry{}
	topo := BuildTopology(reg)
	pp := makePool(t)
	if got := topo.ActiveSiblings("ws-x", pp, 300); len(got) != 0 {
		t.Errorf("expected 0 active siblings, got %v", got)
	}
}

func TestTopology_Rebuild(t *testing.T) {
	reg := &workspace.Registry{}
	topo := BuildTopology(reg)
	// Rebuild with another empty registry — must not panic
	topo.Rebuild(reg)
}

func TestApplyTopology_Empty(t *testing.T) {
	pp := makePool(t)
	reg := &workspace.Registry{}
	topo := BuildTopology(reg)
	pp.ApplyTopology(topo) // must not panic on empty pool
}

// --- ProxyWithRetry test ---

func TestProxyWithRetry_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	f := NewFleetRegistry()
	body := []byte(`{"test":1}`)
	client := &http.Client{Timeout: 2 * time.Second}
	respBody, code, err := f.ProxyWithRetry(client, srv.URL, body, 1)
	if err != nil {
		t.Fatalf("ProxyWithRetry: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("expected 200, got %d", code)
	}
	if !bytes.Contains(respBody, []byte("ok")) {
		t.Errorf("unexpected body: %q", respBody)
	}
}

func TestProxyWithRetry_ServerError(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := NewFleetRegistry()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	_, _, err := f.ProxyWithRetry(client, srv.URL, nil, 2)
	if err == nil {
		t.Error("expected error after exhausting retries with 500 responses")
	}
	if callCount != 2 {
		t.Errorf("expected 2 attempts, got %d", callCount)
	}
}

// --- isTerminal test ---

func TestIsTerminal_RegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("regular file should not be a terminal")
	}
}
