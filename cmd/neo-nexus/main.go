// cmd/neo-nexus — Multi-workspace MCP dispatcher for NeoAnvil. [SRE-68]
//
// Maintains a pool of neo-mcp child processes (one per workspace) and exposes
// a single MCP endpoint that routes each tool call to the correct child.
//
// Usage: neo-nexus [--port 9000] [--bin /path/to/neo-mcp]
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/shadow"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

func main() {
	// [SRE-80.A.3] Load dispatcher config from ~/.neo/nexus.yaml (or defaults).
	// CLI flags override YAML for operator convenience.
	cfg, err := nexus.LoadNexusConfig()
	if err != nil {
		log.Fatalf("[NEXUS-FATAL] LoadNexusConfig: %v", err)
	}

	port := flag.Int("port", cfg.Nexus.DispatcherPort, "Port for the nexus dispatcher")
	binPath := flag.String("bin", cfg.Nexus.BinPath, "Path to neo-mcp binary (auto-detected if empty)")
	reconcile := flag.Bool("reconcile", false, "[282.C] Print workspace reconcile diff and send SIGHUP to running neo-nexus, then exit")
	flag.Parse()

	// [282.C] --reconcile: diff desired vs registry, apply via SIGHUP to running process.
	if *reconcile {
		runReconcile(cfg)
		return
	}

	if *binPath == "" {
		*binPath = autodetectNeoMCPBinary()
	}

	// [SRE-SSRF] Initialize SSRF trusted ports for Nexus: the full child port range
	// plus well-known internal services. Without this, SafeHTTPClient() used in scatter.go
	// blocks loopback calls to children (127.0.0.1 hits SSRF barrier on nil trustedPorts map).
	initSSRFTrustedPorts(cfg)

	log.Printf("[NEXUS] Starting multi-workspace dispatcher on %s:%d", cfg.Nexus.BindAddr, *port)
	log.Printf("[NEXUS] neo-mcp binary: %s", *binPath)
	log.Printf("[NEXUS] Child ports: %d-%d (stdin=%s, logs=%s)",
		cfg.Nexus.PortRangeBase,
		cfg.Nexus.PortRangeBase+cfg.Nexus.PortRangeSize-1,
		cfg.Nexus.Child.StdinMode,
		cfg.Nexus.Logs.Mode)

	// Load workspace registry
	registry, err := workspace.LoadRegistry()
	if err != nil {
		log.Fatalf("[NEXUS-FATAL] Failed to load workspace registry: %v", err)
	}
	log.Printf("[NEXUS] Registry loaded: %d workspace(s)", len(registry.Workspaces))

	// [Area 5.2.A+C] Notifier (Slack/Discord). Default disabled —
	// nexus.yaml::notifications.enabled=true to wire webhooks.
	// Currently always-disabled boot path because nexus.yaml doesn't
	// yet have a NotificationsConfig field; this seam means call sites
	// can use dispatchNexusEvent today and it's a no-op until the
	// config knob lands. Visible boot log confirms readiness.
	initNotifier(notifyConfigFromNexus(cfg))
	dispatchNexusEvent("nexus_boot", 2, "Nexus dispatcher started",
		"Multi-workspace dispatcher online", map[string]any{
			"port":       cfg.Nexus.DispatcherPort,
			"workspaces": len(registry.Workspaces),
		})

	// Initialize port allocator from config
	allocator := nexus.NewPortAllocator(cfg.Nexus.PortRangeBase, cfg.Nexus.PortRangeSize, cfg.Nexus.PortsFile)

	// Initialize process pool with config
	pool := nexus.NewProcessPoolWithConfig(allocator, *binPath, cfg)
	globalPool = pool // [341] expose to federation_api.go handlers

	// [PRIVILEGE-001/002] Generate a random internal token at boot. Injected into
	// every child as NEO_NEXUS_INTERNAL_TOKEN so children can authenticate their
	// /internal/certify/* and /internal/chaos/* calls back to Nexus. The token is
	// ephemeral — a new one is created on each Nexus restart.
	pool.InternalToken = mustGenerateInternalToken()
	log.Printf("[NEXUS] internal auth token generated (len=%d)", len(pool.InternalToken))

	// Context for all background goroutines — created before EnsureAll so services
	// receive cancellation on Nexus shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// [SRE-NEXUS-SVC] Ensure system-level services (Ollama LLM + embed) are running
	// before starting any children. Non-blocking when all services have enabled: false.
	// On partial failure children start anyway — embeddings degrade via circuit breaker.
	svcMgr := nexus.NewServiceManager(cfg)
	if err := svcMgr.EnsureAll(ctx); err != nil {
		log.Printf("[NEXUS-WARN] service startup partial (embedding may degrade): %v", err)
	}
	go svcMgr.WatchServices(ctx)

	// [283.A] Build project topology index — projectID → []workspaceID runtime index.
	// Must be built before StartAll so ApplyTopology can set ProjectID on each process.
	topology := nexus.BuildTopology(registry)

	// [SRE-83.B.1] Two-layer filter — extracted to filteredWorkspaces() for reuse in hot-reload.
	toStart := filteredWorkspaces(cfg, registry)
	if len(toStart) < len(registry.Workspaces) {
		log.Printf("[NEXUS] workspace filter: %d/%d will be started", len(toStart), len(registry.Workspaces))
	}

	// Start managed workspaces
	pool.StartAll(toStart)
	pool.ApplyTopology(topology) // [283.B] set ProjectID on all running processes

	// [285.D] Wire idle-transition callback: when a workspace crosses the 300s idle threshold,
	// pull recent memex entries from all project siblings and push them to its memex buffer.
	pool.OnIdleTransition = func(wsID string) {
		syncMemexOnIdle(wsID, pool)
	}

	// [PILAR-XXXVI/282.B] Boot tip: if there are workspaces in the registry but none will
	// start (e.g. managed_workspaces filter excluded everything), emit a visible hint so the
	// operator knows why the dispatcher is idle and how to fix it.
	if len(toStart) == 0 && len(registry.Workspaces) > 0 {
		log.Printf("[NEXUS-TIP] No workspaces will start. Registry has %d entries but all were filtered.", len(registry.Workspaces))
		log.Printf("[NEXUS-TIP] Check managed_workspaces in ~/.neo/nexus.yaml (empty list = start all).")
		log.Printf("[NEXUS-TIP] To reload after editing: kill -HUP $(pgrep neo-nexus)")
	}

	// Start watchdog
	go pool.WatchDog(ctx)
	go pool.IdleReaper(ctx) // [ÉPICA 150.C] no-op when cfg.Child.IdleSeconds == 0

	// [PILAR-XXIII / 125-integration] Subprocess MCP plugins. No-op when
	// nexus.plugins.enabled=false. Boots after children so plugins can
	// optionally call into the MCP fleet later. Tools are aggregated in
	// memory and exposed via /api/v1/plugins for inspection. Merging into
	// the MCP tools/list response served to clients is a separate epic.
	pluginRT := bootPluginPool(ctx, cfg)
	defer pluginRT.shutdown()

	// [354.Z-redesign] Nexus-as-god for tier:"nexus". Nexus (singleton per
	// installation) owns ~/.neo/shared/db/global.db — children proxy all
	// tier:"nexus" ops here via /api/v1/shared/nexus/*. Boot order irrelevant.
	if err := openNexusGlobalStore(); err != nil {
		log.Printf("[NEXUS-WARN] nexus-global store open failed (tier:\"nexus\" will 503): %v", err)
	}
	defer closeNexusGlobalStore()

	// [SRE-85.A.1] Operator HUD — served by Nexus, data proxied to active child.
	// [PILAR-XXVII/245.Q] DispatchPort lets the HUD reach fleet-level endpoints
	// (/status, /api/v1/metrics/summary, /api/v1/workspaces/*) that live on the
	// dispatcher mux, not on individual children.
	go ListenNexusDashboard(ctx, NexusDashboardOpts{
		Host:         cfg.Nexus.BindAddr,
		Port:         cfg.Nexus.DashboardPort,
		DispatchPort: *port,
		Registry:     registry,
		Pool:         pool,
	})

	// Status ticker — logs child summary every 30s so terminal operators
	// can see child health without tailing per-workspace log files.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, p := range pool.List() {
					log.Printf("[NEXUS-STATUS] id=%-24s port=%d status=%-12s pid=%d restarts=%d",
						p.Entry.ID, p.Port, p.Status, p.PID, p.Restarts)
				}
			}
		}
	}()

	// HTTP dispatcher
	mux := http.NewServeMux()

	// [SRE-92] SSE transport — Nexus owns the SSE stream, children are headless RPC.
	// Claude Code connects here; Nexus forwards tool calls to the resolved child
	// via POST /mcp/message and pushes responses back on the SSE stream.
	sseIdleTimeout := time.Duration(cfg.Nexus.SSEIdleTimeoutSeconds) * time.Second
	sseStore := newSSESessionStore(cfg.Nexus.MaxSSESessions, cfg.Nexus.MaxSSESessionsPerIP, sseIdleTimeout)

	// [376.E] Wire async done callback → SSE broadcast to originating workspace.
	if pluginRT != nil && pluginRT.asyncStore != nil {
		pluginRT.asyncDoneCallback = func(taskID string, task *AsyncTask) {
			if task == nil {
				return
			}
			wsID := "" // resolve from registry active workspace as fallback
			if active := registry.Active(); active != nil {
				wsID = active.ID
			}
			evt := sseEvent{
				Event: "message",
				Data:  fmt.Sprintf(`{"type":"deepseek_result","task_id":"%s","status":"%s","plugin":"%s","elapsed_ms":%d}`, taskID, task.Status, task.Plugin, task.ElapsedMs),
			}
			sent := sseStore.Broadcast(wsID, evt)
			if sent > 0 {
				log.Printf("[PLUGIN-ASYNC-SSE] broadcast task %s → %d session(s)", taskID, sent)
			}
		}
	}

	baseURL := fmt.Sprintf("http://%s:%d", cfg.Nexus.BindAddr, *port)
	mux.HandleFunc("/mcp/sse", handleSSEConnect(sseStore, registry, pool, baseURL))

	// [SRE-92] Shadow traffic mirroring — wrap /mcp/message when cfg.Shadow.Enabled.
	mcpMessageHandler := http.Handler(http.HandlerFunc(handleSSEMessage(sseStore, registry, pool, pluginRT)))
	if cfg.Nexus.Shadow.Enabled {
		mirror := shadow.NewMirror(shadow.MirrorConfig{
			Enabled:         true,
			TargetURL:       cfg.Nexus.Shadow.TargetURL,
			SampleRate:      cfg.Nexus.Shadow.SampleRate,
			TimeoutMs:       cfg.Nexus.Shadow.TimeoutMs,
			UnsafeMethods:   cfg.Nexus.Shadow.UnsafeMethods,
			DiffThresholdMs: cfg.Nexus.Shadow.DiffThresholdMs,
			BufferSize:      cfg.Nexus.Shadow.BufferSize,
		}, func(r shadow.DiffReport) {
			log.Printf("[SHADOW] verdict=%s latency_delta=%dms divergent=%v reason=%s",
				r.Verdict, r.LatencyDeltaMs, r.Divergent, r.Reason)
		})
		mcpMessageHandler = mirror.Middleware(mcpMessageHandler)
		log.Printf("[NEXUS] Shadow traffic enabled → %s (sample_rate=%.2f)", cfg.Nexus.Shadow.TargetURL, cfg.Nexus.Shadow.SampleRate)
	}
	mux.Handle("/mcp/message", mcpMessageHandler)

	// [SRE-68.3.1] REST API: list workspaces
	mux.HandleFunc("/api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			handleAddWorkspace(w, r, registry, pool)
			return
		}
		procs := pool.List()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(procs)
	})

	// [SRE-68.3.2] Start a workspace
	mux.HandleFunc("/api/v1/workspaces/start/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/start/")
		for _, ws := range registry.Workspaces {
			if ws.ID == id || ws.Name == id {
				if ws.Type == "project" {
					http.Error(w, "project-federation roots cannot be started as child processes", http.StatusBadRequest)
					return
				}
				if err := pool.Start(ws); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"status":"started","id":"%s"}`, ws.ID)
				return
			}
		}
		http.Error(w, "workspace not found", http.StatusNotFound)
	})

	// [ÉPICA 150.D] Wake a cold workspace — singleflight-coalesced spawn
	// for lazy lifecycle. Idempotent for concurrent callers (5 hits to
	// /wake/<id> while the child is still spawning all wait on the same
	// outcome instead of triggering 5 races). Returns 200 once running,
	// 504 on lazy_boot_timeout, 404 when workspace unknown.
	//
	// Mirrors /start's accept-by-ID-or-Name pattern (registry lookup
	// before pool dispatch) so the operator can use either identifier.
	mux.HandleFunc("/api/v1/workspaces/wake/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/wake/")
		if id == "" {
			http.Error(w, "missing workspace id", http.StatusBadRequest)
			return
		}
		// Resolve ID-or-Name → canonical workspace ID via registry.
		var canonicalID string
		for _, ws := range registry.Workspaces {
			if ws.ID == id || ws.Name == id {
				canonicalID = ws.ID
				break
			}
		}
		if canonicalID == "" {
			http.Error(w, "workspace not found in registry: "+id, http.StatusNotFound)
			return
		}
		w.Header().Set("X-Nexus-Booting", "1") // hint for clients that may want to retry-after
		if err := pool.EnsureRunning(canonicalID); err != nil {
			// EnsureRunning errors fall into a few categories. Surface
			// timeout as 504 so the client knows the workspace is cold
			// AGAIN and can retry. Other errors → 500.
			if strings.Contains(err.Error(), "wait timeout") {
				http.Error(w, err.Error(), http.StatusGatewayTimeout)
				return
			}
			if strings.Contains(err.Error(), "unknown workspace") {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Del("X-Nexus-Booting")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"running","id":"%s"}`, canonicalID)
	})

	// [SRE-68.3.3] Stop a workspace
	mux.HandleFunc("/api/v1/workspaces/stop/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/stop/")
		if err := pool.Stop(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"status":"stopped","id":"%s"}`, id)
	})

	// [SRE-68.3.4] Switch active workspace
	mux.HandleFunc("/api/v1/workspaces/active", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			fmt.Fprintf(w, `{"active_id":"%s"}`, registry.ActiveID)
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := registry.Select(body.ID); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_ = registry.Save()
		fmt.Fprintf(w, `{"active_id":"%s"}`, registry.ActiveID)
	})

	// [SRE-68.3.5] Add workspace
	// handled in /api/v1/workspaces POST above

	// [SRE-80.D] Proxy OAuth 2.1 and MCP discovery endpoints to the active child.
	// The MCP SDK (Claude Code) hits /.well-known/oauth-authorization-server before
	// opening the SSE stream. Forward to the active workspace's neo-mcp process which
	// handles the full OAuth flow (RFC 7591 dynamic registration + no-op token issuer).
	oauthProxy := func(w http.ResponseWriter, r *http.Request) {
		active := registry.Active()
		if active == nil {
			http.NotFound(w, r)
			return
		}
		// [SRE-83 audit] Use pool.GetProcess with StatusRunning check —
		// allocator.GetPort returns a port even for dead children.
		proc, ok := pool.GetProcess(active.ID)
		if !ok || proc.Status != nexus.StatusRunning {
			http.NotFound(w, r)
			return
		}
		proxyTo(w, r, proc.Port)
	}
	mux.HandleFunc("/.well-known/", oauthProxy)
	mux.HandleFunc("/oauth/", oauthProxy)

	// [Area 4.2.A + Bug-6 fix] Proxy /openapi.json + /docs to the
	// active workspace's child neo-mcp. The endpoints live on the
	// child's sseMux (cmd/neo-mcp/main.go); without this proxy the
	// dispatcher returned 404 for them. Reuses the oauthProxy
	// active-workspace lookup so the operator hits a single URL
	// regardless of which workspace is active.
	mux.HandleFunc("/openapi.json", oauthProxy)
	mux.HandleFunc("/docs", oauthProxy)

	// Health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","workspaces":%d}`, len(pool.List()))
	})

	// [SRE-82.A.1] Status endpoint — full child pool state for dashboards / operators.
	// Returns JSON array: each element mirrors WorkspaceProcess + computed uptime_seconds.
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		type childStatus struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			Type             string `json:"type"`
			Path             string `json:"path"`
			Port             int    `json:"port"`
			Status           string `json:"status"`
			PID              int    `json:"pid"`
			Restarts         int    `json:"restarts"`
			UptimeSeconds    int64  `json:"uptime_seconds"`
			LastPingAgoSec   int64  `json:"last_ping_ago_seconds"`
			LastToolCallUnix int64  `json:"last_tool_call_unix"`        // [Épica 248.C] 0=never
			ToolCallCount    int64  `json:"tool_call_count"`            // [Épica 248.C]
			IdleSeconds      int64  `json:"idle_seconds"`               // [Épica 248.C] 0 if never used
			ProjectID         string `json:"project_id,omitempty"`          // [283.D]
			LastMemexSyncUnix int64  `json:"last_memex_sync_unix,omitempty"` // [285.E]
		}
		procs := pool.List()
		out := make([]childStatus, 0, len(procs))
		now := time.Now()
		for _, p := range procs {
			var uptime int64
			if !p.StartedAt.IsZero() {
				uptime = int64(now.Sub(p.StartedAt).Seconds())
			}
			var pingAgo int64
			if !p.LastPing.IsZero() {
				pingAgo = int64(now.Sub(p.LastPing).Seconds())
			}
			wsType := p.Entry.Type
			if wsType == "" {
				wsType = "workspace"
			}
			// [Épica 248.C] LastToolCallUnix/ToolCallCount come from List() which
			// already uses atomic.Load — plain field access on the copy is safe.
			var idleSec int64
			if p.LastToolCallUnix > 0 {
				idleSec = now.Unix() - p.LastToolCallUnix
			}
			out = append(out, childStatus{
				ID:                p.Entry.ID,
				Name:              p.Entry.Name,
				Type:              wsType,
				Path:              filepath.Base(p.Entry.Path), // [145.A] basename only — full path leaks FS layout

				Port:              p.Port,
				Status:            string(p.Status),
				PID:               p.PID,
				Restarts:          p.Restarts,
				UptimeSeconds:     uptime,
				LastPingAgoSec:    pingAgo,
				LastToolCallUnix:  p.LastToolCallUnix,
				ToolCallCount:     p.ToolCallCount,
				IdleSeconds:       idleSec,
				ProjectID:         p.ProjectID,         // [283.D]
				LastMemexSyncUnix: p.LastMemexSyncUnix, // [285.E]
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	// [SRE-NEXUS-SVC] Service manager status — reports Ollama instance health.
	// [PILAR-XXIII / 125-integration] Plugin runtime status — operator JSON.
	mux.HandleFunc("/api/v1/plugins", handlePluginsStatus(pluginRT))
	// [ÉPICA 154.D] Plugin call observability — p50/p95/p99 latency,
	// counts (calls/errors/rejections/cache_hits) per (plugin, tool).
	// Read-only; respects API.AuthToken via the same auth middleware.
	mux.HandleFunc("/api/v1/plugin_metrics", handlePluginMetrics)
	// [PILAR-XXVII / 138.F.1] Plugin tool dispatch — POST /api/v1/plugins/<plugin>/<tool>
	// allows internal HTTP callers (neo-mcp daemon dispatch, scripts) to invoke
	// plugin tools without joining the SSE transport. Reuses callPluginTool so
	// ACL + policy + idempotency checks fire identically to the SSE path.
	mux.HandleFunc("/api/v1/plugins/", handlePluginCall(pluginRT))

	// [376.F] Async task inspection — REST endpoints for polling background
	// plugin calls without MCP. List + single-task GET.
	mux.HandleFunc("/api/v1/async/tasks/", handleAsyncTaskGet(pluginRT))
	mux.HandleFunc("/api/v1/async/tasks", handleAsyncTaskList(pluginRT))

	mux.HandleFunc("/api/v1/services", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(svcMgr.List())
	})

	// [PILAR-XXXVI/279] GET /api/v1/projects — list registry entries with Type="project".
	// POST /api/v1/projects — register a project root (body: {"path": "..."}).
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			var projects []workspace.WorkspaceEntry
			for _, e := range registry.Workspaces {
				if e.Type == "project" {
					projects = append(projects, e)
				}
			}
			if projects == nil {
				projects = []workspace.WorkspaceEntry{}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(projects)
		case http.MethodPost:
			// [PILAR-XXXVI/279.B] Register a project root in the registry.
			// A project root has Type="project" and does NOT spawn a neo-mcp worker.
			var body struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
				http.Error(w, "body must be JSON {\"path\": \"...\"}", http.StatusBadRequest)
				return
			}
			absPath, err := filepath.Abs(body.Path)
			if err != nil {
				http.Error(w, "invalid path: "+err.Error(), http.StatusBadRequest)
				return
			}
			entry, err := registry.Add(absPath)
			if err != nil {
				http.Error(w, "registry add: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if err := registry.Save(); err != nil {
				http.Error(w, "registry save: "+err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("[NEXUS-EVENT] project_registered id=%s name=%s path=%s", entry.ID, entry.Name, entry.Path)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(entry)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// [PILAR-XXXVI/279.C + 283.E] GET /api/v1/projects/:id/health|activity.
	mux.HandleFunc("/api/v1/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		projectID := parts[0]
		subPath := parts[1]

		// [283.E] Activity endpoint.
		if subPath == "activity" {
			counters := pool.GetProjectActivity(projectID)
			members := topology.SiblingsOf(projectID)
			if len(members) == 0 {
				members = []string{}
			}
			activeCnt := 0
			minIdle := int64(0)
			now := time.Now().Unix()
			type memberStat struct {
				WorkspaceID   string `json:"workspace_id"`
				Name          string `json:"name"`
				Status        string `json:"status"`
				IdleSeconds   int64  `json:"idle_seconds"`
				ToolCallCount int64  `json:"tool_call_count"`
			}
			memberStats := make([]memberStat, 0, len(members))
			for _, mid := range members {
				p, ok := pool.GetProcess(mid)
				if !ok {
					continue
				}
				idle := int64(0)
				if p.LastToolCallUnix > 0 {
					idle = now - p.LastToolCallUnix
				}
				if p.Status == nexus.StatusRunning && idle < 300 {
					activeCnt++
					if minIdle == 0 || idle < minIdle {
						minIdle = idle
					}
				}
				memberStats = append(memberStats, memberStat{
					WorkspaceID:   mid,
					Name:          p.Entry.Name,
					Status:        string(p.Status),
					IdleSeconds:   idle,
					ToolCallCount: p.ToolCallCount,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"project_id":         projectID,
				"active_members":     activeCnt,
				"total_members":      len(members),
				"last_tool_call_unix": counters.LastToolCallUnix,
				"tool_call_count":    counters.ToolCallCount,
				"min_idle_seconds":   minIdle,
				"per_member":         memberStats,
			})
			return
		}

		var projectEntry *workspace.WorkspaceEntry
		for i := range registry.Workspaces {
			if registry.Workspaces[i].ID == projectID && registry.Workspaces[i].Type == "project" {
				projectEntry = &registry.Workspaces[i]
				break
			}
		}
		if projectEntry == nil {
			http.Error(w, fmt.Sprintf("project %q not found", projectID), http.StatusNotFound)
			return
		}
		pc, err := config.LoadProjectConfig(projectEntry.Path)
		if err != nil {
			http.Error(w, "load project config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		type memberHealth struct {
			Path    string `json:"path"`
			Status  string `json:"status"`
			Running bool   `json:"running"`
		}
		results := make([]memberHealth, 0, len(pc.MemberWorkspaces))
		for _, memberPath := range pc.MemberWorkspaces {
			// Find matching registry entry to get the workspace ID.
			mh := memberHealth{Path: memberPath}
			for _, ws := range registry.Workspaces {
				if ws.Path == memberPath {
					proc, ok := pool.GetProcess(ws.ID)
					if ok && proc.Status == nexus.StatusRunning {
						mh.Status = "running"
						mh.Running = true
					} else {
						mh.Status = "not_running"
					}
					break
				}
			}
			if mh.Status == "" {
				mh.Status = "not_registered"
			}
			results = append(results, mh)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})

	// [SRE-82.B.1/92] URL-based workspace routing — /workspaces/{id}/mcp/sse and /message.
	// Allows .mcp.json to target a specific workspace by ID without the X-Neo-Workspace header.
	// For /mcp/sse: create an SSE session pinned to the specific child (children are headless).
	// For /mcp/message and other paths: proxy directly to the child.
	workspaceMCPHandler := func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/workspaces/")
		parts := strings.SplitN(path, "/", 2)
		id := parts[0]
		if id == "" {
			http.Error(w, "missing workspace id in path", http.StatusBadRequest)
			return
		}
		proc, ok := pool.GetProcess(id)
		if !ok {
			http.Error(w, fmt.Sprintf("workspace %q not in pool", id), http.StatusServiceUnavailable)
			return
		}
		// Wait for the child to become healthy when it's still starting.
		// Without this, SSE reconnects after rebuild-restart get 503 while
		// the target workspace boots (HNSW/CPG load), and Claude Code falls
		// back to the generic /mcp/sse which routes to whatever workspace
		// finished first — breaking workspace affinity.
		if proc.Status != nexus.StatusRunning {
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			ready := false
			for !ready {
				select {
				case <-r.Context().Done():
					http.Error(w, "client disconnected", http.StatusServiceUnavailable)
					return
				case <-deadline.C:
					http.Error(w, fmt.Sprintf("workspace %q not ready after 30s (status=%s)", id, proc.Status), http.StatusServiceUnavailable)
					return
				case <-ticker.C:
					proc, ok = pool.GetProcess(id)
					if ok && proc.Status == nexus.StatusRunning {
						ready = true
					}
				}
			}
		}
		// For SSE connect: create a session pinned to this child's port.
		subPath := ""
		if len(parts) > 1 {
			subPath = parts[1]
		}
		// [Épica 248.B] Record tool-call activity on every POST — covers both the
		// direct /mcp/message path and the streamable-HTTP /mcp/sse POST variant.
		if r.Method == http.MethodPost {
			pool.RecordToolCall(id)
		}
		if subPath == "mcp/sse" && r.Method == http.MethodGet {
			// Inject workspace header so handleSSEConnect resolves the correct child.
			r.Header.Set("X-Neo-Workspace", id)
			handleSSEConnect(sseStore, registry, pool, baseURL)(w, r)
			return
		}
		// Streamable HTTP (MCP 2025-03-26): client POSTs to the same URL as the SSE GET.
		// Route to handleSSEMessage so the response comes back inline in the POST body.
		// [ÉPICA 152 / PILAR XXIX] Also route POST /workspaces/<id>/mcp/message
		// through handleSSEMessage — without this the path stripped to /mcp/message
		// and proxyTo'd directly to the child, BYPASSING interceptPluginTools and
		// detectPluginToolCall so plugin tool invocations from stateless POSTs
		// (curl, Streamable HTTP without sessionId) returned "tool not found".
		if (subPath == "mcp/sse" || subPath == "mcp/message") && r.Method == http.MethodPost {
			r.Header.Set("X-Neo-Workspace", id)
			handleSSEMessage(sseStore, registry, pool, pluginRT)(w, r)
			return
		}
		// [Épica 229.3] Strip the /workspaces/<id> prefix before proxying so the
		// child sees its routes at root (/.well-known/*, /oauth/*, /health, etc.).
		// Without this the Claude Code SDK's relative-URL OAuth discovery (e.g.
		// GET /workspaces/<id>/.well-known/oauth-protected-resource) landed at the
		// child with a prefix the child's mux doesn't know → 404 text/plain → SDK
		// parse-error cascade that looked like an auth failure.
		r.URL.Path = "/" + subPath
		proxyTo(w, r, proc.Port)
	}
	mux.HandleFunc("/workspaces/", workspaceMCPHandler)

	// [PILAR-XXVII/244] Observability summary + per-workspace proxy.
	// These live on the global API path prefix so the auth middleware
	// below covers them when API.AuthToken is configured.
	mux.HandleFunc("/api/v1/workspaces/", handleWorkspaceMetrics(pool))
	mux.HandleFunc("/api/v1/metrics/summary", handleMetricsSummary(pool))

	// [292.D] POST /internal/contract/broadcast — neo-mcp child posts breaking changes here.
	// Nexus re-broadcasts to all project siblings via /internal/contract/alert.
	mux.HandleFunc("/internal/contract/broadcast", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Breaking      []json.RawMessage `json:"breaking"`
			WorkspaceID   string            `json:"workspace_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(payload.Breaking) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		go broadcastContractDrift(payload.WorkspaceID, payload.Breaking, pool)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"queued":%d}`, len(payload.Breaking))
	})

	// [354.Z-redesign] tier:"nexus" REST endpoints (/api/v1/shared/nexus/*).
	// Nexus is the sole owner of ~/.neo/shared/db/global.db; children proxy here.
	registerNexusSharedHandlers(mux)

	// [PILAR LXVI / 351.B] /internal/nexus/debt GET/POST/affecting endpoints
	// — exposed iff nexus.yaml debt.enabled:true (pool.Debt non-nil). Resolve
	// endpoint requires X-Nexus-Token when api.auth_token is configured.
	registerNexusDebtHandlers(mux, pool, cfg)

	// [Épica 296.E] POST /internal/knowledge/broadcast — neo-mcp posts here after any store write.
	// Nexus fans out /internal/knowledge/refresh to all project siblings.
	mux.HandleFunc("/internal/knowledge/broadcast", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		srcWs := r.URL.Query().Get("src")
		go broadcastKnowledgeRefresh(srcWs, pool)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"queued":true}`)
	})

	// [345.A] POST /internal/certify/{workspace_id} — route certify call to owning child's MCP.
	mux.HandleFunc("/internal/certify/", handleInternalCertify(pool))
	// [346.A] POST /internal/chaos/{workspace_id} — route chaos drill to owning child's MCP.
	mux.HandleFunc("/internal/chaos/", handleInternalChaos(pool))
	// [347.A] POST /internal/graph_walk/{workspace_id} — proxy GRAPH_WALK to owning child's MCP.
	mux.HandleFunc("/internal/graph_walk/", handleInternalGraphWalk(pool))
	// [348.A] POST /internal/vacuum/begin/{workspace_id} — proxy Vacuum_Memory to owning child's MCP.
	mux.HandleFunc("/internal/vacuum/begin/", handleInternalVacuumBegin(pool))

	// [337.A] POST /internal/presence — neo-mcp children heartbeat presence info.
	mux.HandleFunc("/internal/presence", handlePresenceHeartbeat)
	// [337.A] GET /api/v1/presence — list active agent sessions (< 2min stale).
	mux.HandleFunc("/api/v1/presence", handlePresenceList)
	// [341] GET /api/v1/federation/overview — merged workspace status + presence + activity log.
	mux.HandleFunc("/api/v1/federation/overview", handleFederationOverview)

	// [SRE-80.C.3] API auth token middleware — gates /api/v1/* when configured.
	// [PRIVILEGE-001/002] Also gates /internal/certify/* and /internal/chaos/* with
	// X-Neo-Internal-Token (the ephemeral token injected into children at boot).
	// /internal/presence, /internal/contract/*, /internal/knowledge/*, /internal/graph_walk/*,
	// /internal/vacuum/* and /internal/nexus/* are intentionally left open — they are
	// called by child processes that may not carry the internal token yet (e.g. during boot).
	internalToken := pool.InternalToken
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gate privileged internal endpoints — certify and chaos can trigger significant
		// computation and must only be callable by Nexus-spawned children.
		if strings.HasPrefix(r.URL.Path, "/internal/certify/") ||
			strings.HasPrefix(r.URL.Path, "/internal/chaos/") {
			if r.Header.Get("X-Neo-Internal-Token") != internalToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	if cfg.Nexus.API.AuthToken != "" {
		token := cfg.Nexus.API.AuthToken
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/v1/") {
				if r.Header.Get("X-Nexus-Token") != token {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			inner.ServeHTTP(w, r)
		})
		log.Printf("[NEXUS] API auth enabled (X-Nexus-Token required on /api/v1/*)")
	}
	log.Printf("[NEXUS] internal endpoint auth enabled (/internal/certify/* and /internal/chaos/* require X-Neo-Internal-Token)")

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Nexus.BindAddr, *port),
		Handler: handler,
	}

	go func() {
		log.Printf("[NEXUS] Dispatcher listening on http://%s:%d", cfg.Nexus.BindAddr, *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[NEXUS-FATAL] Server error: %v", err)
		}
	}()

	// [SRE-84.A.1] SIGHUP hot-reload — reloads nexus.yaml and reconciles the child pool
	// without restarting the dispatcher. Operator: kill -HUP $(pgrep neo-nexus)
	// [PILAR-XXIII] Also reloads ~/.neo/plugins.yaml and reconciles the plugin pool.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go func() {
		for range hupCh {
			reloadConfig(cfg, registry, pool, topology) // [283.F] rebuild topology on SIGHUP
			if pluginRT != nil {
				if err := pluginRT.reload(ctx); err != nil {
					log.Printf("[NEXUS-PLUGINS-RELOAD] error: %v", err)
				}
			}
		}
	}()

	// [PILAR-XXXVI/282] Auto-reconcile on workspace registry changes.
	// Polls ~/.neo/workspaces.json mtime every 5s; reconciles pool when it changes.
	go watchRegistryChanges(ctx, cfg, registry, pool)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("[NEXUS] Shutting down...")
	cancel()
	svcMgr.StopAll()
	pool.StopAll()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	_ = server.Shutdown(shutCtx)
	log.Println("[NEXUS] Shutdown complete.")
}

// resolveChildPort determines which child should handle a non-message request
// (e.g. SSE connect). Uses header → active workspace fallback. [SRE-85.B]
func resolveChildPort(r *http.Request, registry *workspace.Registry, pool *nexus.ProcessPool) int {
	// Check X-Neo-Workspace header.
	if wsHeader := r.Header.Get("X-Neo-Workspace"); wsHeader != "" {
		if port := lookupWorkspacePort(wsHeader, registry, pool); port != 0 {
			return port
		}
	}
	// Fallback: active workspace — only if its child is healthy.
	return activeWorkspacePort(registry, pool)
}

// resolveChildPortFromMessage extracts target_workspace from JSON-RPC payload,
// falling back to header → active workspace. Body is re-wound after reading. [SRE-85.B.2]
func resolveChildPortFromMessage(r *http.Request, registry *workspace.Registry, pool *nexus.ProcessPool) int {
	// Read body to extract target_workspace from JSON-RPC params.
	if r.Method == "POST" && r.Body != nil {
		bodyBytes, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(bodyBytes))) // re-wind

		// [SRE-85.B.2] Extract params.arguments.target_workspace from JSON-RPC.
		var envelope struct {
			Params struct {
				Arguments struct {
					TargetWorkspace string `json:"target_workspace"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(bodyBytes, &envelope) == nil && envelope.Params.Arguments.TargetWorkspace != "" {
			if port := lookupWorkspacePort(envelope.Params.Arguments.TargetWorkspace, registry, pool); port != 0 {
				return port
			}
		}
	}
	// Fall through to header → active workspace.
	if wsHeader := r.Header.Get("X-Neo-Workspace"); wsHeader != "" {
		if port := lookupWorkspacePort(wsHeader, registry, pool); port != 0 {
			return port
		}
	}
	return activeWorkspacePort(registry, pool)
}

// lookupWorkspacePort finds a running child by workspace ID or name.
func lookupWorkspacePort(idOrName string, registry *workspace.Registry, pool *nexus.ProcessPool) int {
	for _, ws := range registry.Workspaces {
		if ws.ID == idOrName || ws.Name == idOrName {
			proc, ok := pool.GetProcess(ws.ID)
			if ok && proc.Status == nexus.StatusRunning {
				return proc.Port
			}
		}
	}
	return 0
}

// activeWorkspacePort returns the port of the active workspace if it's healthy.
func activeWorkspacePort(registry *workspace.Registry, pool *nexus.ProcessPool) int {
	active := registry.Active()
	if active == nil {
		return 0
	}
	proc, ok := pool.GetProcess(active.ID)
	if ok && proc.Status == nexus.StatusRunning {
		return proc.Port
	}
	log.Printf("[NEXUS-WARN] active workspace %q unhealthy (status=%s) — no fallback port", active.ID, func() string {
		if ok {
			return string(proc.Status)
		}
		return "not_in_pool"
	}())
	return 0
}

// [PILAR-XXVIII hotfix] Shared transport for every proxyTo call.
// NewSingleHostReverseProxy per request allocated a fresh Transport
// with its own connection pool — with SSE long-lived connections
// plus frequent HUD polling, that leaked 3000+ ESTABLISHED sockets
// against the child. The shared Transport cap keep-alive conn usage
// bounded (32/host) and reuses sockets across requests.
var proxyToTransport = &http.Transport{
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     60 * time.Second,
}

// proxyTo forwards the request to a child process port. [SRE-68.2.4]
func proxyTo(w http.ResponseWriter, r *http.Request, port int) {
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = proxyToTransport
	proxy.ServeHTTP(w, r)
}

// runReconcile diffs the workspace registry against nexus.yaml managed_workspaces,
// prints what would start/stop, then sends SIGHUP to the running neo-nexus process
// so it reloads without restart. [282.C]
func runReconcile(cfg *nexus.NexusConfig) {
	registry, err := workspace.LoadRegistry()
	if err != nil {
		log.Fatalf("[reconcile] LoadRegistry: %v", err)
	}

	desired := filteredWorkspaces(cfg, registry)
	fmt.Printf("[neo-nexus reconcile] Registry: %d workspaces — Desired (filtered): %d\n\n",
		len(registry.Workspaces), len(desired))

	fmt.Println("Workspaces that would start:")
	if len(desired) == 0 {
		fmt.Println("  (none)")
	}
	for _, ws := range desired {
		fmt.Printf("  + %s (%s)\n", ws.Name, ws.ID)
	}

	skipped := len(registry.Workspaces) - len(desired)
	if skipped > 0 {
		fmt.Printf("\nWorkspaces that would be skipped (project/stdio/filter): %d\n", skipped)
		for _, reg := range registry.Workspaces {
			skip := true
			for _, d := range desired {
				if d.ID == reg.ID {
					skip = false
					break
				}
			}
			if skip {
				reason := "filter"
				if reg.Type == "project" {
					reason = "project-root"
				} else if reg.Transport == "stdio" {
					reason = "stdio"
				}
				fmt.Printf("  - %s (%s) [%s]\n", reg.Name, reg.ID, reason)
			}
		}
	}

	// Send SIGHUP to running neo-nexus to apply the reconcile without restart.
	fmt.Println("\nTo apply to the running neo-nexus without restart:")
	fmt.Println("  kill -HUP $(pgrep neo-nexus)")
	fmt.Println("\nSending SIGHUP to running neo-nexus...")
	if err := exec.Command("sh", "-c", "kill -HUP $(pgrep -x neo-nexus) 2>/dev/null || echo '  [no running neo-nexus found]'").Run(); err != nil { //nolint:gosec // G204-SHELL-WITH-VALIDATION: literal command, no user input
		fmt.Println("  [SIGHUP failed — is neo-nexus running?]")
	} else {
		fmt.Println("  SIGHUP sent. neo-nexus will reload nexus.yaml and reconcile the pool.")
	}
}

// handleAddWorkspace registers a new workspace. [SRE-68.3.5]
// [SRE-83.A.2] Accepts optional "transport" field ("sse"|"stdio"|"").
func handleAddWorkspace(w http.ResponseWriter, r *http.Request, registry *workspace.Registry, pool *nexus.ProcessPool) {
	var body struct {
		Path      string `json:"path"`
		Transport string `json:"transport"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	// Validate transport enum.
	switch body.Transport {
	case "", "sse", "stdio":
		// valid
	default:
		http.Error(w, `transport must be "sse", "stdio", or "" (empty)`, http.StatusBadRequest)
		return
	}
	entry, err := registry.Add(body.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Transport != "" {
		entry.Transport = body.Transport
	}
	_ = registry.Save()
	// Only auto-start if transport is sse (or unset — backward compat).
	// Project federation roots are meta-nodes and never run a neo-mcp worker. [Épica 261.C]
	if entry.Transport != "stdio" && entry.Type != "project" {
		_ = pool.Start(*entry)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entry)
}

// filteredWorkspaces applies the two-layer filter (transport flag + managed_workspaces)
// and returns the subset of registry entries that Nexus should run as SSE children.
// Extracted from main() so both startup and hot-reload share identical logic. [SRE-83.B.1/84.A.2]
func filteredWorkspaces(cfg *nexus.NexusConfig, registry *workspace.Registry) []workspace.WorkspaceEntry {
	var managedAllowed map[string]struct{}
	if len(cfg.Nexus.ManagedWorkspaces) > 0 {
		managedAllowed = make(map[string]struct{}, len(cfg.Nexus.ManagedWorkspaces))
		for _, v := range cfg.Nexus.ManagedWorkspaces {
			managedAllowed[v] = struct{}{}
		}
	}
	out := make([]workspace.WorkspaceEntry, 0, len(registry.Workspaces))
	for _, ws := range registry.Workspaces {
		// [Épica 261.C] Project federation roots are meta-nodes — they coordinate
		// member workspaces but do not run a neo-mcp worker themselves.
		if ws.Type == "project" {
			log.Printf("[NEXUS] skipping project federation root id=%s name=%s", ws.ID, ws.Name)
			continue
		}
		switch ws.Transport {
		case "sse":
			out = append(out, ws)
		case "stdio":
			log.Printf("[NEXUS] skipping stdio workspace id=%s name=%s", ws.ID, ws.Name)
		default:
			if managedAllowed != nil {
				if _, ok := managedAllowed[ws.ID]; ok {
					out = append(out, ws)
					continue
				}
				if _, ok := managedAllowed[ws.Name]; ok {
					out = append(out, ws)
				}
			} else {
				out = append(out, ws)
			}
		}
	}
	return out
}

// reloadConfig re-reads ~/.neo/nexus.yaml and reconciles the running child pool:
// stops workspaces that dropped out of the filter, starts ones that entered it.
// Called from the SIGHUP goroutine in main(). [SRE-84.A.2]
// watchRegistryChanges polls ~/.neo/workspaces.json and reconciles the child pool
// whenever the file's mtime changes. [PILAR-XXXVI/282]
func watchRegistryChanges(ctx context.Context, cfg *nexus.NexusConfig, registry *workspace.Registry, pool *nexus.ProcessPool) {
	regPath := workspace.DefaultRegistryPath()
	var lastMod time.Time
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(regPath)
			if err != nil {
				continue
			}
			if fi.ModTime().Equal(lastMod) {
				continue
			}
			lastMod = fi.ModTime()
			newReg, loadErr := workspace.LoadRegistry()
			if loadErr != nil {
				log.Printf("[NEXUS-WARN] registry_watch_reload_failed: %v", loadErr)
				continue
			}
			registry.Workspaces = newReg.Workspaces
			registry.ActiveID = newReg.ActiveID
			log.Printf("[NEXUS-EVENT] registry_changed: %d workspaces", len(registry.Workspaces))
			reconcilePool(cfg, registry, pool)
		}
	}
}

// reconcilePool diffs the desired workspace set against running children and
// starts/stops as needed. Shared by SIGHUP reload and registry watcher.
func reconcilePool(cfg *nexus.NexusConfig, registry *workspace.Registry, pool *nexus.ProcessPool) {
	desired := filteredWorkspaces(cfg, registry)
	desiredMap := make(map[string]struct{}, len(desired))
	for _, ws := range desired {
		desiredMap[ws.ID] = struct{}{}
	}
	for _, p := range pool.List() {
		if _, ok := desiredMap[p.Entry.ID]; !ok {
			log.Printf("[NEXUS-EVENT] reconcile stopping id=%s (not in desired set)", p.Entry.ID)
			_ = pool.Stop(p.Entry.ID)
		}
	}
	runningMap := make(map[string]struct{}, len(pool.List()))
	for _, p := range pool.List() {
		runningMap[p.Entry.ID] = struct{}{}
	}
	for _, ws := range desired {
		if _, ok := runningMap[ws.ID]; !ok {
			log.Printf("[NEXUS-EVENT] reconcile starting id=%s (new in desired set)", ws.ID)
			_ = pool.Start(ws)
		}
	}
}

func reloadConfig(cfg *nexus.NexusConfig, registry *workspace.Registry, pool *nexus.ProcessPool, topo *nexus.TopologyIndex) {
	newCfg, err := nexus.LoadNexusConfig()
	if err != nil {
		log.Printf("[NEXUS-WARN] config_reload_failed: %v", err)
		return
	}

	desired := filteredWorkspaces(newCfg, registry)
	desiredMap := make(map[string]struct{}, len(desired))
	for _, ws := range desired {
		desiredMap[ws.ID] = struct{}{}
	}

	// Stop children no longer in the desired set.
	for _, p := range pool.List() {
		if _, ok := desiredMap[p.Entry.ID]; !ok {
			log.Printf("[NEXUS-EVENT] config_reloaded stopping id=%s (removed from filter)", p.Entry.ID)
			_ = pool.Stop(p.Entry.ID)
		}
	}

	// Start children newly in the desired set.
	runningMap := make(map[string]struct{}, len(pool.List()))
	for _, p := range pool.List() {
		runningMap[p.Entry.ID] = struct{}{}
	}
	for _, ws := range desired {
		if _, ok := runningMap[ws.ID]; !ok {
			log.Printf("[NEXUS-EVENT] config_reloaded starting id=%s (added to filter)", ws.ID)
			_ = pool.Start(ws)
		}
	}

	// [283.F] Rebuild topology index so new/removed project members are reflected.
	topo.Rebuild(registry)
	pool.ApplyTopology(topo)

	// Promote new config so future reloads diff against the latest state.
	*cfg = *newCfg
	log.Printf("[NEXUS-EVENT] config_reloaded managed_workspaces=%d active_children=%d",
		len(newCfg.Nexus.ManagedWorkspaces), len(pool.List()))
}

// syncMemexOnIdle pulls recent memex entries from project siblings and pushes them
// into wsID's /internal/memex/import endpoint. [285.D]
// Called from the pool.OnIdleTransition edge-trigger — runs in its own goroutine.
func syncMemexOnIdle(wsID string, pool *nexus.ProcessPool) {
	target, ok := pool.GetProcess(wsID)
	if !ok || target.Port == 0 {
		return
	}
	siblings := pool.List()
	client := sre.SafeInternalHTTPClient(10)
	now := time.Now().Unix()

	var allEntries []json.RawMessage
	for _, sibling := range siblings {
		if sibling.Entry.ID == wsID || sibling.Port == 0 {
			continue
		}
		since := target.LastMemexSyncUnix
		url := fmt.Sprintf("http://127.0.0.1:%d/internal/memex/recent?n=50&since=%d", sibling.Port, since)
		resp, err := client.Get(url) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient, loopback-only
		if err != nil {
			log.Printf("[285.D] memex fetch from %s: %v", sibling.Entry.ID, err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		var batch []json.RawMessage
		if json.Unmarshal(body, &batch) == nil {
			allEntries = append(allEntries, batch...)
		}
	}
	if len(allEntries) == 0 {
		pool.UpdateLastMemexSync(wsID, now)
		return
	}

	payload, err := json.Marshal(allEntries)
	if err != nil {
		return
	}
	importURL := fmt.Sprintf("http://127.0.0.1:%d/internal/memex/import", target.Port)
	resp, err := client.Post(importURL, "application/json", strings.NewReader(string(payload))) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT
	if err != nil {
		log.Printf("[285.D] memex import to %s: %v", wsID, err)
		return
	}
	resp.Body.Close()
	pool.UpdateLastMemexSync(wsID, now)
	log.Printf("[285.D] memex synced to %s: %d candidate entries from %d siblings", wsID, len(allEntries), len(siblings)-1)
}

// broadcastContractDrift POSTs breaking changes to all project siblings. [292.D]
// Fire-and-forget with semaphore=4.
func broadcastContractDrift(srcWsID string, breaking []json.RawMessage, pool *nexus.ProcessPool) {
	if pool == nil || len(breaking) == 0 {
		return
	}
	srcProc, ok := pool.GetProcess(srcWsID)
	if !ok || srcProc.ProjectID == "" {
		return
	}
	siblings := pool.SiblingsByProject(srcProc.ProjectID)

	payload, err := json.Marshal(map[string]any{
		"breaking":       breaking,
		"from_workspace": srcWsID,
	})
	if err != nil {
		return
	}

	sem := make(chan struct{}, 4)
	client := sre.SafeInternalHTTPClient(5)
	for _, sib := range siblings {
		if sib.Entry.ID == srcWsID || sib.Port == 0 {
			continue
		}
		sem <- struct{}{}
		go func(port int, sibID string) {
			defer func() { <-sem }()
			url := fmt.Sprintf("http://127.0.0.1:%d/internal/contract/alert", port)
			resp, err := client.Post(url, "application/json", strings.NewReader(string(payload))) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT
			if err != nil {
				log.Printf("[NEXUS-EVENT] contract_drift_broadcast src=%s dst=%s err=%v", srcWsID, sibID, err)
				return
			}
			resp.Body.Close()
			log.Printf("[NEXUS-EVENT] contract_drift_broadcast src=%s dst=%s breaking=%d", srcWsID, sibID, len(breaking))
		}(sib.Port, sib.Entry.ID)
	}
	// Drain semaphore
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
}

// broadcastKnowledgeRefresh POSTs /internal/knowledge/refresh to all running siblings. [296.E]
// Fire-and-forget with semaphore=4. srcWsID may be empty (non-project workspaces).
func broadcastKnowledgeRefresh(srcWsID string, pool *nexus.ProcessPool) {
	if pool == nil {
		return
	}
	var siblings []nexus.WorkspaceProcess
	if srcWsID != "" {
		if src, ok := pool.GetProcess(srcWsID); ok && src.ProjectID != "" {
			siblings = pool.SiblingsByProject(src.ProjectID)
		}
	}
	if len(siblings) == 0 {
		siblings = pool.List()
	}

	sem := make(chan struct{}, 4)
	client := sre.SafeInternalHTTPClient(5)
	for _, sib := range siblings {
		if sib.Entry.ID == srcWsID || sib.Port == 0 {
			continue
		}
		sem <- struct{}{}
		go func(port int, sibID string) {
			defer func() { <-sem }()
			url := fmt.Sprintf("http://127.0.0.1:%d/internal/knowledge/refresh", port)
			resp, err := client.Post(url, "application/json", strings.NewReader("{}")) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: loopback-only via SafeInternalHTTPClient
			if err != nil {
				log.Printf("[NEXUS-EVENT] knowledge_refresh src=%s dst=%s err=%v", srcWsID, sibID, err)
				return
			}
			resp.Body.Close()
			log.Printf("[NEXUS-EVENT] knowledge_refresh src=%s dst=%s ok", srcWsID, sibID)
		}(sib.Port, sib.Entry.ID)
	}
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
}

// autodetectNeoMCPBinary returns the path of the neo-mcp binary the
// dispatcher will spawn. Looks for it next to the running binary first
// (matches our build layout); falls back to "neo-mcp" so the caller
// can rely on $PATH lookup. Extracted from main() to keep CC ≤ 15.
// [CC refactor]
func autodetectNeoMCPBinary() string {
	exe, _ := os.Executable()
	candidate := filepath.Join(filepath.Dir(exe), "neo-mcp")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return "neo-mcp"
}

// initSSRFTrustedPorts seeds sre.InitTrustedPorts with the full child
// port range plus well-known internal services. Without this,
// SafeHTTPClient() used in scatter.go blocks loopback calls to
// children (127.0.0.1 hits the SSRF barrier on a nil trustedPorts
// map). Extracted from main() to keep CC ≤ 15. [CC refactor]
func initSSRFTrustedPorts(cfg *nexus.NexusConfig) {
	ports := make([]int, 0, cfg.Nexus.PortRangeSize+16)
	for p := cfg.Nexus.PortRangeBase; p < cfg.Nexus.PortRangeBase+cfg.Nexus.PortRangeSize; p++ {
		ports = append(ports, p)
	}
	// Well-known internal services.
	ports = append(ports, 11434, 11435, 9000, 8087, 8085, 8081, 8080, 6060)
	sre.InitTrustedPorts(ports)
}

// mustGenerateInternalToken returns a 32-byte hex token used by
// children to authenticate /internal/certify/* and /internal/chaos/*
// calls back to Nexus. Fatal if rand.Read fails (system entropy is
// genuinely broken at that point). Extracted from main(). [CC refactor]
func mustGenerateInternalToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("[NEXUS-FATAL] failed to generate internal token: %v", err)
	}
	return hex.EncodeToString(b)
}
