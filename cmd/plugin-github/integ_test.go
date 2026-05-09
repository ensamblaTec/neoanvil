package main_test

// integ_test spawns the real plugin-github binary as a subprocess
// and drives it through stdin/stdout JSON-RPC against the testmock
// harness. Mirrors cmd/plugin-jira/integ_test.go shape so the future
// shared infra (drain, audit, multi-tenant) migrates with minimal diff.
// [Area 2.3.C]
//
// Gated behind testing.Short() so the unit-test loop stays fast; CI
// runs `make test-integration` to exercise the full pipeline.

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

const (
	integTimeout = 60 * time.Second
	rpcTimeout   = 15 * time.Second
)

func TestPluginGitHubIntegration_HandshakeAndListPRs(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)
	h.GitHub.SetPRs("acme", "neoanvil", []testmock.GitHubPR{
		{Number: 1, Title: "[FEATURE][CORE] integ test PR", State: "open", User: "alice", Head: "feat", Base: "main"},
		{Number: 2, Title: "[BUG][CORE] fix memory leak", State: "open", User: "bob", Head: "fix-leak", Base: "main"},
	})
	// Token defaults to "fake-github-token" (testmock + harness env
	// agree). No SetToken() needed.

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	listResp := mustRPC(t, rpc, 2, "tools/list", nil)
	tools := extractToolNames(t, listResp)
	if len(tools) != 1 || tools[0] != "github" {
		t.Fatalf("tools/list reported %v want [\"github\"]", tools)
	}

	resp := mustRPC(t, rpc, 3, "tools/call", map[string]any{
		"name": "github",
		"arguments": map[string]any{
			"action": "list_prs",
			"owner":  "acme",
			"repo":   "neoanvil",
			"state":  "open",
		},
	})
	text := extractTextContent(t, resp)
	if !strings.Contains(text, "integ test PR") {
		t.Errorf("list_prs text=%q missing fixture summary", text)
	}
	if !strings.Contains(text, "alice") {
		t.Errorf("list_prs text=%q missing PR author", text)
	}
	if h.GitHub.CallCount() < 1 {
		t.Errorf("mock CallCount=%d want at least 1", h.GitHub.CallCount())
	}
}

func TestPluginGitHubIntegration_HealthShortCircuit(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)
	// Default token from testmock harness env.
	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	resp := mustRPC(t, rpc, 2, "tools/call", map[string]any{
		"name":      "github",
		"arguments": map[string]any{"action": "__health__"},
	})
	result, _ := resp["result"].(map[string]any)
	if alive, _ := result["plugin_alive"].(bool); !alive {
		t.Errorf("plugin_alive=%v want true", alive)
	}
	if got, _ := result["api_key_present"].(bool); !got {
		t.Errorf("api_key_present should be true with GITHUB_TOKEN env set")
	}
	// __health__ MUST NOT touch upstream API per PLUGIN-HEALTH-CONTRACT.
	if got := h.GitHub.CallCount(); got != 0 {
		t.Errorf("__health__ leaked %d upstream call(s); want 0", got)
	}
}

func TestPluginGitHubIntegration_CrossRefRegex(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	resp := mustRPC(t, rpc, 2, "tools/call", map[string]any{
		"name": "github",
		"arguments": map[string]any{
			"action": "cross_ref",
			"text":   "Fixes MCPI-1, also see MCPI-2 and ABC-99 (and MCPI-1 again)",
		},
	})
	text := extractTextContent(t, resp)
	// cross_ref returns JSON-as-text — verify dedup + count.
	var parsed struct {
		Keys  []string `json:"keys"`
		Count int      `json:"count"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("cross_ref produced non-JSON: %v\n%s", err, text)
	}
	if parsed.Count != 3 {
		t.Errorf("count=%d want 3 (MCPI-1, MCPI-2, ABC-99)", parsed.Count)
	}
	// cross_ref is local-only — should NOT hit the GitHub API.
	if got := h.GitHub.CallCount(); got != 0 {
		t.Errorf("cross_ref hit upstream %d times; want 0", got)
	}
}

// ── helpers ────────────────────────────────────────────────────────────
// Mirror cmd/plugin-jira/integ_test.go helpers — same shapes, GitHub
// env injection.

type rpcChannel struct {
	enc     *json.Encoder
	scanner *bufio.Scanner
}

// skipIfShortOrWindows mirrors the plugin-jira helper. Opts the test
// into parallel execution + 30s wall-clock budget. [3.3.A/C parity]
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
			t.Errorf("integ test exceeded 30s budget (took %s)", elapsed)
		}
	})
}

func buildPluginBinary(t *testing.T, pkgPath string) string {
	t.Helper()
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "neo-plugin-github")
	cmd := exec.Command("go", "build", "-o", binPath, pkgPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkgPath, err, out)
	}
	return binPath
}

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
		if t.Failed() && stderrBuf.Len() > 0 {
			t.Logf("plugin stderr:\n%s", stderrBuf.String())
		}
	}

	return &rpcChannel{
		enc:     json.NewEncoder(stdin),
		scanner: scanner,
	}, stop
}

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
		t.Fatalf("rpc %s timed out after %s", method, rpcTimeout)
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
