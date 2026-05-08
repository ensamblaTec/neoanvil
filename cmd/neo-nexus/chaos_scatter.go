// chaos_scatter.go — Nexus /internal/chaos/{workspace_id} endpoint.
// Routes a neo_chaos_drill call to the owning child workspace's MCP.
// [346.A] PILAR LXIV — Cross-Workspace Chaos Drill.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// ChaosScatterReq is the request body for POST /internal/chaos/{workspace_id}.
type ChaosScatterReq struct {
	Target          string `json:"target"`
	AggressionLevel int    `json:"aggression_level"`
	InjectFaults    bool   `json:"inject_faults"`
}

// ChaosScatterResp is the response from POST /internal/chaos/{workspace_id}.
type ChaosScatterResp struct {
	WorkspaceID string `json:"workspace_id"`
	Report      string `json:"report"`
}

// handleInternalChaos routes a chaos drill call to the named child workspace's MCP.
// POST /internal/chaos/{workspace_id}
func handleInternalChaos(pool *nexus.ProcessPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		wsID := strings.TrimPrefix(r.URL.Path, "/internal/chaos/")
		if wsID == "" {
			http.Error(w, "workspace_id required in path", http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var req ChaosScatterReq
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Target == "" {
			http.Error(w, "target required", http.StatusBadRequest)
			return
		}

		proc, found := pool.GetProcess(wsID)
		if !found || proc.Port == 0 {
			http.Error(w, fmt.Sprintf("workspace %q not found or not running", wsID), http.StatusNotFound)
			return
		}

		report, proxyErr := proxyChaosToChild(r.Context(), proc.Port, req)
		if proxyErr != nil {
			http.Error(w, proxyErr.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChaosScatterResp{
			WorkspaceID: wsID,
			Report:      report,
		})
	}
}

// proxyChaosToChild sends an MCP tools/call to a child's /mcp/message endpoint
// and returns the drill report text.
func proxyChaosToChild(ctx context.Context, port int, req ChaosScatterReq) (string, error) {
	aggrLevel := req.AggressionLevel
	if aggrLevel < 1 || aggrLevel > 10 {
		aggrLevel = 5
	}
	mcpPayload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "neo_chaos_drill",
			"arguments": map[string]any{
				"target":           req.Target,
				"aggression_level": aggrLevel,
				"inject_faults":    req.InjectFaults,
			},
		},
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/mcp/message", port) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is runtime-assigned by ProcessPool
	mcpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(mcpPayload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	mcpReq.Header.Set("Content-Type", "application/json")

	client := sre.SafeInternalHTTPClient(35) // 10s siege + 25s buffer
	resp, err := client.Do(mcpReq)
	if err != nil {
		return "", fmt.Errorf("child MCP call: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(resp.Body)

	var mcpResult struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	if parseErr := json.Unmarshal(respBody, &mcpResult); parseErr != nil {
		return fmt.Sprintf("⚠️ parse error from child port %d: %v", port, parseErr), nil
	}
	if mcpResult.Error != nil {
		return fmt.Sprintf("❌ MCP error from child: %s", mcpResult.Error.Message), nil
	}

	var parts []string
	for _, c := range mcpResult.Result.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}
