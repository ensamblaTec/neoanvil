package main_test

// integ_test for plugin-deepseek mirrors the structure of
// cmd/plugin-jira/integ_test.go: build the production binary, spawn it
// as a subprocess, drive JSON-RPC against the testmock harness, and
// assert end-to-end behavior. Gated on testing.Short() and skipped on
// Windows.

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

func TestPluginDeepSeekIntegration_Distill(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)
	h.DeepSeek.SetReply(testmock.DeepSeekReply{
		Content: "compressed payload summary",
		Usage: testmock.DeepSeekUsage{
			PromptTokens:          5000,
			CompletionTokens:      120,
			TotalTokens:           5120,
			PromptCacheHitTokens:  4500,
			PromptCacheMissTokens: 500,
		},
	})

	// distill_payload requires real on-disk files — chunk before API call.
	srcPath := writeTempFile(t, "func add(a, b int) int { return a + b }\n", "go")

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	listResp := mustRPC(t, rpc, 2, "tools/list", nil)
	if names := extractToolNames(t, listResp); len(names) != 1 || names[0] != "call" {
		t.Fatalf("tools/list reported %v want [\"call\"]", names)
	}

	resp := mustRPC(t, rpc, 3, "tools/call", map[string]any{
		"name": "call",
		"arguments": map[string]any{
			"action":        "distill_payload",
			"target_prompt": "summarize the function",
			"files":         []string{srcPath},
		},
	})
	text := extractTextContent(t, resp)
	if !strings.Contains(text, "compressed payload summary") {
		t.Errorf("response text=%q missing mock content", text)
	}
	if h.DeepSeek.CallCount() == 0 {
		t.Errorf("mock CallCount=%d want at least 1", h.DeepSeek.CallCount())
	}
}

func TestPluginDeepSeekIntegration_HealthAction(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	resp := mustRPC(t, rpc, 2, "tools/call", map[string]any{
		"name":      "call",
		"arguments": map[string]any{"action": "__health__"},
	})
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("__health__ returned no result: %+v", resp)
	}
	if alive, _ := result["plugin_alive"].(bool); !alive {
		t.Errorf("plugin_alive=%v want true", alive)
	}
	tools, _ := result["tools_registered"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools_registered=%v want one entry", tools)
	}
	// __health__ must NOT touch the upstream API — Nexus relies on this
	// invariant to detect zombies cheaply.
	if h.DeepSeek.CallCount() != 0 {
		t.Errorf("__health__ leaked %d upstream call(s); want 0", h.DeepSeek.CallCount())
	}
}

func TestPluginDeepSeekIntegration_ErrorPropagation(t *testing.T) {
	skipIfShortOrWindows(t)

	binPath := buildPluginBinary(t, ".")
	h := testmock.NewHarness(t)
	// 429 rate-limit response from upstream. The plugin's dispatchAction
	// should surface this as text containing the upstream error string;
	// distill_payload aggregates per-chunk errors instead of bubbling
	// them up as MCP error envelopes, so we assert on the text payload.
	h.DeepSeek.SetReply(testmock.DeepSeekReply{Status: 429})
	srcPath := writeTempFile(t, "package x\n", "go")

	rpc, stop := startPlugin(t, binPath, h)
	defer stop()

	mustRPC(t, rpc, 1, "initialize", nil)
	resp := mustRPC(t, rpc, 2, "tools/call", map[string]any{
		"name": "call",
		"arguments": map[string]any{
			"action":        "distill_payload",
			"target_prompt": "anything",
			"files":         []string{srcPath},
		},
	})
	text := extractTextContent(t, resp)
	if !strings.Contains(text, "429") && !strings.Contains(strings.ToLower(text), "error") {
		t.Errorf("response %q does not surface upstream error", text)
	}
}

// writeTempFile writes content to a fresh tmp file with the given
// extension so distill_payload's chunker can pick it up. Returns the
// absolute path; cleanup is handled by t.TempDir().
func writeTempFile(t *testing.T, content, ext string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "src."+ext)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

// ── helpers ────────────────────────────────────────────────────────────

type rpcChannel struct {
	enc     *json.Encoder
	scanner *bufio.Scanner
}

func skipIfShortOrWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped under -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("plugin subprocess integration tests are POSIX-only")
	}
}

func buildPluginBinary(t *testing.T, pkgPath string) string {
	t.Helper()
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "neo-plugin-deepseek")
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
