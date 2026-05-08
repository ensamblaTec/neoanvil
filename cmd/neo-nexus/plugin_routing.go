// cmd/neo-nexus/plugin_routing.go — MCP request/response interception for
// subprocess plugins. PILAR XXIII / final integration.
//
// Two interception points:
//
//	tools/list  — augment the child's response with plugin tools, namespaced.
//	tools/call  — when name matches a plugin's namespace prefix, dispatch to
//	              the plugin's MCP client instead of forwarding to the child.
//
// Both helpers are no-ops when the pluginRuntime is nil or empty, so the
// existing dispatcher path is unchanged when plugins are disabled.
//
// Épica integrations active in callPluginTool:
//   [P-POLICY] semantic firewall via PolicyEngine.Evaluate
//   [P-IDEM]   idempotency dedup via sha256 key + TTL cache
//   [P4]       trace ID injected in _meta for cross-process observability

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

// batchMap tracks batch_id → task_ids for batch poll. In-memory only —
// survives within a Nexus process lifetime. [376.I]
var (
	batchMapMu sync.RWMutex
	batchMap   = make(map[string][]string)
)

// interceptPluginTools merges plugin tools into a tools/list response from
// the child. Returns the original respBody untouched when:
//   - rt is nil or has no aggregated tools
//   - respBody is not a JSON object with a `result` field (error response or
//     unexpected shape)
//   - re-marshaling fails (defensive fall-back)
//
// On success, returns a freshly marshaled body with plugin tool entries
// appended after the child's tools.
func interceptPluginTools(respBody []byte, rt *pluginRuntime) []byte {
	if rt == nil {
		return respBody
	}
	rt.mu.RLock()
	tools := rt.tools
	rt.mu.RUnlock()
	if len(tools) == 0 {
		return respBody
	}

	var raw map[string]any
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return respBody
	}
	result, ok := raw["result"].(map[string]any)
	if !ok {
		return respBody
	}
	existing, _ := result["tools"].([]any)
	// [F-152.3 / DS audit 2026-05-02] Dedup against existing names. A plugin
	// that announces a prefixed name colliding with a core tool (e.g. plugin
	// prefix="neo" tool="radar" → "neo_radar" same as core neo_radar) would
	// otherwise produce duplicate entries in tools/list. MCP clients handle
	// duplicates inconsistently; some keep the second entry (silently
	// shadowing core), others reject the whole list. Skip-with-log makes the
	// behavior explicit: core wins, plugin entry dropped, operator sees why.
	existingNames := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		if name != "" {
			existingNames[name] = struct{}{}
		}
	}
	for _, nt := range tools {
		prefixed := nt.PrefixedName()
		if _, collides := existingNames[prefixed]; collides {
			log.Printf("[NEXUS-PLUGINS] tool name collision: plugin=%s tool=%s prefixed=%q already in core list — dropping plugin entry",
				nt.PluginName, nt.Tool.Name, prefixed)
			continue
		}
		// [ÉPICA 152.L / DS audit fix #6] InputSchema validation. Plugin
		// authors can return malformed JSON in InputSchema (whether by
		// bug or malice). json.Marshal of the response wrapper would
		// then produce invalid JSON-RPC → MCP client crashes or silently
		// drops the entire tools/list. Validate + skip on invalid.
		if len(nt.Tool.InputSchema) > 0 && !json.Valid([]byte(nt.Tool.InputSchema)) {
			log.Printf("[NEXUS-PLUGINS] invalid InputSchema: plugin=%s tool=%s — dropping plugin entry to preserve tools/list integrity",
				nt.PluginName, nt.Tool.Name)
			continue
		}
		existing = append(existing, map[string]any{
			"name":        prefixed,
			"description": nt.Tool.Description,
			"inputSchema": json.RawMessage(nt.Tool.InputSchema),
		})
		existingNames[prefixed] = struct{}{}
	}
	result["tools"] = existing
	raw["result"] = result
	out, err := json.Marshal(raw)
	if err != nil {
		return respBody
	}
	return out
}

