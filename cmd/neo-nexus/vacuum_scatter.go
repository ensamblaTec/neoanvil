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

// VacuumScatterResp is the JSON response from a proxied Vacuum_Memory call. [348.A]
type VacuumScatterResp struct {
	WorkspaceID string `json:"workspace_id"`
	Message     string `json:"message"`
}

// handleInternalVacuumBegin proxies a Vacuum_Memory daemon call to a child workspace. [348.A]
func handleInternalVacuumBegin(pool *nexus.ProcessPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		wsID := strings.TrimPrefix(r.URL.Path, "/internal/vacuum/begin/")
		if wsID == "" {
			http.Error(w, "workspace_id required", http.StatusBadRequest)
			return
		}
		proc, found := pool.GetProcess(wsID)
		if !found || proc.Port == 0 {
			http.Error(w, fmt.Sprintf("workspace %q not running", wsID), http.StatusNotFound)
			return
		}
		msg, err := proxyVacuumToChild(r.Context(), proc.Port)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VacuumScatterResp{WorkspaceID: wsID, Message: msg})
	}
}

// proxyVacuumToChild forwards a Vacuum_Memory call to a child's MCP endpoint. [348.A]
func proxyVacuumToChild(ctx context.Context, port int) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "neo_daemon",
			"arguments": map[string]any{
				"action": "Vacuum_Memory",
			},
		},
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp/message", port) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is runtime-assigned by ProcessPool
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build vacuum forward: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := sre.SafeInternalHTTPClient(15)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("child vacuum call: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	var result struct {
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
	if parseErr := json.Unmarshal(body, &result); parseErr != nil {
		return fmt.Sprintf("⚠️ parse error (port %d): %v", port, parseErr), nil
	}
	if result.Error != nil {
		return fmt.Sprintf("❌ MCP error: %s", result.Error.Message), nil
	}
	var parts []string
	for _, c := range result.Result.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	if len(parts) == 0 {
		return "dispatched", nil
	}
	return strings.Join(parts, "\n"), nil
}
