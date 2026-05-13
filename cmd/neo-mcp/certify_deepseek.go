// cmd/neo-mcp/certify_deepseek.go — ÉPICA 371.B+C+D: DeepSeek pre-certify
// integration. Invokes red_team_audit on hot-path files before sealing.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// isHotPath checks if filename matches any of the configured hot-path globs.
// Returns true if the file should trigger DeepSeek pre-certify. [371.B]
func isHotPath(filename string, hotPaths []string, workspace string) bool {
	rel, err := filepath.Rel(workspace, filename)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	for _, pattern := range hotPaths {
		if matched, _ := filepath.Match(pattern, rel); matched {
			return true
		}
	}
	return false
}

// deepseekPreCheckResult holds the parsed outcome of a DS pre-certify call.
type deepseekPreCheckResult struct {
	Mode     string // "auto", "manual", "off"
	IsHot    bool
	Findings int
	MaxSev   int
	Summary  string
	Blocked  bool
}

// deepseekPreCheck invokes DeepSeek red_team_audit on a hot-path file via
// the Nexus plugin dispatch endpoint. Returns advisory text for the certify
// result. Non-blocking: plugin unavailable or timeout → log + continue. [371.C]
func (t *CertifyMutationTool) deepseekPreCheck(filename string, _ []byte) deepseekPreCheckResult {
	mode := t.cfg.SRE.DeepseekPreCertify
	if mode == "" {
		mode = "manual"
	}
	if mode == "off" {
		return deepseekPreCheckResult{Mode: mode}
	}

	hotPaths := t.cfg.SRE.DeepseekHotPaths
	if !isHotPath(filename, hotPaths, t.workspace) {
		return deepseekPreCheckResult{Mode: mode, IsHot: false}
	}

	blockSev := t.cfg.SRE.DeepseekBlockSeverity
	if blockSev == 0 {
		blockSev = 9
	}

	if mode == "manual" {
		rel, _ := filepath.Rel(t.workspace, filename)
		return deepseekPreCheckResult{
			Mode:    mode,
			IsHot:   true,
			Summary: fmt.Sprintf("⚠️ DS_ADVISORY: %s is a hot-path file — consider running deepseek/red_team_audit before closing this epic.", filepath.ToSlash(rel)),
		}
	}

	// mode == "auto": invoke DS via Nexus plugin endpoint
	rel, _ := filepath.Rel(t.workspace, filename)
	relSlash := filepath.ToSlash(rel)

	nexusURL := os.Getenv("NEO_NEXUS_URL")
	if nexusURL == "" {
		if t.cfg.Server.NexusDispatcherPort != 0 {
			nexusURL = fmt.Sprintf("http://127.0.0.1:%d", t.cfg.Server.NexusDispatcherPort)
		} else {
			nexusURL = "http://127.0.0.1:9000"
		}
	}

	body, err := json.Marshal(map[string]any{
		"action":      "red_team_audit",
		"model":       "deepseek-v4-flash",
		"max_tokens":  3000,
		"background":  true,
		"files":       []string{relSlash},
		"target_prompt": fmt.Sprintf("Quick security audit of %s. Focus on: injection, TOCTOU, path traversal, crypto misuse. Max 5 findings. Report: ID, severity 1-10, line, description.", relSlash),
	})
	if err != nil {
		log.Printf("[DS-PRECERTIFY] marshal error: %v", err)
		return deepseekPreCheckResult{Mode: mode, IsHot: true, Summary: "⚠️ DS_SKIP: marshal error"}
	}

	client := sre.SafeInternalHTTPClient(5)
	url := nexusURL + "/workspaces/" + certifyResolveWSID(t) + "/mcp/message"
	rpcBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "deepseek_call",
			"arguments": json.RawMessage(body),
		},
	})

	req, err := http.NewRequest("POST", url, strings.NewReader(string(rpcBody)))
	if err != nil {
		log.Printf("[DS-PRECERTIFY] request error: %v", err)
		return deepseekPreCheckResult{Mode: mode, IsHot: true, Summary: "⚠️ DS_SKIP: request build error"}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[DS-PRECERTIFY] dispatch error: %v", err)
		return deepseekPreCheckResult{Mode: mode, IsHot: true, Summary: "⚠️ DS_SKIP: plugin unavailable"}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// Parse the MCP response to extract task_id
	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &rpcResp); err != nil || len(rpcResp.Result.Content) == 0 {
		log.Printf("[DS-PRECERTIFY] parse error: %v", err)
		return deepseekPreCheckResult{Mode: mode, IsHot: true, Summary: "⚠️ DS_SKIP: response parse error"}
	}

	taskText := rpcResp.Result.Content[0].Text
	var taskResp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(taskText), &taskResp); err != nil {
		log.Printf("[DS-PRECERTIFY] task parse error: %v body=%s", err, taskText[:min(len(taskText), 200)])
		return deepseekPreCheckResult{Mode: mode, IsHot: true, Summary: "⚠️ DS_SKIP: task parse error"}
	}

	if taskResp.TaskID != "" {
		return deepseekPreCheckResult{
			Mode:    mode,
			IsHot:   true,
			Summary: fmt.Sprintf("🔍 DS_SUBMITTED: async audit task %s for %s — poll with deepseek_call(task_id:\"%s\") or GET /api/v1/async/tasks/%s", taskResp.TaskID, relSlash, taskResp.TaskID, taskResp.TaskID),
		}
	}

	return deepseekPreCheckResult{Mode: mode, IsHot: true, Summary: "⚠️ DS_SKIP: no task_id returned"}
}

func certifyResolveWSID(t *CertifyMutationTool) string {
	if t.registry != nil {
		for _, ws := range t.registry.Workspaces {
			if ws.Path == t.workspace {
				return ws.ID
			}
		}
	}
	return "neoanvil-45913"
}

// formatDSCertifyCheck returns the certify check entry for the DS pre-check. [371.D]
func formatDSCertifyCheck(r deepseekPreCheckResult) string {
	if r.Mode == "off" || !r.IsHot {
		return "skipped"
	}
	if r.Blocked {
		return "fail:" + r.Summary
	}
	return "advisory:" + r.Summary
}