// isToolsListRequest returns true when the JSON-RPC body is a tools/list
// request. Cheap parse — only inspects the top-level method field.
func isToolsListRequest(body []byte) bool {
	var req struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return req.Method == "tools/list"
}

// detectPluginToolCall returns (connected, localName) when body is a
// tools/call whose name starts with a registered plugin's namespace
// prefix. Otherwise (nil, "").
func detectPluginToolCall(body []byte, rt *pluginRuntime) (*plugin.Connected, string) {
	if rt == nil {
		return nil, ""
	}
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, ""
	}
	if req.Method != "tools/call" {
		return nil, ""
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	// Underscore separator (MCP spec compliance — see PrefixedName godoc).
	// Slash was the original contract per ADR-005 but Claude Code normalized
	// it silently, breaking dispatch. ÉPICA 152 / PILAR XXIX.
	//
	// [F-152.1 / DS audit 2026-05-02] Iterate by DESCENDING prefix length so
	// "my_plugin" wins over "my" when both are registered and request name is
	// "my_plugin_tool". Without this ordering the slice-order-dependent first
	// match could route a call intended for "my_plugin" to "my", silently
	// dispatching to the wrong plugin (potential privilege escalation when
	// the prefixes differ in trust level). N is small (handful of plugins)
	// so the sort overhead per call is negligible.
	indices := make([]int, len(rt.conns))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool {
		return len(rt.conns[indices[a]].NamespacePrefix) > len(rt.conns[indices[b]].NamespacePrefix)
	})
	for _, i := range indices {
		prefix := rt.conns[i].NamespacePrefix + "_"
		if local, ok := strings.CutPrefix(req.Params.Name, prefix); ok {
			// [154.C / DS audit bonus] Empty localName guard. When the
			// request name equals the prefix exactly (e.g. "deepseek_"
			// with prefix "deepseek"), CutPrefix returns ("", true) —
			// dispatching with localName="" gives the plugin an empty
			// tool name, which is undefined behavior. Skip to next
			// candidate prefix instead.
			if local == "" {
				continue
			}
			return &rt.conns[i], local
		}
	}
	return nil, ""
}

