package main_test

// integ_test spawns the real plugin-jira binary as a subprocess, drives
// it through stdin/stdout JSON-RPC, and asserts that requests reach the
// in-process testmock harness with the expected shape. This is the first
// piece of Area 3.2 — Plugin Subprocess Integration Tests — and is the
// canonical pattern for the deepseek and Nexus E2E counterparts.
//
// The test is gated behind testing.Short() so the unit-test loop stays
// fast; CI runs `go test -run 'TestInteg|TestE2E'` to exercise the full
// flow.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/internal/testmock"
)

// integTimeout caps the entire integ test (build + spawn + several RPCs).
// The slowest step is `go build` which can take a few seconds the first
// time; subsequent runs hit the build cache and finish in <1s.
const integTimeout = 60 * time.Second

// rpcTimeout caps a single request/response cycle so a plugin that
// silently hangs after handling the request surfaces a fast failure
// instead of stalling the whole integ suite up to integTimeout.
// [DS-AUDIT 3.2.B Finding 2]
const rpcTimeout = 15 * time.Second

func TestPluginJiraIntegration_HandshakeAndGetContext(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)
	h.Jira.SetIssue("MCPI-1", testmock.JiraIssue{
		Summary:     "Integration test ticket",
		Status:      "In Progress",
		Description: "Hello from the harness",
	})

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	// initialize → tools/list → tools/call get_context
	mustRPC(t, rpc, 1, "initialize", nil)
	listResp := mustRPC(t, rpc, 2, "tools/list", nil)
	tools := extractToolNames(t, listResp)
	if len(tools) != 1 || tools[0] != "jira" {
		t.Fatalf("tools/list reported %v want [\"jira\"]", tools)
	}

	resp := mustRPC(t, rpc, 3, "tools/call", map[string]any{
		"name": "jira",
		"arguments": map[string]any{
			"action":    "get_context",
			"ticket_id": "MCPI-1",
		},
	})
	text := extractTextContent(t, resp)
	if !strings.Contains(text, "Integration test ticket") {
		t.Errorf("get_context text=%q missing summary fixture", text)
	}
	if !strings.Contains(text, "In Progress") {
		t.Errorf("get_context text=%q missing status fixture", text)
	}
	if h.Jira.CallCount() < 1 {
		t.Errorf("mock CallCount=%d want at least 1", h.Jira.CallCount())
	}
}

func TestPluginJiraIntegration_GetContextNotFound(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)
	// No SetIssue call → /issue/X returns 404.

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	// Use a syntactically-valid but absent ticket so the request
	// reaches the mock (and gets a 404), rather than tripping the
	// dispatch-time format validator. [Phase E]
	resp := mustRPC(t, rpc, 2, "tools/call", map[string]any{
		"name": "jira",
		"arguments": map[string]any{
			"action":    "get_context",
			"ticket_id": "NOPE-9999",
		},
	})
	if errObj, ok := resp["error"].(map[string]any); !ok {
		t.Fatalf("expected error envelope, got result=%v", resp["result"])
	} else if msg, _ := errObj["message"].(string); !strings.Contains(strings.ToLower(msg), "not found") {
		t.Errorf("error message %q does not mention not-found", msg)
	}
}

func TestPluginJiraIntegration_TransitionFlipsStatus(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)
	h.Jira.SetIssue("MCPI-2", testmock.JiraIssue{Summary: "x", Status: "To Do"})
	h.Jira.SetTransitions("MCPI-2", []testmock.JiraTransition{
		{ID: "11", Name: "Start", ToStatus: "In Progress"},
		{ID: "21", Name: "Mark Done", ToStatus: "Done"},
	})

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	mustRPC(t, rpc, 2, "tools/call", map[string]any{
		"name": "jira",
		"arguments": map[string]any{
			"action":             "transition",
			"ticket_id":          "MCPI-2",
			"target_status":      "Done",
			"resolution_comment": "integ-tested",
		},
	})

	// Verify the mock now reports the issue as Done.
	getResp := mustRPC(t, rpc, 3, "tools/call", map[string]any{
		"name": "jira",
		"arguments": map[string]any{
			"action":    "get_context",
			"ticket_id": "MCPI-2",
		},
	})
	text := extractTextContent(t, getResp)
	if !strings.Contains(text, "Done") {
		t.Errorf("post-transition text=%q does not contain Done status", text)
	}
}

// TestPluginJiraIntegration_CreateIssue verifies the create_issue
// action lands on the testmock's POST /issue endpoint and returns the
// generated key in the MCP response. [Area 1.3.B]
func TestPluginJiraIntegration_CreateIssue(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	resp := mustRPC(t, rpc, 2, "tools/call", map[string]any{
		"name": "jira",
		"arguments": map[string]any{
			"action":      "create_issue",
			"summary":     "[FEATURE][CORE] integ-tested create",
			"issue_type":  "Story",
			"labels":      []string{"FEATURE", "CORE"},
			"description": "Body",
		},
	})
	text := extractTextContent(t, resp)
	// Mock returns synthesized key (typically "MOCK-1"); the response
	// from the plugin includes that key wrapped in human-readable
	// markdown. Just verify SOMETHING that looks like a Jira key.
	if !strings.Contains(text, "-") {
		t.Errorf("create_issue response %q missing issue key", text)
	}
	if h.Jira.CallCount() < 1 {
		t.Errorf("mock saw 0 calls for create_issue")
	}
}

