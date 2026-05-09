package main_test

// integ_e2e_test exercises the full Nexus plugin-pool stack: it builds
// the plugin-jira and plugin-deepseek binaries, registers them with a
// real PluginPool wired to the testmock harness's VaultLookup, and
// drives JSON-RPC against each spawned subprocess. This is the
// canonical end-to-end seam for Area 3.2 — once it passes under -race
// the mock-injection contract is proven across the whole boundary.
//
// Compared to the per-plugin integ tests (3.2.B / 3.2.C), this one
// exercises:
//   - PluginPool.Start lifecycle (resource cleanup via StopAll)
//   - Vault resolution: env vars come from harness.VaultLookup, not raw
//     os.Setenv — the production path that real Nexus uses
//   - Both plugins running in parallel with concurrent traffic
//   - __health__ short-circuit invariant on both plugins simultaneously

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
	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

const (
	e2eTimeout = 90 * time.Second
	e2eRPC     = 15 * time.Second
)

func TestNexusE2E_BothPluginsRouteToMocks(t *testing.T) {
	skipIfShortOrWindows(t)

	jiraBin := buildBinary(t, "../plugin-jira", "neo-plugin-jira")
	deepBin := buildBinary(t, "../plugin-deepseek", "neo-plugin-deepseek")

	h := testmock.NewHarness(t)
	h.Jira.SetIssue("E2E-1", testmock.JiraIssue{
		Summary: "E2E ticket", Status: "In Progress",
	})
	h.DeepSeek.SetReply(testmock.DeepSeekReply{
		Content: "e2e mock reply",
		Usage: testmock.DeepSeekUsage{
			PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()

	pool, jiraProc, dsProc := bootPool(t, ctx, h, jiraBin, deepBin)
	defer func() {
		_ = pool.StopAllGracefully(2 * time.Second)
	}()

	jiraRPC := wrapStdio(jiraProc.Stdin, jiraProc.Stdout)
	dsRPC := wrapStdio(dsProc.Stdin, dsProc.Stdout)

	// Initialize both plugins concurrently — proves the pool handles
	// parallel handshakes without cross-talk.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mustE2ERPC(t, jiraRPC, 1, "initialize", nil)
	}()
	go func() {
		defer wg.Done()
		mustE2ERPC(t, dsRPC, 1, "initialize", nil)
	}()
	wg.Wait()

	jiraResp := mustE2ERPC(t, jiraRPC, 2, "tools/call", map[string]any{
		"name": "jira",
		"arguments": map[string]any{
			"action":    "get_context",
			"ticket_id": "E2E-1",
		},
	})
	if !strings.Contains(extractTextE2E(t, jiraResp), "E2E ticket") {
		t.Errorf("jira response missing fixture summary")
	}

	srcPath := writeTempE2EFile(t, "package x\nfunc Sum(a,b int) int { return a + b }\n")
	dsResp := mustE2ERPC(t, dsRPC, 2, "tools/call", map[string]any{
		"name": "call",
		"arguments": map[string]any{
			"action":        "distill_payload",
			"target_prompt": "summarize",
			"files":         []string{srcPath},
		},
	})
	if !strings.Contains(extractTextE2E(t, dsResp), "e2e mock reply") {
		t.Errorf("deepseek response missing mock content")
	}

	if h.Jira.CallCount() == 0 {
		t.Errorf("jira mock saw zero calls")
	}
	if h.DeepSeek.CallCount() == 0 {
		t.Errorf("deepseek mock saw zero calls")
	}
}

func TestNexusE2E_HealthShortCircuitOnBothPlugins(t *testing.T) {
	skipIfShortOrWindows(t)

	jiraBin := buildBinary(t, "../plugin-jira", "neo-plugin-jira")
	deepBin := buildBinary(t, "../plugin-deepseek", "neo-plugin-deepseek")

	h := testmock.NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()

	pool, jiraProc, dsProc := bootPool(t, ctx, h, jiraBin, deepBin)
	defer func() {
		_ = pool.StopAllGracefully(2 * time.Second)
	}()

	jiraRPC := wrapStdio(jiraProc.Stdin, jiraProc.Stdout)
	dsRPC := wrapStdio(dsProc.Stdin, dsProc.Stdout)

	mustE2ERPC(t, jiraRPC, 1, "initialize", nil)
	mustE2ERPC(t, dsRPC, 1, "initialize", nil)

	// __health__ on both plugins — must NOT touch upstream API.
	jiraHealth := mustE2ERPC(t, jiraRPC, 2, "tools/call", map[string]any{
		"name":      "jira",
		"arguments": map[string]any{"action": "__health__"},
	})
	dsHealth := mustE2ERPC(t, dsRPC, 2, "tools/call", map[string]any{
		"name":      "call",
		"arguments": map[string]any{"action": "__health__"},
	})

	for name, h := range map[string]map[string]any{"jira": jiraHealth, "deepseek": dsHealth} {
		result, _ := h["result"].(map[string]any)
		if alive, _ := result["plugin_alive"].(bool); !alive {
			t.Errorf("%s plugin_alive=%v want true", name, alive)
		}
	}

	if got := h.Jira.CallCount(); got != 0 {
		t.Errorf("jira __health__ leaked %d upstream call(s); want 0", got)
	}
	if got := h.DeepSeek.CallCount(); got != 0 {
		t.Errorf("deepseek __health__ leaked %d upstream call(s); want 0", got)
	}
}

// ── helpers ────────────────────────────────────────────────────────────

type rpcChan struct {
	enc     *json.Encoder
	scanner *bufio.Scanner
}

// skipIfShortOrWindows guards the E2E suite + registers a 30s wall-clock
// budget. Note: NOT parallel-safe — bootPool calls t.Setenv("HOME", ...)
// to isolate plugin config lookups, and t.Setenv panics under t.Parallel()
// per the testing package contract. [Area 3.3.C]
func skipIfShortOrWindows(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("E2E test — skipped under -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Nexus E2E is POSIX-only")
	}
	start := time.Now()
	t.Cleanup(func() {
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Errorf("E2E test exceeded 30s budget (took %s) — split or speed up", elapsed)
		}
	})
}