// callPluginTool invokes the plugin's tool and wraps the result as a
// JSON-RPC response body ready to push onto the SSE stream. The id from
// the original request is preserved.
//
// Integration points (all no-ops when rt is nil):
//   [P-WSACL] Workspace allowlist check — if conn.AllowedWorkspaces is non-empty,
//             workspaceID must be present in the list or the call is rejected (-32601).
//   [P-POLICY] PolicyEngine evaluates "plugin_tool_call" before dispatch.
//              DENY returns a JSON-RPC error (-32601) without calling the plugin.
//   [P-IDEM]   Deterministic idempotency key = sha256(plugin+tool+args)[0:16].
//              Cache hit returns the stored result without re-calling the plugin.
//   [P4]       Trace ID (16-byte random hex) + idempotency key injected in _meta.
func callPluginTool(ctx context.Context, body []byte, conn *plugin.Connected, localName string, rt *pluginRuntime, workspaceID string) ([]byte, error) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Params struct {
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}

	// [ÉPICA 154.B] Plugin call observability — get the metric entry once
	// and route counters at each early-return + the actual dispatch.
	metric := getOrCreatePluginMetric(conn.Name, localName)

	// [P-WSACL + P-POLICY] Authorize before any plugin I/O.
	if resp := rejectIfUnauthorized(conn, localName, workspaceID, rt, metric, req.ID); resp != nil {
		return resp, nil
	}

	// [376.C+D] Async dispatch (background:true) or poll (task_id without action).
	if resp := handleAsyncDispatch(req.ID, req.Params.Arguments, conn, localName, rt); resp != nil {
		return resp, nil
	}

	// [P-IDEM] Compute deterministic idempotency key; short-circuit on cache hit.
	// workspaceID is included so different workspaces never share cached results
	// even when calling the same plugin/tool with identical args. [DS-F5 fix]
	idemKey := computeIdempotencyKey(conn.Name, localName, workspaceID, req.Params.Arguments)
	if rt != nil && rt.idem != nil {
		if cached, ok := rt.idem.get(idemKey); ok {
			log.Printf("[NEXUS-PLUGINS-IDEM] HIT plugin=%s tool=%s key=%s", conn.Name, localName, idemKey)
			metric.recordCacheHit()
			return json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  json.RawMessage(cached),
			})
		}
	}

	// [P4] Generate trace ID; inject _meta for cross-process observability.
	traceID := newTraceID()
	meta := map[string]any{
		"trace_id":        traceID,
		"idempotency_key": idemKey,
		"workspace_id":    workspaceID,
	}

	// [ÉPICA 154.B] Measure end-to-end plugin call latency. Both ok and err
	// paths feed the latency ring — operators want to see latency including
	// failure modes (timeouts, broken pipe, schema mismatch).
	//
	// Merged context: 180s hard cap allows slow plugins (DeepSeek 30-120s)
	// to complete, BUT client cancellation propagates immediately to stop
	// token generation mid-stream — avoids orphaned API calls that waste
	// money on retries.
	pluginCtx, pluginCancel := context.WithTimeout(context.Background(), 180*time.Second)
	go func() {
		select {
		case <-ctx.Done():
			pluginCancel()
		case <-pluginCtx.Done():
		}
	}()
	defer pluginCancel()
	callStart := time.Now()
	raw, err := conn.Client.CallToolWithMeta(pluginCtx, localName, req.Params.Arguments, meta)
	metric.recordCall(time.Since(callStart), err != nil)
	if err != nil {
		// [ÉPICA 152.K / DS audit fix #5] Use-after-reload race: if SIGHUP
		// reloaded pluginRuntime between detectPluginToolCall (which
		// captured &rt.conns[i] under RLock then released) and this
		// CallToolWithMeta, conn.Client may now be a closed pipe. Re-look
		// up the plugin by name and surface a clearer error so callers
		// know to retry rather than seeing "broken pipe" / generic EOF.
		if rt != nil && pluginReloaded(rt, conn.Name) {
			return json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error": map[string]any{
					"code":    -32603,
					"message": fmt.Sprintf("plugin %q reloaded mid-call — retry", conn.Name),
				},
			})
		}
		return json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]any{
				"code":    -32603,
				"message": err.Error(),
			},
		})
	}

	// [P-IDEM] Store result for future dedup within TTL.
	if rt != nil && rt.idem != nil {
		rt.idem.set(idemKey, raw)
	}

	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result":  json.RawMessage(raw),
	})
}

// rejectIfUnauthorized enforces the workspace ACL [P-WSACL] and semantic
// handleAsyncDispatch checks for background:true (submit) or task_id without
// action (poll). Returns a JSON-RPC response to short-circuit, or nil to
// continue with the synchronous path. Extracted from callPluginTool to keep
// CC under 15. [376.C+D]
func handleAsyncDispatch(reqID json.RawMessage, args map[string]any, conn *plugin.Connected, localName string, rt *pluginRuntime) []byte {
	if bg, _ := args["background"].(bool); bg && rt != nil && rt.asyncStore != nil {
		// [376.H] Batch dispatch: batch_files present → fan-out N goroutines.
		if batchFiles, ok := args["batch_files"].([]any); ok && len(batchFiles) > 0 {
			return handleBatchDispatch(reqID, args, batchFiles, conn, localName, rt)
		}
		return handleSingleAsyncSubmit(reqID, args, conn, localName, rt)
	}
	// [376.I] Batch poll: batch_id present without action.
	if batchID, ok := args["batch_id"].(string); ok {
		if _, hasAction := args["action"]; !hasAction && rt != nil && rt.asyncStore != nil {
			return handleBatchPoll(reqID, batchID, rt)
		}
	}
	// [376.D] Single task poll.
	if taskID, ok := args["task_id"].(string); ok {
		if _, hasAction := args["action"]; !hasAction && rt != nil && rt.asyncStore != nil {
			return handleTaskPoll(reqID, taskID, rt)
		}
	}
	return nil
}

