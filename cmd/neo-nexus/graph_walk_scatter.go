// graph_walk_scatter.go — Nexus /internal/graph_walk/{workspace_id} endpoint.
// Proxies a neo_radar GRAPH_WALK call to the named child workspace's MCP.
// [347.A] PILAR LXIV — Cross-Workspace GRAPH_WALK via http_boundary edges.
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

// GraphWalkScatterReq is the request body for POST /internal/graph_walk/{workspace_id}.
type GraphWalkScatterReq struct {
	StartSym    string `json:"start_sym"`
	MaxDepth    int    `json:"max_depth"`
	EdgeKind    string `json:"edge_kind"`
	BypassCache bool   `json:"bypass_cache,omitempty"`
}

// GraphWalkScatterResp is the response from POST /internal/graph_walk/{workspace_id}.
type GraphWalkScatterResp struct {
	WorkspaceID string `json:"workspace_id"`
	Body        string `json:"body"`
}

// handleInternalGraphWalk proxies a GRAPH_WALK call to the named child workspace's MCP.
// POST /internal/graph_walk/{workspace_id}
func handleInternalGraphWalk(pool *nexus.ProcessPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		wsID := strings.TrimPrefix(r.URL.Path, "/internal/graph_walk/")
		if wsID == "" {
			http.Error(w, "workspace_id required in path", http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var req GraphWalkScatterReq
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.StartSym == "" {
			http.Error(w, "start_sym required", http.StatusBadRequest)
			return
		}
		if req.MaxDepth <= 0 {
			req.MaxDepth = 2
		}

		proc, found := pool.GetProcess(wsID)
		if !found || proc.Port == 0 {
			http.Error(w, fmt.Sprintf("workspace %q not found or not running", wsID), http.StatusNotFound)
			return
		}

		walkBody, proxyErr := proxyGraphWalkToChild(r.Context(), proc.Port, req)
		if proxyErr != nil {
			http.Error(w, proxyErr.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GraphWalkScatterResp{
			WorkspaceID: wsID,
			Body:        walkBody,
		})
	}
}

// proxyGraphWalkToChild forwards a GRAPH_WALK call to a child's /mcp/message endpoint.
func proxyGraphWalkToChild(ctx context.Context, port int, req GraphWalkScatterReq) (string, error) {
	args := map[string]any{
		"intent":    "GRAPH_WALK",
		"target":    req.StartSym,
		"max_depth": req.MaxDepth,
	}
	if req.EdgeKind != "" {
		args["edge_kind"] = req.EdgeKind
	}
	if req.BypassCache {
		args["bypass_cache"] = true
	}
	mcpPayload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "neo_radar",
			"arguments": args,
		},
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/mcp/message", port) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is runtime-assigned by ProcessPool
	mcpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(mcpPayload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	mcpReq.Header.Set("Content-Type", "application/json")

	client := sre.SafeInternalHTTPClient(10)
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