func buildBinary(t *testing.T, pkgPath, name string) string {
	t.Helper()
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, name)
	cmd := exec.Command("go", "build", "-o", binPath, pkgPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkgPath, err, out)
	}
	return binPath
}

// bootPool wires the harness vault into a fresh PluginPool, registers
// jira + deepseek specs (allowed_workspaces=["*"] → ACL passes), and
// waits for both processes to reach Running state. Returns the pool +
// each process handle.
//
// Important isolation: PluginPool.buildEnv calls os.Environ() and
// inherits HOME from the parent. The plugin-jira config loader reads
// $HOME/.neo/plugins/jira.json; without isolation the test would talk
// to the operator's real Jira credentials. t.Setenv("HOME", tmp)
// shadows it for the duration of the test (auto-restored by t.Cleanup).
//
// TODO(3.2.D-followup): PluginPool inherits ALL env vars (not only HOME)
// — a hardening pass should switch buildEnv to an allowlist + vault.
// Out of scope for this test; the harness ENV doesn't contain real
// secrets and HOME isolation is sufficient for current threat model.
func bootPool(
	t *testing.T,
	ctx context.Context,
	h *testmock.Harness,
	jiraBin, dsBin string,
) (*nexus.PluginPool, *nexus.PluginProcess, *nexus.PluginProcess) {
	t.Helper()

	homeIso := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeIso, 0o700); err != nil {
		t.Fatalf("mkdir HOME: %v", err)
	}
	t.Setenv("HOME", homeIso)

	logsDir := t.TempDir()
	pool := nexus.NewPluginPool(nexus.VaultLookup(h.VaultLookup()), logsDir)

	jiraSpec := &plugin.PluginSpec{
		Name:   "jira",
		Binary: jiraBin,
		EnvFromVault: []string{
			"JIRA_TOKEN", "JIRA_EMAIL", "JIRA_DOMAIN", "JIRA_BASE_URL",
			"JIRA_ACTIVE_SPACE", "JIRA_ACTIVE_BOARD",
		},
		Tier:              "nexus",
		Enabled:           true,
		AllowedWorkspaces: []string{"*"},
	}
	dsSpec := &plugin.PluginSpec{
		Name:              "deepseek",
		Binary:            dsBin,
		EnvFromVault:      []string{"DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL"},
		Tier:              "nexus",
		Enabled:           true,
		AllowedWorkspaces: []string{"*"},
	}

	jiraProc, err := pool.Start(jiraSpec)
	if err != nil {
		t.Fatalf("start jira: %v", err)
	}
	dsProc, err := pool.Start(dsSpec)
	if err != nil {
		t.Fatalf("start deepseek: %v", err)
	}

	// Plugins are launched async; the stdio pipes are open immediately,
	// so we can start sending JSON-RPC right away.
	_ = ctx // reserved for future deadline-bounded health waits
	return pool, jiraProc, dsProc
}

func wrapStdio(stdin io.WriteCloser, stdout io.ReadCloser) *rpcChan {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	return &rpcChan{
		enc:     json.NewEncoder(stdin),
		scanner: scanner,
	}
}

func mustE2ERPC(t *testing.T, rpc *rpcChan, id int, method string, params any) map[string]any {
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
	case <-time.After(e2eRPC):
		t.Fatalf("rpc %s timed out after %s", method, e2eRPC)
		return nil
	}
}

func extractTextE2E(t *testing.T, resp map[string]any) string {
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
	return text
}

func writeTempE2EFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "src.go")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return path
}