func handleSingleAsyncSubmit(reqID json.RawMessage, args map[string]any, conn *plugin.Connected, localName string, rt *pluginRuntime) []byte {
	action, _ := args["action"].(string)
	taskID, err := rt.asyncStore.Submit(conn.Name, action)
	if err != nil {
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": reqID, "error": map[string]any{"code": -32000, "message": err.Error()}})
		return resp
	}
	cleanArgs := make(map[string]any, len(args))
	for k, v := range args {
		if k != "background" {
			cleanArgs[k] = v
		}
	}
	toolName := conn.ToolName
	if toolName == "" {
		toolName = localName
	}
	go RunAsync(rt.asyncStore, conn, toolName, cleanArgs, taskID, rt.asyncDoneCallback)
	log.Printf("[PLUGIN-ASYNC] submitted task %s plugin=%s action=%s", taskID, conn.Name, action)
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprintf(`{"task_id":"%s","status":"pending"}`, taskID)}}},
	})
	return resp
}

// handleBatchDispatch fans out N async tasks from batch_files, each with its
// own files entry. Returns batch_id + task_ids immediately. Sem=4. [376.H]
func handleBatchDispatch(reqID json.RawMessage, args map[string]any, batchFiles []any, conn *plugin.Connected, localName string, rt *pluginRuntime) []byte {
	action, _ := args["action"].(string)
	count := len(batchFiles)
	batchID, taskIDs, err := rt.asyncStore.SubmitBatch(conn.Name, action, count)
	if err != nil {
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": reqID, "error": map[string]any{"code": -32000, "message": err.Error()}})
		return resp
	}
	toolName := conn.ToolName
	if toolName == "" {
		toolName = localName
	}
	sem := make(chan struct{}, 4)
	for i, bf := range batchFiles {
		files, ok := bf.([]any)
		if !ok {
			continue
		}
		fileStrs := make([]string, 0, len(files))
		for _, f := range files {
			if s, ok := f.(string); ok {
				fileStrs = append(fileStrs, s)
			}
		}
		taskArgs := make(map[string]any, len(args))
		for k, v := range args {
			if k != "background" && k != "batch_files" {
				taskArgs[k] = v
			}
		}
		taskArgs["files"] = fileStrs
		taskID := taskIDs[i]
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			RunAsync(rt.asyncStore, conn, toolName, taskArgs, taskID, rt.asyncDoneCallback)
		}()
	}
	batchMapMu.Lock()
	batchMap[batchID] = taskIDs
	batchMapMu.Unlock()
	log.Printf("[PLUGIN-ASYNC-BATCH] submitted batch %s with %d tasks plugin=%s", batchID, count, conn.Name)
	resultJSON, _ := json.Marshal(map[string]any{"batch_id": batchID, "task_ids": taskIDs, "status": "pending", "count": count})
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(resultJSON)}}},
	})
	return resp
}

func handleTaskPoll(reqID json.RawMessage, taskID string, rt *pluginRuntime) []byte {
	task, err := rt.asyncStore.Get(taskID)
	if err != nil {
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": reqID, "error": map[string]any{"code": -32000, "message": err.Error()}})
		return resp
	}
	taskJSON, _ := json.Marshal(task)
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(taskJSON)}}},
	})
	return resp
}

