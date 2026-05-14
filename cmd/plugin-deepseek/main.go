// Command plugin-deepseek is the DeepSeek Fan-Out Engine MCP plugin for neoanvil.
// PILAR XXIV / Épica 131.A.
//
// Wire format: newline-delimited JSON-RPC over stdio (MCP stdio transport).
// Auth: env vars (injected by Nexus PluginPool from ~/.neo/credentials.json):
//
//	DEEPSEEK_API_KEY — DeepSeek API key (required)
//
// Session modes (Épica 131.B):
//
//	ephemeral — fire-and-forget; no thread state persisted (distill, map_reduce_refactor)
//	threaded  — ThreadID persisted in BoltDB; context window managed (red_team_audit)
//
// Cache (Épica 131.C):
//
//	Block 1 static  — system prompt + directives + unchanged code files (SHA-256 keyed)
//	Block 2 dynamic — task payload (per-call)
//
// Babel Pattern (Épica 131.D): target_prompt MUST be written in English.
// Claude translates user input before calling this plugin.
//
// Billing (Épica 131.E): max_tokens_per_tx enforced per call; session counter
// in BoltDB deepseek_billing bucket; circuit breaker trips at daily limit.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
)

const (
	protocolVersion = "2024-11-05"
	pluginVersion   = "0.1.0"
)

// state holds the long-lived plugin state.
type state struct {
	apiKey string
	client *deepseek.Client      // nil when DBPath is empty or API key missing
	notify func(map[string]any)  // emit an out-of-band JSON-RPC notification (progress etc.)

	// [ÉPICA 152.H] Local-only health counters. Consumed by the
	// __health__ MCP action so neo-mcp / Nexus can poll plugin state
	// without invoking the upstream DeepSeek API. Atomic counters keep
	// the read path lock-free at <10ms target latency.
	startedAtUnix    int64
	lastDispatchUnix int64
	errorCount       int64

	// [ÉPICA 151.E] Cache discipline metrics. Sum of prompt cache token
	// counts reported by the DS API per call. Cache hit tokens cost
	// ~50× less than miss tokens (directive #237 / #248 regla 1), so
	// the ratio hit/(hit+miss) is the actionable metric: <30% in a
	// session with >3 calls indicates poor cache discipline (thread_id
	// not reused, Files[] varying, etc.). Surfaced via __health__ for
	// neo-mcp / BRIEFING consumption.
	cacheHitTokens  int64
	cacheMissTokens int64
	threadCount     int64 // [375.D] threads created this session
}

func main() {
	st, err := buildState()
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin-deepseek: init failed:", err)
		os.Exit(1)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	enc := json.NewEncoder(os.Stdout)

	// Wire up out-of-band notification emitter for progress events.
	st.notify = func(n map[string]any) {
		if err := enc.Encode(n); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-deepseek: notify encode:", err)
		}
	}

	for scanner.Scan() {
		var req map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-deepseek: bad json:", err)
			continue
		}
		resp := st.handle(req)
		if resp == nil {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-deepseek: encode:", err)
			return
		}
	}
}

func buildState() (*state, error) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		return nil, errors.New("DEEPSEEK_API_KEY is required")
	}
	st := &state{apiKey: key}
	// [ÉPICA 152.H] Capture boot timestamp for uptime reporting via __health__.
	atomic.StoreInt64(&st.startedAtUnix, time.Now().Unix())
	// Build the DeepSeek client. DBPath is optional; without it threads are in-memory only.
	// BaseURL: Area 3.2.A integration-test override; empty falls back to defaultBaseURL.
	c, err := deepseek.New(deepseek.Config{
		APIKey:             key,
		DBPath:             os.Getenv("DEEPSEEK_DB_PATH"),
		BaseURL:            os.Getenv("DEEPSEEK_BASE_URL"),
		HTTPTimeoutSeconds: envInt("DEEPSEEK_HTTP_TIMEOUT_SECONDS"),
	})
	if err == nil {
		st.client = c
	}
	return st, nil
}

func (s *state) handle(req map[string]any) map[string]any {
	method, _ := req["method"].(string)
	id := req["id"]
	switch method {
	case "initialize":
		return handleInitialize(id)
	case "notifications/initialized":
		return nil
	case "tools/list":
		return handleToolsList(id)
	case "tools/call":
		return s.handleToolsCall(id, req)
	}
	return rpcErr(id, -32601, "method not found: "+method)
}

func handleInitialize(id any) map[string]any {
	return ok(id, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "plugin-deepseek", "version": pluginVersion},
	})
}

