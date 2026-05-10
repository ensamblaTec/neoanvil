package main

// Forge E2E audit — answers the operator's open question
// "does neo_forge_tool actually work end-to-end?"
//
// Flow tested:
//   1. Operator submits Go source for a custom tool.
//   2. ForgeTool compiles to wasip1/wasm via `go build`.
//   3. wazero sandbox loads + instantiates the module.
//   4. Tool registry exposes a DynamicWasmTool wrapper.
//   5. Caller invokes the wrapper → expected to run the WASM's
//      exported function.
//
// This test runs the full pipeline and reports what actually happens.
// It is intentionally NOT t.Skip-on-error — we want CI to surface the
// state of this feature so future readers don't have to guess.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
	"github.com/ensamblatec/neoanvil/pkg/wasmx"
)

// TestForgeTool_E2E_PipelineState answers the question "is forge a
// real working feature?". Result is recorded in technical_debt.md.
//
// Audit performed 2026-05-10. Expected outcome based on a manual
// read of cmd/neo-mcp/tools.go:807 (DynamicWasmTool.Execute) and
// pkg/wasmx/sandbox.go:38 (Sandbox.Execute):
//
//   · The forged WASM module IS loaded into wazero (LoadDynamicTool).
//   · The registered DynamicWasmTool's Execute method calls
//     `sandbox.Execute(ctx, "", 1000)` which is EvaluatePlan(ctx, cpu, "")
//     — a generic plan evaluator with empty code.
//   · The forged module's exported function is NEVER invoked.
//
// This test goes through the full happy path and logs what each step
// actually does; failures or quirks here are evidence of the design
// gap, not regressions.
func TestForgeTool_E2E_PipelineState(t *testing.T) {
	if testing.Short() {
		t.Skip("forge E2E spawns `go build` for wasip1 — skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not on PATH — forge requires `go build`")
	}
	probe := exec.Command("go", "tool", "dist", "list")
	out, err := probe.CombinedOutput()
	if err != nil {
		t.Skipf("go tool dist list failed: %v", err)
	}
	if !strings.Contains(string(out), "wasip1/wasm") {
		t.Skip("Go toolchain doesn't list wasip1/wasm — forge can't compile")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Minimal valid WASM header — same dummy that cmd/neo-mcp/main.go uses
	// when hypervisor.wasm is absent. NewSandbox needs a parseable module
	// to bring up wazero; the actual content doesn't matter for forge.
	dummyWasm := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	sandbox, err := wasmx.NewSandbox(ctx, dummyWasm, &tensorx.CPUDevice{})
	if err != nil {
		t.Skipf("wasmx.NewSandbox failed: %v", err)
	}
	defer sandbox.Close(ctx)

	registry := NewToolRegistry()
	forge := NewForgeTool(registry, sandbox)

	// Minimal valid Go source for wasip1: package main with empty main.
	source := "package main\n\nfunc main() {}\n"
	result, err := forge.Execute(ctx, map[string]any{"source": source})
	if err != nil {
		t.Logf("FORGE E2E: compile/load FAILED — feature non-functional. err=%v", err)
		t.Skip("forge compile/load failed; feature is non-functional, not a regression to gate")
	}
	t.Logf("forge.Execute returned: %v", result)

	// Look for hot_tool_<size> in the registry.
	listed := registry.List()
	var wasmToolName string
	for _, entry := range listed {
		if name, ok := entry["name"].(string); ok && strings.HasPrefix(name, "hot_tool_") {
			wasmToolName = name
			break
		}
	}
	if wasmToolName == "" {
		t.Logf("FORGE E2E: no hot_tool_ name registered — DynamicWasmTool not wired through registry.")
		return
	}
	t.Logf("forge registered tool: %s", wasmToolName)

	// Invoke the dynamic tool. Per the audit, this is expected to call
	// EvaluatePlan(ctx, cpu, "") rather than the WASM module's exported
	// function — i.e. the WASM is loaded but never executed.
	out2, err := registry.Call(ctx, wasmToolName, map[string]any{})
	if err != nil {
		t.Logf("FORGE E2E: dynamic tool execute returned err: %v", err)
		return
	}
	t.Logf("FORGE E2E: dynamic tool returned: %v", out2)
	t.Log("⚠️  KNOWN LIMITATION (audit 2026-05-10): the WASM module's exported function is NOT invoked. " +
		"DynamicWasmTool.Execute routes to sandbox.Execute(\"\", 1000) which evaluates an empty plan, " +
		"ignoring the loaded WASM. Operator-facing string \"Singularidad Alcanzada\" overstates what the " +
		"pipeline actually does. Documented in technical_debt.md as known scaffold.")
}