// TestPluginJiraIntegration_RateLimitPropagation verifies that when
// the testmock returns 429, the plugin surfaces a clean MCP error
// rather than crashing or silently swallowing the limit. [Area 1.3.B]
func TestPluginJiraIntegration_RateLimitPropagation(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)
	h.Jira.SetIssue("MCPI-99", testmock.JiraIssue{Summary: "x", Status: "To Do"})
	h.Jira.SetRateLimit(true) // every request → 429

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	resp := mustRPC(t, rpc, 2, "tools/call", map[string]any{
		"name": "jira",
		"arguments": map[string]any{
			"action":    "get_context",
			"ticket_id": "MCPI-99",
		},
	})
	if errObj, ok := resp["error"].(map[string]any); !ok {
		t.Fatalf("expected error envelope on 429, got result=%v", resp["result"])
	} else if msg, _ := errObj["message"].(string); msg == "" {
		t.Errorf("error envelope had empty message")
	}
}

// ── helpers ────────────────────────────────────────────────────────────

// rpcChannel wraps the spawned plugin's stdin/stdout in a JSON encoder /
// scanner pair. The plugin does not send unsolicited notifications, so a
// strict request/response loop is sufficient.
type rpcChannel struct {
	enc     *json.Encoder
	scanner *bufio.Scanner
}

// skipIfShortOrWindows centralises the platform/short-mode guard, opts the
// test into parallel execution (each test has its own t.TempDir + spawned
// process, so there's no shared state), and registers a 30s budget cleanup
// that fails the test if the integ pipeline regressed wall-clock-wise.
// [Area 3.3.C]
func skipIfShortOrWindows(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test — skipped under -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("plugin subprocess integration tests are POSIX-only")
	}
	t.Parallel()
	start := time.Now()
	t.Cleanup(func() {
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Errorf("integ test exceeded 30s budget (took %s) — split or speed up", elapsed)
		}
	})
}

// buildPluginBinary compiles the plugin's main package into a tmp dir
// owned by the test. Returns the absolute path to the built binary.
func buildPluginBinary(t *testing.T, pkgPath string) string {
	t.Helper()
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "neo-plugin-jira")
	cmd := exec.Command("go", "build", "-o", binPath, pkgPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkgPath, err, out)
	}
	return binPath
}

// startPlugin spawns the binary with the harness env + an isolated HOME
// (so the plugin does not pick up an operator's real ~/.neo/plugins/jira.json
// and falls into the legacy env-var path). Returns a channel for sending
// JSON-RPC and a stop func that cleanly tears the subprocess down.
func startPlugin(t *testing.T, binPath string, h *testmock.Harness) (*rpcChannel, func()) {
	t.Helper()

	homeIso := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeIso, 0o700); err != nil {
		t.Fatalf("mkdir HOME: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), integTimeout)

	proc := exec.CommandContext(ctx, binPath)
	proc.Env = append([]string{
		"HOME=" + homeIso,
		"PATH=" + os.Getenv("PATH"),
	}, h.EnvSlice()...)

	stdin, err := proc.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdout: %v", err)
	}
	stderrBuf := &syncBuffer{}
	proc.Stderr = stderrBuf

	if err := proc.Start(); err != nil {
		cancel()
		t.Fatalf("start plugin: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)

	stop := func() {
		_ = stdin.Close()
		_ = proc.Wait()
		cancel()
		// Surface plugin stderr only when the test actually failed —
		// nothing in the success path needs it.
		if t.Failed() && stderrBuf.Len() > 0 {
			t.Logf("plugin stderr:\n%s", stderrBuf.String())
		}
	}

	return &rpcChannel{
		enc:     json.NewEncoder(stdin),
		scanner: scanner,
	}, stop
}

// mustRPC sends a single JSON-RPC request and decodes the next response
// line from the plugin's stdout. Fails the test on any I/O or decode error.
func mustRPC(t *testing.T, rpc *rpcChannel, id int, method string, params any) map[string]any {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	if err := rpc.enc.Encode(req); err != nil {
		t.Fatalf("encode %s: %v", method, err)
	}

	// Run the scanner read in a goroutine so we can timeout on a hung
	// plugin without waiting for the whole-suite integTimeout to fire.
	type scanResult struct {
		ok   bool
		body []byte
		err  error
	}
	done := make(chan scanResult, 1)
	go func() {
		ok := rpc.scanner.Scan()
		body := append([]byte(nil), rpc.scanner.Bytes()...)
		done <- scanResult{ok: ok, body: body, err: rpc.scanner.Err()}
	}()

	select {
	case res := <-done:
		if !res.ok {
			err := res.err
			if err == nil {
				err = io.EOF
			}
			t.Fatalf("scan response for %s: %v", method, err)
		}
		var resp map[string]any
		if err := json.Unmarshal(res.body, &resp); err != nil {
			t.Fatalf("decode %s response: %v\nbody: %s", method, err, res.body)
		}
		return resp
	case <-time.After(rpcTimeout):
		t.Fatalf("rpc %s timed out after %s — plugin may be hung", method, rpcTimeout)
		return nil
	}
}

func extractToolNames(t *testing.T, resp map[string]any) []string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %+v", resp)
	}
	tools, _ := result["tools"].([]any)
	out := make([]string, 0, len(tools))
	for _, raw := range tools {
		m, _ := raw.(map[string]any)
		if name, ok := m["name"].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

func extractTextContent(t *testing.T, resp map[string]any) string {
	t.Helper()
	if errObj, ok := resp["error"]; ok {
		t.Fatalf("expected result, got error: %+v", errObj)
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("response had empty content: %+v", resp)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if text == "" {
		t.Fatalf("first content item had empty text: %+v", first)
	}
	return text
}

// syncBuffer is a minimal thread-safe bytes.Buffer for capturing the
// plugin's stderr without racing with the goroutine that os/exec uses
// to drain the pipe.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}

func (s *syncBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