func handleToolsList(id any) map[string]any {
	return ok(id, map[string]any{
		"tools": []map[string]any{
			{
				"name":        "call",
				"description": "DeepSeek Fan-Out Engine. Delegates bulk token work to DeepSeek API in parallel. Claude orchestrates; DeepSeek handles distillation, refactor, and red-team audit tasks efficiently.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": map[string]any{
							"type":        "string",
							"enum":        []string{"distill_payload", "map_reduce_refactor", "red_team_audit", "generate_boilerplate"},
							"description": "Task type. distill_payload: compress context (ephemeral). map_reduce_refactor: multi-file refactor fan-out (ephemeral). red_team_audit: adversarial code review with thread continuity (threaded). generate_boilerplate: generate tests/docs for a file in background (returns task_id immediately).",
						},
						"target_prompt": map[string]any{
							"type":        "string",
							"description": "Task description or style guide. Must be written in English (Babel Pattern — Claude translates before calling).",
						},
						"files": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "[map_reduce_refactor, red_team_audit] Repo-relative file paths to include as Block 1 static context.",
						},
						"thread_id": map[string]any{
							"type":        "string",
							"description": "[red_team_audit] Optional existing thread ID (ds_thread_<8hex>) to continue. Omit to start a new thread.",
						},
						"max_tokens": map[string]any{
							"type":        "integer",
							"description": "Hard cap on output tokens for this call. Default 4096. Max 50000.",
						},
						"target_file": map[string]any{
							"type":        "string",
							"description": "[generate_boilerplate] Path of the source file to generate boilerplate for.",
						},
						"boilerplate_type": map[string]any{
							"type":        "string",
							"enum":        []string{"tests", "docs", "both"},
							"description": "[generate_boilerplate] Output type: tests (_test.go), docs (doc.go), or both.",
						},
						"task_id": map[string]any{
							"type":        "string",
							"description": "[generate_boilerplate] Optional task ID to query status of a previously launched background task.",
						},
						// [Phase 4 audit fix · 2026-05-01] Per-call routing of model + thinking effort.
						// Lesson from PAIR-AUDIT-EMIT-SCHEMA-GAP: handlers read these via args[…], so
						// they MUST be declared here or the MCP client strips them at the boundary.
						"model": map[string]any{
							"type":        "string",
							"enum":        []string{"deepseek-v4-flash", "deepseek-v4-pro", "deepseek-chat", "deepseek-reasoner"},
							"description": "Optional model override per call. Empty = config default (deepseek-v4-flash). Use deepseek-v4-pro for security-critical audits (75% off until 2026-05-31). The chat/reasoner aliases are deprecated but still function.",
						},
						"thinking_type": map[string]any{
							"type":        "string",
							"enum":        []string{"enabled", "disabled"},
							"description": "Optional explicit override of thinking mode. Empty = server's per-model default (enabled on v4-flash/v4-pro, disabled on the deepseek-chat alias).",
						},
						"reasoning_effort": map[string]any{
							"type":        "string",
							"enum":        []string{"high", "max"},
							"description": "Optional CoT depth knob. high (default) is sufficient for most audits; max produces 3-5× more reasoning tokens — reserve for crypto / distributed-lock / concurrency audits.",
						},
						"background": map[string]any{
							"type":        "boolean",
							"description": "[376.G] When true, Nexus dispatches the call in a background goroutine and returns {task_id, status:pending} immediately. Poll with task_id to retrieve the result. Applies to red_team_audit and map_reduce_refactor.",
						},
					},
					"required": []string{"action"},
				},
			},
		},
	})
}

func (s *state) handleToolsCall(id any, req map[string]any) map[string]any {
	params, _ := req["params"].(map[string]any)
	if params == nil {
		return rpcErr(id, -32602, "missing params")
	}
	name, _ := params["name"].(string)
	if name != "call" {
		return rpcErr(id, -32601, "unknown tool: "+name)
	}
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}

	action, _ := args["action"].(string)
	if action == "" {
		return rpcErr(id, -32602, "action is required")
	}

	// [ÉPICA 152.H] __health__ is a local-only liveness probe — never
	// touches the upstream DeepSeek API, returns instantly. neo-mcp +
	// Nexus health pollers consume this to detect "process alive but
	// dispatcher dead" without paying the 60s context-canceled cost
	// that real API calls incur.
	if action == "__health__" {
		return s.handleHealth(id)
	}

	// Dispatch + bookkeeping for real actions. last_dispatch_unix is
	// updated so __health__ can show recency; error_count is bumped
	// when the action handler returns rpcErr (best-effort heuristic —
	// errors counted by JSON-RPC error code presence in response).
	atomic.StoreInt64(&s.lastDispatchUnix, time.Now().Unix())
	resp := s.dispatchAction(id, action, args)
	if _, isErr := resp["error"]; isErr {
		atomic.AddInt64(&s.errorCount, 1)
	}
	return resp
}

