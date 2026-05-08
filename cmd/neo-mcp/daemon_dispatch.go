// cmd/neo-mcp/daemon_dispatch.go — DeepSeek dispatch helper for the daemon
// loop. PILAR XXVII / 138.F.2.
//
// dispatchToDeepSeek invokes the DeepSeek plugin (registered with namespace
// prefix "deepseek") through the Nexus HTTP endpoint added in 138.F.1
// (POST /api/v1/plugins/deepseek/call). It hides the JSON-RPC + MCP envelope
// from the daemon caller and surfaces a small dispatchResult with the text
// output and token usage parsed out of the plugin response.
//
// Why through Nexus and not stdio: the plugin's stdin/stdout is held by the
// Nexus PluginPool. neo-mcp child cannot open its own channel without
// breaking the singleton-plugin invariant. The HTTP endpoint reuses the
// same callPluginTool path, so ACL + policy + idempotency checks fire
// identically to the SSE transport.
//
// Loopback-only: uses sre.SafeInternalHTTPClient which rejects any
// non-loopback IP — same guard as Nexus → child scatter.
//
// Lives in cmd/neo-mcp (not pkg/state) because pkg/sre imports pkg/state
// via ebpf_tracer.go — placing this in pkg/state would create an import
// cycle. The function is only called from handleExecuteNext inside this
// process, so cmd/neo-mcp is the correct home.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/state"
)

// dispatchResult captures the data extracted from a deepseek/call response.
// Output is the post-metadata payload (the actual model output); TokensUsed
// reflects the plugin's own tokens_used metric.
type dispatchResult struct {
	Output     string
	TokensUsed int
	CacheHit   bool
	ToolName   string
	ThreadID   string // populated by red_team_audit / generate_boilerplate
	Truncated  bool   // true if Output was truncated to fit OutputSummary cap
}

// dispatchHTTPTimeoutSec bounds a single deepseek_call. red_team_audit with
// reasoner mode can take 30-60s for medium files; distill is faster. 90s
// covers both with margin while still failing fast on hung plugins.
const dispatchHTTPTimeoutSec = 90

// outputSummaryMaxBytes caps the Output we return — the daemon persists this
// in DaemonResult.OutputSummary which is shown to the operator. Multi-MB
// raw plugin output bloats BoltDB without value.
const outputSummaryMaxBytes = 4096

// dispatchToDeepSeek sends a deepseek/call request to Nexus and returns the
// parsed result. workspaceID is required for plugin ACL — empty value yields
// HTTP 400 from the Nexus side.
func dispatchToDeepSeek(ctx context.Context, workspaceID string, task state.SRETask, toolName string) (*dispatchResult, error) {
	if workspaceID == "" {
		return nil, fmt.Errorf("dispatch: workspace_id required")
	}
	if toolName == "" {
		return nil, fmt.Errorf("dispatch: tool_name required")
	}

	args, err := buildDeepseekArgs(task, toolName)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(map[string]any{"arguments": args})
	if err != nil {
		return nil, fmt.Errorf("dispatch: marshal args: %w", err)
	}

	url := resolveNexusURL() + "/api/v1/plugins/deepseek/call"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("dispatch: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Neo-Workspace", workspaceID)

	client := sre.SafeInternalHTTPClient(dispatchHTTPTimeoutSec)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dispatch: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return nil, fmt.Errorf("dispatch: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dispatch: nexus returned %d: %s", resp.StatusCode, truncateBytes(respBody, 256))
	}

	out, perr := parseDispatchResponse(respBody)
	if perr != nil {
		return nil, perr
	}
	out.ToolName = toolName
	return out, nil
}

// buildDeepseekArgs maps an SRETask + tool name into the schema expected by
// deepseek/call (see cmd/plugin-deepseek/tool_*.go for per-tool fields).
func buildDeepseekArgs(task state.SRETask, toolName string) (map[string]any, error) {
	prompt := strings.TrimSpace(task.Description)
	if prompt == "" {
		prompt = "Process this file according to the daemon task contract."
	}
	switch toolName {
	case "distill_payload", "map_reduce_refactor", "red_team_audit":
		args := map[string]any{
			"action":        toolName,
			"target_prompt": prompt,
		}
		if task.TargetFile != "" {
			args["files"] = []string{task.TargetFile}
		}
		return args, nil
	case "generate_boilerplate":
		if task.TargetFile == "" {
			return nil, fmt.Errorf("dispatch: generate_boilerplate requires task.TargetFile")
		}
		return map[string]any{
			"action":        "generate_boilerplate",
			"target_prompt": prompt,
			"target_file":   task.TargetFile,
		}, nil
	default:
		return nil, fmt.Errorf("dispatch: unknown deepseek tool %q (expected distill_payload|map_reduce_refactor|red_team_audit|generate_boilerplate)", toolName)
	}
}

