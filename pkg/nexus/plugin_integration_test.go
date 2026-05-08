package nexus

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

// buildEchoBinary compiles cmd/plugin-echo into a temp dir and returns the
// resulting binary path. Compilation is the dominant cost of the
// integration test — ~1s on a warm cache. Skipped under -short.
func buildEchoBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "plugin-echo")
	cmd := exec.Command("go", "build", "-o", out, "github.com/ensamblatec/neoanvil/cmd/plugin-echo") //nolint:gosec // G204-LITERAL-BIN: literal "go" binary, args are fixed test fixtures.
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin-echo: %v\n%s", err, output)
	}
	return out
}

// TestPluginEcho_EndToEnd validates the full PILAR XXIII plugin pipeline:
//   1. Spawn cmd/plugin-echo via PluginPool (123.3)
//   2. MCP handshake via Client.Initialize (123.4)
//   3. Discover tools via Client.ListTools (123.4)
//   4. Aggregate with namespace prefix (123.4)
//   5. Invoke tools via Client.CallTool (123.4 + 123.7)
//   6. Graceful shutdown via Stop (123.5)
//
// Each verification step lives in a small helper to keep this orchestrator
// under the CC cap while preserving the linear narrative.
func TestPluginEcho_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test (-short)")
	}
	binary := buildEchoBinary(t)

	pool := NewPluginPool(nil, t.TempDir())
	proc := startEchoPlugin(t, pool, binary)
	t.Cleanup(func() { _ = pool.Stop("echo") })

	client := plugin.NewClient(proc.Stdin, proc.Stdout)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	verifyTools(t, ctx, client)
	verifyAggregator(t, ctx, client)
	verifyEchoCall(t, ctx, client)
	verifyVersionCall(t, ctx, client)

	_ = client.Close()
	verifyShutdown(t, pool, proc)
}

func startEchoPlugin(t *testing.T, pool *PluginPool, binary string) *PluginProcess {
	t.Helper()
	spec := &plugin.PluginSpec{
		Name:            "echo",
		Binary:          binary,
		Tier:            plugin.TierNexus,
		NamespacePrefix: "echo",
		Enabled:         true,
	}
	proc, err := pool.Start(spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return proc
}

func verifyTools(t *testing.T, ctx context.Context, c *plugin.Client) {
	t.Helper()
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools count=%d want 2: %+v", len(tools), tools)
	}
	wantNames := map[string]bool{"echo": false, "version": false}
	for _, tool := range tools {
		if _, ok := wantNames[tool.Name]; ok {
			wantNames[tool.Name] = true
		}
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("missing expected tool %q", name)
		}
	}
}

func verifyAggregator(t *testing.T, ctx context.Context, c *plugin.Client) {
	t.Helper()
	conn := plugin.Connected{Name: "echo", NamespacePrefix: "echo", Client: c}
	res := plugin.AggregateTools(ctx, []plugin.Connected{conn})
	if len(res.Errors) != 0 {
		t.Fatalf("aggregator errors: %v", res.Errors)
	}
	prefixed := make(map[string]bool)
	for _, nt := range res.Tools {
		prefixed[nt.PrefixedName()] = true
	}
	if !prefixed["echo/echo"] || !prefixed["echo/version"] {
		t.Errorf("aggregator output missing prefixed names, got %v", prefixed)
	}
}

func verifyEchoCall(t *testing.T, ctx context.Context, c *plugin.Client) {
	t.Helper()
	raw, err := c.CallTool(ctx, "echo", map[string]any{"text": "hello-from-nexus"})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hello-from-nexus" {
		t.Errorf("echo result=%+v want text=hello-from-nexus", resp.Content)
	}
}

func verifyVersionCall(t *testing.T, ctx context.Context, c *plugin.Client) {
	t.Helper()
	raw, err := c.CallTool(ctx, "version", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool version: %v", err)
	}
	if !contains(raw, "plugin-echo") {
		t.Errorf("version result %q does not contain plugin-echo", raw)
	}
}

func verifyShutdown(t *testing.T, pool *PluginPool, proc *PluginProcess) {
	t.Helper()
	if err := pool.Stop("echo"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-proc.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("plugin did not exit within 3s of Stop")
	}
}

func contains(b []byte, s string) bool {
	return len(b) > 0 && stringIndex(string(b), s) >= 0
}

func stringIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestPluginEcho_BinaryExists is a fast sanity check. Verifies that
// cmd/plugin-echo compiles. Runs even under -short to catch breakage early.
func TestPluginEcho_BinaryExists(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot resolve caller")
	}
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	mainGo := filepath.Join(repoRoot, "cmd", "plugin-echo", "main.go")
	cmd := exec.Command("go", "vet", mainGo) //nolint:gosec // G204-LITERAL-BIN: literal "go" binary, mainGo is repo-relative test fixture.
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet: %v\n%s", err, output)
	}
}
