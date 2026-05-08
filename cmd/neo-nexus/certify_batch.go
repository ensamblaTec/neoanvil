// certify_batch.go — Nexus /internal/certify/{workspace_id} endpoint.
// Routes a neo_sre_certify_mutation call to the owning child workspace's MCP.
// [345.A] PILAR LXIV — Cross-Workspace Certify.
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

// CertifyBatchReq is the request body for POST /internal/certify/{workspace_id}.
type CertifyBatchReq struct {
	MutatedFiles     []string `json:"mutated_files"`
	ComplexityIntent string   `json:"complexity_intent"`
	RollbackMode     string   `json:"rollback_mode"`
	DryRun           bool     `json:"dry_run"`
}

// CertifyBatchResp is the response from POST /internal/certify/{workspace_id}.
type CertifyBatchResp struct {
	Results     []string `json:"results"`
	WorkspaceID string   `json:"workspace_id"`
}

// handleInternalCertify routes a certify call to the named child workspace's MCP.
// POST /internal/certify/{workspace_id}
func handleInternalCertify(pool *nexus.ProcessPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		wsID := strings.TrimPrefix(r.URL.Path, "/internal/certify/")
		if wsID == "" {
			http.Error(w, "workspace_id required in path", http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var req CertifyBatchReq
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.MutatedFiles) == 0 {
			http.Error(w, "mutated_files required", http.StatusBadRequest)
			return
		}

		proc, found := pool.GetProcess(wsID)
		if !found || proc.Port == 0 {
			http.Error(w, fmt.Sprintf("workspace %q not found or not running", wsID), http.StatusNotFound)
			return
		}

		results, proxyErr := proxyCertifyToChild(r.Context(), proc.Port, req)
		if proxyErr != nil {
			http.Error(w, proxyErr.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CertifyBatchResp{
			Results:     results,
			WorkspaceID: wsID,
		})
	}
}

// proxyCertifyToChild sends an MCP tools/call to a child's /mcp/message endpoint
// and returns the per-file result lines.
func proxyCertifyToChild(ctx context.Context, port int, req CertifyBatchReq) ([]string, error) {
	mcpPayload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "neo_sre_certify_mutation",
			"arguments": map[string]any{
				"mutated_files":     req.MutatedFiles,
				"complexity_intent": req.ComplexityIntent,
				"rollback_mode":     req.RollbackMode,
				"dry_run":           req.DryRun,
			},
		},
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/mcp/message", port) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is runtime-assigned by ProcessPool
	mcpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(mcpPayload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	mcpReq.Header.Set("Content-Type", "application/json")

	client := sre.SafeInternalHTTPClient(30)
	resp, err := client.Do(mcpReq)
	if err != nil {
		return nil, fmt.Errorf("child MCP call: %w", err)
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
		return []string{fmt.Sprintf("⚠️ parse error from child port %d: %v", port, parseErr)}, nil
	}
	if mcpResult.Error != nil {
		return []string{fmt.Sprintf("❌ MCP error from child: %s", mcpResult.Error.Message)}, nil
	}

	var results []string
	for _, c := range mcpResult.Result.Content {
		if c.Type == "text" {
			for line := range strings.SplitSeq(c.Text, "\n") {
				if line != "" {
					results = append(results, line)
				}
			}
		}
	}
	return results, nil
}