// parseDispatchResponse extracts text + tokens from a JSON-RPC response
// envelope produced by callPluginTool. The deepseek plugin's content[0].text
// has shape "chunks_processed=N tokens=M cache_hit=BOOL\n<output>" for
// distill_payload, and "thread_id=ID tokens_used=M\n<output>" for
// red_team_audit / generate_boilerplate.
func parseDispatchResponse(body []byte) (*dispatchResult, error) {
	var env struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("dispatch: parse envelope: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("dispatch: plugin error %d: %s", env.Error.Code, env.Error.Message)
	}
	if len(env.Result.Content) == 0 {
		return nil, fmt.Errorf("dispatch: empty content array in plugin response")
	}
	raw := env.Result.Content[0].Text
	out := &dispatchResult{}
	if nl := strings.IndexByte(raw, '\n'); nl >= 0 {
		parseDispatchMetaLine(raw[:nl], out)
		text := strings.TrimSpace(raw[nl+1:])
		if len(text) > outputSummaryMaxBytes {
			out.Output = text[:outputSummaryMaxBytes]
			out.Truncated = true
		} else {
			out.Output = text
		}
	} else {
		out.Output = raw
	}
	return out, nil
}

// parseDispatchMetaLine extracts key=value pairs from the plugin's first
// response line. Recognized keys: tokens, tokens_used, chunks_processed,
// cache_hit, thread_id. Unrecognized keys are silently ignored — schema may
// evolve.
func parseDispatchMetaLine(line string, out *dispatchResult) {
	for _, tok := range strings.Fields(line) {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			continue
		}
		k, v := tok[:eq], tok[eq+1:]
		switch k {
		case "tokens", "tokens_used":
			if n, err := strconv.Atoi(v); err == nil {
				out.TokensUsed = n
			}
		case "cache_hit":
			out.CacheHit = (v == "true")
		case "thread_id":
			out.ThreadID = v
		}
	}
}

// resolveNexusURL returns the dispatcher URL: $NEO_NEXUS_URL → default loopback.
//
// [F23] When the env var is set, we accept it only if the host resolves to a
// loopback address — same defense as sre.SafeInternalHTTPClient. A misset env
// (or a malicious parent) cannot redirect dispatch to an external server.
// On any validation failure we fall back to the default rather than fail
// dispatch outright; the SSRF guard in the HTTP client still catches it.
func resolveNexusURL() string {
	const fallback = "http://127.0.0.1:9000"
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv("NEO_NEXUS_URL")), "/")
	if raw == "" {
		return fallback
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fallback
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return raw
	}
	return fallback
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// summarizeDispatchOutput collapses the plugin's text output into a single
// short line suitable for ExecuteNextResponse.OutputSummary. The full text
// is persisted separately in DaemonResult.OutputSummary; this helper just
// gives the operator a one-glance preview in the MCP response.
const dispatchSummaryMaxBytes = 240

func summarizeDispatchOutput(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[:nl]
	}
	if len(s) > dispatchSummaryMaxBytes {
		s = s[:dispatchSummaryMaxBytes] + "…"
	}
	return s
}

// computePipelinePhase resolves the lifecycle stage label that should ship
// in ExecuteNextResponse.PipelinePhase. It distinguishes the four states
// the operator cares about: dispatch succeeded (audit pending), dispatch
// failed, claude backend (no dispatch wired yet), and the empty case.
func computePipelinePhase(backend string, tokens int, dispatchErr error) string {
	if dispatchErr != nil {
		return "dispatch_failed"
	}
	if backend == "deepseek" && tokens > 0 {
		return "dispatched" // [138.G TODO] flip to "auditing" when audit pipeline lands
	}
	if backend == "claude" {
		return "skeleton: backend=claude"
	}
	return ""
}
