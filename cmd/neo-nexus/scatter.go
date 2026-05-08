package main

// [SRE-87.A] Scatter-Gather interceptor for cross-workspace BLAST_RADIUS.
//
// When Nexus detects a tools/call for neo_radar with intent BLAST_RADIUS, it
// fans out the request to ALL running children in parallel. Each child resolves
// BLAST_RADIUS locally and returns its edges. Nexus fuses the results into a
// single unified response with cross-workspace impact data.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// [PILAR-XXVIII hotfix] Package-level shared client for scatter-gather.
// Was allocated per-call inside scatterBlastRadius, leaking one
// http.Transport (+ its keep-alive pool) per BLAST_RADIUS request.
var scatterBlastClient = sre.SafeInternalHTTPClient(10)

// childResult holds the response from a single child during scatter-gather.
type childResult struct {
	WorkspaceID   string
	WorkspaceName string
	StatusCode    int
	Body          []byte
	Err           error
}

// scatterBlastRadius sends the BLAST_RADIUS request to all running children
// and merges their responses. Returns the fused JSON-RPC response body. [SRE-87.A.1]
func scatterBlastRadius(bodyBytes []byte, pool *nexus.ProcessPool) []byte {
	procs := pool.List()
	if len(procs) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	results := make([]childResult, len(procs))
	client := scatterBlastClient // shared — see package var

	// Fan-out: POST to all running children in parallel. [SRE-87.A.2]
	for i, p := range procs {
		if p.Status != nexus.StatusRunning {
			continue
		}
		wg.Add(1)
		go func(idx int, proc nexus.WorkspaceProcess) {
			defer wg.Done()
			url := fmt.Sprintf("http://127.0.0.1:%d/mcp/message", proc.Port)
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
			if err != nil {
				results[idx] = childResult{WorkspaceID: proc.Entry.ID, WorkspaceName: proc.Entry.Name, Err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				results[idx] = childResult{WorkspaceID: proc.Entry.ID, WorkspaceName: proc.Entry.Name, Err: err}
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			results[idx] = childResult{
				WorkspaceID:   proc.Entry.ID,
				WorkspaceName: proc.Entry.Name,
				StatusCode:    resp.StatusCode,
				Body:          body,
			}
		}(i, p)
	}
	wg.Wait()

	// Fuse results. [SRE-87.A.3]
	return fuseBlastResults(bodyBytes, results)
}

// fuseBlastResults merges individual BLAST_RADIUS responses into a unified report.
func fuseBlastResults(originalBody []byte, results []childResult) []byte {
	// Extract the original request ID for the response.
	var envelope struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal(originalBody, &envelope)

	var sections []string
	var crossImpacts []map[string]any
	respondedCount := 0
	totalImpacted := 0

	for _, r := range results {
		if r.Err != nil {
			log.Printf("[NEXUS-SCATTER] child %s error: %v", r.WorkspaceID, r.Err)
			continue
		}
		if r.StatusCode != http.StatusOK || len(r.Body) == 0 {
			continue
		}

		// Parse JSON-RPC response to extract the text content.
		var rpcResp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(r.Body, &rpcResp); err != nil {
			continue
		}
		if len(rpcResp.Result.Content) == 0 {
			continue
		}

		text := rpcResp.Result.Content[0].Text
		respondedCount++

		// Count impacted files from the text.
		impacted := strings.Count(text, "\n- ")
		totalImpacted += impacted

		sections = append(sections, fmt.Sprintf("### Workspace: %s (%s)\n%s", r.WorkspaceName, r.WorkspaceID, text))

		if impacted > 0 {
			crossImpacts = append(crossImpacts, map[string]any{
				"workspace":    r.WorkspaceName,
				"workspace_id": r.WorkspaceID,
				"impact_count": impacted,
			})
		}
	}

	if respondedCount == 0 {
		return nil
	}

	// Build fused report.
	var report strings.Builder
	report.WriteString("## BLAST_RADIUS (Cross-Workspace Scatter-Gather)\n\n")
	report.WriteString(fmt.Sprintf("**workspaces_queried:** %d | **responded:** %d | **total_impacted:** %d\n\n", len(results), respondedCount, totalImpacted))

	if len(crossImpacts) > 1 {
		report.WriteString("### Cross-Workspace Impacts\n")
		for _, ci := range crossImpacts {
			report.WriteString(fmt.Sprintf("- **%s**: %d impacted files\n", ci["workspace"], ci["impact_count"]))
		}
		report.WriteString("\n")
	}

	report.WriteString(strings.Join(sections, "\n---\n\n"))

	// Build JSON-RPC response.
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      envelope.ID,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": report.String()},
			},
			"_meta": map[string]any{
				"scatter_gather":       true,
				"workspaces_queried":   len(results),
				"workspaces_responded": respondedCount,
				"cross_workspace_impacts": crossImpacts,
				"fused_at":            time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	out, _ := json.Marshal(resp)
	return out
}

// isBlastRadiusCall checks if the JSON-RPC body is a tools/call for neo_radar BLAST_RADIUS.
func isBlastRadiusCall(body []byte) bool {
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Intent string `json:"intent"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	return envelope.Method == "tools/call" &&
		envelope.Params.Name == "neo_radar" &&
		envelope.Params.Arguments.Intent == "BLAST_RADIUS"
}
