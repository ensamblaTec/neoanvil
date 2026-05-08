// cmd/neo-nexus/api_plugins_call.go — REST endpoint for plugin tool dispatch.
// PILAR XXVII / 138.F.1.
//
// Allows neo-mcp children (and other internal HTTP callers) to invoke plugin
// tools without participating in the SSE transport. Body is the inner MCP
// arguments map; this handler wraps it into a JSON-RPC tools/call request
// and forwards to callPluginTool with the same authorization, idempotency,
// and policy checks as the SSE path.
//
// Endpoint: POST /api/v1/plugins/<plugin>/<tool>
// Headers:  X-Neo-Workspace: <workspace_id>  (required for ACL — P-WSACL)
// Body:     {"arguments": {...}}             (the tool's argument map)
// Returns:  the plugin's JSON-RPC response (id=1, result or error)
//
// Body cap is 1 MiB. Plugins like deepseek can carry large file contents in
// args, but anything beyond that suggests the caller should pre-distill.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

const pluginCallMaxBodyBytes = 1 << 20 // 1 MiB

func handlePluginCall(rt *pluginRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if rt == nil {
			http.Error(w, "plugin pool disabled (nexus.plugins.enabled=false)", http.StatusServiceUnavailable)
			return
		}

		// Parse URL: /api/v1/plugins/<plugin>/<tool>. The exact-match
		// /api/v1/plugins route serves status and never reaches here.
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/plugins/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "URL must be /api/v1/plugins/<plugin>/<tool>", http.StatusBadRequest)
			return
		}
		prefix, localName := parts[0], parts[1]
		// [F21] Reject embedded slashes in tool name — protects against
		// /api/v1/plugins/deepseek/red_team_audit/foo collapsing to
		// localName="red_team_audit/foo" which would silently hit a
		// non-existent tool inside the plugin.
		if strings.ContainsAny(localName, "/?#") {
			http.Error(w, "tool segment must not contain /, ?, or #", http.StatusBadRequest)
			return
		}

		wsID := r.Header.Get("X-Neo-Workspace")
		if wsID == "" {
			http.Error(w, "X-Neo-Workspace header required", http.StatusBadRequest)
			return
		}

		// Find the connected plugin by namespace prefix.
		var conn *plugin.Connected
		rt.mu.RLock()
		for i := range rt.conns {
			if rt.conns[i].NamespacePrefix == prefix {
				conn = &rt.conns[i]
				break
			}
		}
		rt.mu.RUnlock()
		if conn == nil {
			http.Error(w, fmt.Sprintf("no connected plugin with prefix %q", prefix), http.StatusNotFound)
			return
		}

		// Pre-flight health check: fast-fail if the plugin is degraded
		// (alive=false or api_key_present=false) rather than waiting the
		// full dispatch timeout (~120s). Returns 503 with actionable message.
		rt.mu.RLock()
		snap, hasSnap := rt.health[prefix]
		rt.mu.RUnlock()
		if hasSnap {
			if !snap.Alive {
				msg := "plugin " + prefix + " is not alive (poll_err: " + snap.PollErr + ") — check plugin process and logs"
				http.Error(w, msg, http.StatusServiceUnavailable)
				return
			}
			if !snap.APIKeyPresent {
				msg := "plugin " + prefix + " has no API key — run `neo login --provider " + prefix + "` to configure credentials"
				http.Error(w, msg, http.StatusServiceUnavailable)
				return
			}
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, pluginCallMaxBodyBytes))
		if err != nil {
			http.Error(w, "body read error: "+err.Error(), http.StatusBadRequest)
			return
		}

		var argsBody struct {
			Arguments json.RawMessage `json:"arguments"`
		}
		// [F4] Reject {"arguments": null} (RawMessage "null" passes raw len
		// check) and anything that isn't a JSON object — handlers downstream
		// expect a map and would otherwise hit nil-deref.
		if err := json.Unmarshal(body, &argsBody); err != nil ||
			len(argsBody.Arguments) == 0 ||
			!bytes.HasPrefix(bytes.TrimSpace(argsBody.Arguments), []byte("{")) {
			http.Error(w, `body must be {"arguments":{...}} with a JSON object`, http.StatusBadRequest)
			return
		}

		// Wrap into MCP tools/call shape expected by callPluginTool.
		// Internal `name` uses underscore (MCP spec); REST URL still uses
		// slash since URL paths permit it. ÉPICA 152 / PILAR XXIX.
		mcpBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      prefix + "_" + localName,
				"arguments": json.RawMessage(argsBody.Arguments),
			},
		})

		respBytes, err := callPluginTool(r.Context(), mcpBody, conn, localName, rt, wsID)
		if err != nil {
			log.Printf("[NEXUS-PLUGIN-CALL] %s/%s ws=%s: %v", prefix, localName, wsID, err)
			http.Error(w, "plugin call failed: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBytes)
	}
}