// handleBatchPoll looks up batch_id in task IDs stored in a simple naming
// convention: batch tasks share the batch prefix. For now we store batch→taskIDs
// mapping in memory via a package-level map. [376.I]
func handleBatchPoll(reqID json.RawMessage, batchID string, rt *pluginRuntime) []byte {
	batchMapMu.RLock()
	taskIDs, ok := batchMap[batchID]
	batchMapMu.RUnlock()
	if !ok {
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": reqID, "error": map[string]any{"code": -32000, "message": "batch not found: " + batchID}})
		return resp
	}
	status := rt.asyncStore.BatchStatus(taskIDs)
	statusJSON, _ := json.Marshal(status)
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(statusJSON)}}},
	})
	return resp
}

// policy firewall [P-POLICY] before any plugin I/O. Returns nil when the
// call is permitted. On denial it records the metric rejection, logs the
// reason, and returns the JSON-RPC error body ready to send to the caller.
func rejectIfUnauthorized(conn *plugin.Connected, localName, workspaceID string, rt *pluginRuntime, metric *pluginMetricEntry, reqID json.RawMessage) []byte {
	wsPermitted := false
	for _, ws := range conn.AllowedWorkspaces {
		if ws == "*" || ws == workspaceID {
			wsPermitted = true
			break
		}
	}
	if !wsPermitted {
		log.Printf("[NEXUS-PLUGINS-ACL] DENY plugin=%s tool=%s workspace=%q (not in allowlist; use allowed_workspaces:[\"*\"] to permit all)", conn.Name, localName, workspaceID)
		metric.recordRejection()
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      reqID,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("workspace %q is not permitted to call plugin %q; add it to allowed_workspaces in ~/.neo/plugins.yaml", workspaceID, conn.Name),
			},
		})
		return resp
	}
	if rt != nil && rt.policy != nil {
		decision := rt.policy.Evaluate("plugin_tool_call", map[string]string{
			"plugin":    conn.Name,
			"tool":      localName,
			"workspace": workspaceID,
		})
		if !decision.Allowed {
			log.Printf("[NEXUS-PLUGINS-POLICY] DENY plugin=%s tool=%s reason=%s", conn.Name, localName, decision.Reason)
			metric.recordRejection()
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      reqID,
				"error": map[string]any{
					"code":    -32601,
					"message": fmt.Sprintf("plugin tool call denied by policy: %s", decision.Reason),
				},
			})
			return resp
		}
	}
	return nil
}

// pluginReloaded returns true when the named plugin is no longer in
// the runtime's connected list — typically because SIGHUP reload
// removed it (operator dropped from manifest) or removeConnAndToolsLocked
// fired (memory cgroup OOM killed it). Caller surfaces a "retry" error
// instead of the generic broken-pipe shown by CallToolWithMeta.
// [ÉPICA 152.K / DS audit fix #5]
func pluginReloaded(rt *pluginRuntime, name string) bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for i := range rt.conns {
		if rt.conns[i].Name == name {
			return false
		}
	}
	return true
}

// computeIdempotencyKey derives a stable 16-hex-char key from
// (pluginName, toolName, workspaceID, canonicalArgs). workspaceID prevents
// cross-workspace cache sharing when two workspaces call the same plugin with
// identical args.
func computeIdempotencyKey(pluginName, toolName, workspaceID string, args map[string]any) string {
	argsJSON, _ := json.Marshal(args) // nil args → "null"; stable
	h := sha256.Sum256([]byte(pluginName + "\x00" + toolName + "\x00" + workspaceID + "\x00" + string(argsJSON)))
	return hex.EncodeToString(h[:8]) // 16 hex chars = 64-bit key
}

// newTraceID returns a 32-hex-char random trace ID. Falls back to a fixed
// sentinel when the random source is unavailable (should never happen).
func newTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// workspaceIDFromBody extracts target_workspace from a JSON-RPC payload.
// Returns "" when absent or unparseable. Used as fallback when the
// X-Neo-Workspace header is not set (e.g. stateless curl POST).
func workspaceIDFromBody(body []byte) string {
	var env struct {
		Params struct {
			Arguments struct {
				TargetWorkspace string `json:"target_workspace"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &env) == nil {
		return env.Params.Arguments.TargetWorkspace
	}
	return ""
}