// dispatchAction is the real-actions switch. Extracted from
// handleToolsCall so __health__ can short-circuit before the bookkeeping.
func (s *state) dispatchAction(id any, action string, args map[string]any) map[string]any {
	switch action {
	case "distill_payload":
		return distillPayload(s, id, args)
	case "map_reduce_refactor":
		return mapReduceRefactor(s, id, args)
	case "red_team_audit":
		return redTeamAudit(s, id, args)
	case "generate_boilerplate":
		return generateBoilerplate(s, id, args)
	default:
		result := fmt.Sprintf("[deepseek/%s] unknown action — api_key_present:%v", action, s.apiKey != "")
		return rpcErr(id, -32601, result)
	}
}

// handleHealth returns the plugin's self-reported health snapshot. Local
// state only — no upstream API calls. Documented schema (ÉPICA 152.H):
//
//	plugin_alive: bool       — always true if this code runs
//	tools_registered: []string — tool names this plugin handles
//	uptime_seconds: int64    — wall-clock since plugin started
//	last_dispatch_unix: int64 — Unix ts of last tools/call (0 = never)
//	error_count: int64       — cumulative error responses since start
//	api_key_present: bool    — does the plugin have credentials loaded
//	cache_hit_tokens: int64  — sum of prompt_cache_hit_tokens across DS calls (151.E)
//	cache_miss_tokens: int64 — sum of prompt_cache_miss_tokens (151.E)
//
// Polled every 30s by neo-mcp's plugin manager (152.C). Zombies
// (process alive but tools_registered=[]) are detected at the
// neo-mcp side by comparing tools_registered against the plugin's
// initial registered tools.
func (s *state) handleHealth(id any) map[string]any {
	started := atomic.LoadInt64(&s.startedAtUnix)
	uptime := int64(0)
	if started > 0 {
		uptime = time.Now().Unix() - started
	}
	hitTokens := atomic.LoadInt64(&s.cacheHitTokens)
	missTokens := atomic.LoadInt64(&s.cacheMissTokens)
	var cacheHitRatio float64
	if total := hitTokens + missTokens; total > 0 {
		cacheHitRatio = float64(hitTokens) / float64(total)
	}
	return ok(id, map[string]any{
		"plugin_alive":       true,
		"tools_registered":   []string{"call"},
		"uptime_seconds":     uptime,
		"last_dispatch_unix": atomic.LoadInt64(&s.lastDispatchUnix),
		"error_count":        atomic.LoadInt64(&s.errorCount),
		"api_key_present":    s.apiKey != "",
		"cache_hit_tokens":   hitTokens,
		"cache_miss_tokens":  missTokens,
		"cache_hit_ratio":    cacheHitRatio,
		"thread_count":       atomic.LoadInt64(&s.threadCount),
	})
}

// recordAPICall accumulates DS API cache discipline stats from a
// CallResponse. Called by every action handler post-success so the
// aggregate is observable via __health__. [ÉPICA 151.E]
//
// Sum semantics (NOT averaging): cache discipline is measured across
// a session — operators want to know "of all input tokens this session,
// what fraction came from the cheap prefix cache". hit/(hit+miss) is
// the actionable ratio.
func (s *state) recordAPICall(resp *deepseek.CallResponse) {
	if resp == nil {
		return
	}
	if resp.CacheHitTokens > 0 {
		atomic.AddInt64(&s.cacheHitTokens, int64(resp.CacheHitTokens))
	}
	if resp.CacheMissTokens > 0 {
		atomic.AddInt64(&s.cacheMissTokens, int64(resp.CacheMissTokens))
	}
}

// cacheColdAdvisory returns a warning string if the call had very few
// cache hit tokens relative to miss, indicating poor cache discipline.
// Empty string when cache is warm or metrics disabled. [375.A]
func (s *state) cacheColdAdvisory(resp *deepseek.CallResponse) string {
	if resp == nil {
		return ""
	}
	if resp.CacheHitTokens < 1000 && resp.CacheMissTokens > 0 {
		return fmt.Sprintf("\n\n⚠️ CACHE_COLD: %d hit vs %d miss tokens. Keep Files[] identical across calls for 50× cheaper cache hits.",
			resp.CacheHitTokens, resp.CacheMissTokens)
	}
	return ""
}

func textContent(s string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": s},
		},
	}
}

func ok(id any, result map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErr(id any, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
}

// envInt reads an integer env var, returning 0 when unset or unparseable so
// the consumer's own default backfill applies. Used for optional numeric
// tuning knobs injected by Nexus (e.g. DEEPSEEK_HTTP_TIMEOUT_SECONDS).
func envInt(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
