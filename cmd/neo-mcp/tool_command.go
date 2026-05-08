// cmd/neo-mcp/tool_command.go — unified shell-command dispatcher.
// [Épica 239 + PILAR-XXXVIII/291.C/D]
//
// Actions:
//   run        → stage a command for human approval (risk-scored)
//   approve    → execute a previously-staged command by ticket ID
//   kill       → terminate a background process started via run, or a mock server by ID
//   mock_start → launch an ephemeral HTTP mock server from contract graph [291.C]

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/nexus"
)

// activeMocks tracks live MockServer instances keyed by "mock-<uuid>". [291.C]
var (
	activeMocksMu sync.Mutex
	activeMocks   = make(map[string]*MockServer)
)

// mockPortBase and mockPortSize are wired at boot from nexus config or defaults. [291.B]
var mockPortBase = 34800
var mockPortSize = 100

type CommandTool struct {
	run        *RunCommandTool
	approve    *ApproveCommandTool
	kill       *KillCommandTool
	workspace  string
	contracts  func() []cpg.ContractNode // lazy supplier, may be nil
}

func (t *CommandTool) Name() string { return "neo_command" }

func (t *CommandTool) Description() string {
	return "SRE Tool: Unified shell-command dispatcher. `action: run` stages a command for human authorization (requires risk_score + blast_radius_analysis). `action: approve` executes a previously-staged ticket. `action: kill` terminates a background process by PID or mock server by ID. `action: mock_start` launches an ephemeral HTTP mock server from the contract graph [291]. Replaces neo_run_command / neo_approve_command / neo_kill_command."
}

func (t *CommandTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Which command operation.",
				"enum":        []string{"run", "approve", "kill", "mock_start"},
			},
			// run
			"command": map[string]any{
				"type":        "string",
				"description": "[run] Exact shell command to stage. Append `// turbo` for auto-approval.",
			},
			"risk_score": map[string]any{
				"type":        "integer",
				"description": "[run] Danger scale 1 (safe) … 10 (system destruction).",
			},
			"blast_radius_analysis": map[string]any{
				"type":        "string",
				"description": "[run] Briefly explain what happens if this command fails or is malicious.",
			},
			// approve
			"ticket_id": map[string]any{
				"type":        "string",
				"description": "[approve] The Ticket ID returned by a prior `run` action.",
			},
			// kill
			"pid": map[string]any{
				"description": "[kill] PID of the background process (integer), or mock server ID string (e.g. 'mock-abc') returned by a prior mock_start.",
			},
			// mock_start
			"endpoints": map[string]any{
				"type":        "array",
				"description": "[mock_start] Optional path filter (e.g. [\"/api/users\"]). Empty = all resolved contracts.",
				"items":       map[string]any{"type": "string"},
			},
			"port": map[string]any{
				"type":        "integer",
				"description": "[mock_start] Bind port. 0 = auto-assign from mock port range (34800-34899 by default).",
			},
		},
		Required: []string{"action"},
	}
}

func (t *CommandTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	action, _ := args["action"].(string)
	switch action {
	case "run":
		return t.run.Execute(ctx, args)
	case "approve":
		return t.approve.Execute(ctx, args)
	case "kill":
		return t.execKill(ctx, args)
	case "mock_start":
		return t.execMockStart(args)
	case "":
		return nil, fmt.Errorf("neo_command: action is required (one of: run, approve, kill, mock_start)")
	default:
		return nil, fmt.Errorf("neo_command: unknown action %q — valid: run, approve, kill, mock_start", action)
	}
}

// execKill dispatches to mock server stop when pid is a "mock-…" string, else delegates to KillCommandTool. [291.D]
func (t *CommandTool) execKill(ctx context.Context, args map[string]any) (any, error) {
	switch v := args["pid"].(type) {
	case string:
		if strings.HasPrefix(v, "mock-") {
			return t.stopMock(v)
		}
		return nil, fmt.Errorf("neo_command kill: string pid only valid for mock IDs (prefix 'mock-')")
	case float64:
		return t.kill.Execute(ctx, args)
	default:
		return nil, fmt.Errorf("neo_command kill: pid must be an integer or mock ID string")
	}
}

// stopMock stops a MockServer by ID. [291.D]
func (t *CommandTool) stopMock(mockID string) (any, error) {
	activeMocksMu.Lock()
	ms, ok := activeMocks[mockID]
	if ok {
		delete(activeMocks, mockID)
	}
	activeMocksMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("neo_command kill: mock server %q not found (already stopped?)", mockID)
	}
	ms.Stop()
	return map[string]any{"stopped": mockID, "port": ms.Port()}, nil
}

// execMockStart launches an ephemeral mock server. [291.C]
func (t *CommandTool) execMockStart(args map[string]any) (any, error) {
	// Parse endpoints filter.
	var endpoints []string
	if raw, ok := args["endpoints"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				endpoints = append(endpoints, s)
			}
		}
	}

	// Parse port.
	port := 0
	if pv, ok := args["port"].(float64); ok && pv > 0 {
		port = int(pv)
	}
	if port == 0 {
		port = nexus.AllocateMockPort(mockPortBase, mockPortSize)
		// If no port free in range, fall back to OS-assigned (0).
	}

	// Resolve contracts.
	var contracts []cpg.ContractNode
	if t.contracts != nil {
		contracts = t.contracts()
	}

	ms := NewMockServer(contracts, endpoints)

	// Add schema for each matched contract if possible (best-effort).
	for _, c := range ms.contracts {
		if c.BackendFn != "" {
			if schema, _, err := cpg.ExtractRequestSchema(t.workspace, c.BackendFn); err == nil {
				ms.AddSchema(c.Path, schema)
			}
		}
	}

	boundPort, err := ms.Start(port)
	if err != nil {
		return nil, fmt.Errorf("neo_command mock_start: %w", err)
	}

	var b [4]byte
	_, _ = rand.Read(b[:])
	mockID := "mock-" + hex.EncodeToString(b[:])
	activeMocksMu.Lock()
	activeMocks[mockID] = ms
	activeMocksMu.Unlock()

	return map[string]any{
		"mock_id": mockID,
		"port":    boundPort,
		"routes":  len(ms.contracts),
		"health":  fmt.Sprintf("http://127.0.0.1:%d/__mock/health", boundPort),
	}, nil
}
