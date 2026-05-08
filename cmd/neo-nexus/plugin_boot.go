// cmd/neo-nexus/plugin_boot.go — boot wiring for subprocess MCP plugins.
// PILAR XXIII / Épica 125+ integration debt resolved.
//
// Reads ~/.neo/plugins.yaml manifest, spawns each enabled plugin via
// nexus.PluginPool, drives MCP handshake (initialize + tools/list), and
// exposes status via /api/v1/plugins for operator inspection.
//
// Auth + active space context are injected as env vars at spawn time
// using auth.NewMultiProviderLookupWithContext: credentials from
// ~/.neo/credentials.json (or keyring) + active space from
// ~/.neo/contexts.json. Convention: <PROVIDER>_<FIELD>.
//
// Limitation (follow-up): Plugin tools are aggregated in memory and
// surfaced via /api/v1/plugins, but NOT yet merged into the MCP
// tools/list response served to Claude (that requires reverse-proxy
// interception — separate epic).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/auth"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/plugin"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

const pluginHandshakeTimeout = 5 * time.Second

// idempotencyTTL is how long a cached plugin result is held for dedup. [P-IDEM]
const idempotencyTTL = 5 * time.Minute

// idempotencyEntry caches a single tool result for dedup replay.
type idempotencyEntry struct {
	result    json.RawMessage
	expiresAt time.Time
}

// idempotencyStore is a thread-safe TTL cache keyed by deterministic hash of
// (pluginName, toolName, args). Used to short-circuit retried identical calls. [P-IDEM]
type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]idempotencyEntry
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{entries: make(map[string]idempotencyEntry)}
}

func (s *idempotencyStore) get(key string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || time.Now().After(e.expiresAt) {
		delete(s.entries, key)
		return nil, false
	}
	return e.result, true
}

func (s *idempotencyStore) set(key string, result json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.entries { // lazy eviction on write
		if now.After(v.expiresAt) {
			delete(s.entries, k)
		}
	}
	s.entries[key] = idempotencyEntry{result: result, expiresAt: now.Add(idempotencyTTL)}
}

// pluginRuntime holds the live state of subprocess plugins managed by
// Nexus at boot. Read-only after initialization; protected by Mutex
// for concurrent reads from the /api/v1/plugins handler.
type pluginRuntime struct {
	mu               sync.RWMutex
	pool             *nexus.PluginPool
	conns            []plugin.Connected
	tools            []plugin.NamespacedTool
	errors           map[string]error            // plugin name → boot/handshake error
	manifest         *plugin.Manifest
	policy           *sre.PolicyEngine            // [P-POLICY] semantic firewall for plugin tool calls
	idem             *idempotencyStore            // [P-IDEM] dedup cache for idempotent tool dispatch
	watchdogCancels  map[string]context.CancelFunc // [P-QUOTA] per-plugin memory watchdog cancel
	// [ÉPICA 152.C] Per-plugin health snapshot populated by background
	// goroutine calling __health__ every healthPollInterval. Zombie
	// detection consumes this — process alive but tools_registered=[]
	// or stale last_dispatch indicates the plugin is dead but its
	// stdio loop hasn't crashed. Map key: plugin name.
	health map[string]pluginHealthSnapshot
	// [ÉPICA 152.D] Zombie auto-restart tracking. zombieSince records when a
	// plugin first entered a zombie state; cleared on successful restart or
	// when the plugin recovers on its own. zombieRestarts counts restarts in
	// the current hour window; reset when the window rolls over.
	// zombiePending prevents concurrent goroutines (two poll cycles overlapping)
	// from both restarting the same plugin simultaneously — set under rt.mu before
	// releasing the lock, cleared after startAndHandshake completes.
	zombieSince    map[string]time.Time
	zombieRestarts map[string]int
	zombieHourTick map[string]time.Time // start of the current 1-hour rate-limit window
	zombiePending  map[string]bool      // restart in-flight guard
	// [376.A] Async task store for background plugin calls.
	asyncStore        *AsyncTaskStore
	asyncDoneCallback func(taskID string, task *AsyncTask)
}

// pluginHealthSnapshot is the parsed response from a plugin's
// __health__ MCP action. [ÉPICA 152.C / 152.H]
type pluginHealthSnapshot struct {
	Alive            bool     `json:"alive"`
	ToolsRegistered  []string `json:"tools_registered"`
	UptimeSeconds    int64    `json:"uptime_seconds"`
	LastDispatchUnix int64    `json:"last_dispatch_unix"`
	ErrorCount       int64    `json:"error_count"`
	APIKeyPresent    bool     `json:"api_key_present"`
	// PolledAtUnix records when neo-nexus last received this snapshot.
	// Stale snapshots (poll older than 2× healthPollInterval) imply
	// the polling loop itself broke or the plugin's stdio loop hung.
	PolledAtUnix int64 `json:"polled_at_unix"`
	// PollErr is non-empty when the most recent poll failed (timeout,
	// JSON parse, missing fields). When set, BRIEFING surfaces a
	// "zombie" marker for this plugin.
	PollErr string `json:"poll_err,omitempty"`
}

// healthPollInterval bounds how often runHealthMonitor calls each
// plugin's __health__ action. 30s balances responsiveness (operator
// notices zombies quickly) against load (each poll is one MCP round-trip).
const healthPollInterval = 30 * time.Second

// bootPluginPool initializes the subprocess plugin pool. Returns nil
// (no-op) when cfg.Nexus.Plugins.Enabled is false. On error logging and
// returning a partial runtime — operator can inspect via /api/v1/plugins
// to diagnose.
//
// The pool itself is ALWAYS created when plugins.enabled is true, even
// when the initial manifest is empty or missing. This keeps SIGHUP reload
// viable from the empty state — operator can drop a plugins.yaml in
// place and `kill -HUP` without restarting Nexus.
func bootPluginPool(ctx context.Context, cfg *nexus.NexusConfig) *pluginRuntime {
	if !cfg.Nexus.Plugins.Enabled {
		log.Printf("[NEXUS-PLUGINS] disabled (set nexus.plugins.enabled=true to activate)")
		return nil
	}
	manifest, err := plugin.LoadManifest()
	if err != nil {
		log.Printf("[NEXUS-PLUGINS-ERROR] load manifest: %v", err)
		// Fall through to create the pool anyway — operator can fix the
		// manifest and SIGHUP without rebooting Nexus.
		manifest = &plugin.Manifest{ManifestVersion: plugin.CurrentManifestVersion}
	}

	backend := auth.NewFileBackend("")
	contextStore, ctxErr := auth.LoadContexts(auth.DefaultContextsPath())
	if ctxErr != nil {
		log.Printf("[NEXUS-PLUGINS-WARN] load contexts: %v (no active-space env)", ctxErr)
		contextStore = nil
	}
	vault := auth.NewMultiProviderLookupWithContext(backend, contextStore)

	asyncDBPath := filepath.Join(cfg.Nexus.Logs.Dir, "..", "db", "nexus_async.db")
	asyncStore, asyncErr := NewAsyncTaskStore(asyncDBPath)
	if asyncErr != nil {
		log.Printf("[NEXUS-PLUGINS-WARN] async store failed: %v (background dispatch disabled)", asyncErr)
	} else {
		asyncStore.StartReaper(ctx, time.Hour)
	}

	rt := &pluginRuntime{
		pool:            nexus.NewPluginPool(vault, cfg.Nexus.Logs.Dir),
		errors:          make(map[string]error),
		manifest:        manifest,
		policy:          sre.NewPolicyEngine(config.DefaultSentinelConfig()), // [MCPI-46 bugfix] zero SentinelConfig fired goroutine_guard at any goroutine count > 0
		idem:            newIdempotencyStore(),
		watchdogCancels: make(map[string]context.CancelFunc),
		health:          map[string]pluginHealthSnapshot{},
		zombieSince:     map[string]time.Time{},
		zombieRestarts:  map[string]int{},
		zombieHourTick:  map[string]time.Time{},
		zombiePending:   map[string]bool{},
		asyncStore:      asyncStore,
	}

	enabled := manifest.EnabledPlugins()
	if len(enabled) == 0 {
		log.Printf("[NEXUS-PLUGINS] manifest has no enabled plugins (boot — drop entries and SIGHUP to load)")
		return rt
	}

	for _, spec := range enabled {
		startAndHandshake(ctx, rt, spec)
	}

	log.Printf("[NEXUS-PLUGINS] booted %d plugin(s); %d tool(s) aggregated; %d error(s)",
		len(rt.conns), len(rt.tools), len(rt.errors))

	// [ÉPICA 152.C] Background health monitor — polls each connected
	// plugin's __health__ action every 30s. Provides zombie detection
	// data to /api/v1/plugins + BRIEFING. Cancels cleanly on ctx.Done.
	go runPluginHealthMonitor(ctx, rt)
	return rt
}

// startAndHandshake spawns one plugin, drives MCP handshake + tools/list,
// and appends the result to the runtime. Errors are captured per-plugin
// so a single bad plugin does not block the others.
//
// Thread-safe: rt.mu.Lock() guards every write to rt.errors / rt.conns /
// rt.tools. Safe to call concurrently with the read-locked routing
// helpers (interceptPluginTools, detectPluginToolCall) and with the
// HTTP /api/v1/plugins handler.
func startAndHandshake(ctx context.Context, rt *pluginRuntime, spec *plugin.PluginSpec) {
	// [ÉPICA 152.B] Lifecycle log scaffolding. Each event below tracks a
	// transition through plugin boot. When a plugin reaches handshake_start
	// but never dispatcher_ready (process alive + tools dead), the log
	// trail makes the failure mode trivially attributable to the right
	// stage. Today's episode of "deepseek_call returns context-canceled
	// despite plugins:1 active" was masked by missing emission.
	log.Printf("[PLUGIN-LIFECYCLE] %s: plugin_spawn_start", spec.Name)
	proc, err := rt.pool.Start(spec)
	if err != nil {
		rt.recordError(spec.Name, fmt.Errorf("spawn: %w", err))
		log.Printf("[NEXUS-PLUGINS-ERROR] %s: spawn failed: %v", spec.Name, err)
		log.Printf("[PLUGIN-LIFECYCLE] %s: plugin_spawn_failed err=%v", spec.Name, err)
		return
	}
	log.Printf("[PLUGIN-LIFECYCLE] %s: plugin_spawn_ok pid=%d", spec.Name, proc.PID)

	client := plugin.NewClient(proc.Stdin, proc.Stdout)
	hsCtx, cancel := context.WithTimeout(ctx, pluginHandshakeTimeout)
	defer cancel()

	log.Printf("[PLUGIN-LIFECYCLE] %s: handshake_start timeout=%s", spec.Name, pluginHandshakeTimeout)
	if err := client.Initialize(hsCtx); err != nil {
		rt.recordError(spec.Name, fmt.Errorf("handshake: %w", err))
		log.Printf("[NEXUS-PLUGINS-ERROR] %s: handshake failed: %v", spec.Name, err)
		log.Printf("[PLUGIN-LIFECYCLE] %s: handshake_failed err=%v", spec.Name, err)
		_ = rt.pool.Stop(spec.Name)
		return
	}
	log.Printf("[PLUGIN-LIFECYCLE] %s: handshake_ok protocol=initialized", spec.Name)
	tools, err := client.ListTools(hsCtx)
	if err != nil {
		rt.recordError(spec.Name, fmt.Errorf("tools/list: %w", err))
		log.Printf("[NEXUS-PLUGINS-ERROR] %s: tools/list failed: %v", spec.Name, err)
		log.Printf("[PLUGIN-LIFECYCLE] %s: tools_list_failed err=%v", spec.Name, err)
		_ = rt.pool.Stop(spec.Name)
		return
	}
	log.Printf("[PLUGIN-LIFECYCLE] %s: plugin_tools_received count=%d", spec.Name, len(tools))

	// [P-QUOTA] Apply CPU + memory resource limits post-spawn.
	if spec.MaxCPUPercent > 0 {
		if err := sre.SetCPULimit(proc.PID, spec.MaxCPUPercent); err != nil {
			log.Printf("[NEXUS-PLUGINS-WARN] %s: cpu limit: %v (continuing without limit)", spec.Name, err)
		}
	}
	if spec.MaxMemoryMB > 0 {
		if err := sre.SetMemoryLimitCgroup(proc.PID, spec.MaxMemoryMB); err != nil {
			log.Printf("[NEXUS-PLUGINS-WARN] %s: memory cgroup: %v (watchdog active as fallback)", spec.Name, err)
		}
		wCtx, wCancel := context.WithCancel(context.Background())
		rt.mu.Lock()
		rt.watchdogCancels[spec.Name] = wCancel
		rt.mu.Unlock()
		sre.WatchProcessMemory(wCtx, proc.PID, spec.MaxMemoryMB, func() {
			log.Printf("[NEXUS-PLUGIN-OOM] %s exceeded %dMB RSS — force stopping", spec.Name, spec.MaxMemoryMB)
			_ = rt.pool.Stop(spec.Name)
			rt.mu.Lock()
			rt.removeConnAndToolsLocked(spec.Name)
			delete(rt.errors, spec.Name)
			delete(rt.watchdogCancels, spec.Name)
			rt.mu.Unlock()
		})
	}

	rt.mu.Lock()
	delete(rt.errors, spec.Name) // clear stale errors on success
	toolName := ""
	if len(tools) > 0 {
		toolName = tools[0].Name
	}
	rt.conns = append(rt.conns, plugin.Connected{
		Name:              spec.Name,
		NamespacePrefix:   spec.NamespacePrefix,
		ToolName:          toolName,
		Client:            client,
		AllowedWorkspaces: spec.AllowedWorkspaces,
	})
	for _, t := range tools {
		rt.tools = append(rt.tools, plugin.NamespacedTool{
			PluginName:      spec.Name,
			NamespacePrefix: spec.NamespacePrefix,
			Tool:            t,
		})
	}
	rt.mu.Unlock()

	// [ÉPICA 152.E] Pre-flight self-test. Invoke __health__ once
	// post-handshake. If the plugin doesn't respond OR returns
	// plugin_alive=false / api_key_present=false (when expected),
	// remove the plugin from rt.tools so it doesn't appear in
	// tools/list with a non-functional dispatcher. Operator sees the
	// failure attributed to the plugin via [PLUGIN-LIFECYCLE]
	// self_test_failed and the plugin shows in /api/v1/plugins with
	// errors map populated.
	if err := pluginSelfTest(ctx, rt, spec); err != nil {
		log.Printf("[PLUGIN-LIFECYCLE] %s: self_test_failed err=%v — removing from tools/list", spec.Name, err)
		rt.mu.Lock()
		rt.removeConnAndToolsLocked(spec.Name)
		if rt.errors == nil {
			rt.errors = map[string]error{}
		}
		rt.errors[spec.Name] = fmt.Errorf("self_test: %w", err)
		rt.mu.Unlock()
		_ = rt.pool.Stop(spec.Name)
		return
	}

	log.Printf("[NEXUS-PLUGINS] %s ready: %d tool(s) cpu_limit=%d%% mem_limit=%dMB",
		spec.Name, len(tools), spec.MaxCPUPercent, spec.MaxMemoryMB)
	log.Printf("[PLUGIN-LIFECYCLE] %s: plugin_dispatcher_ready tools=%d", spec.Name, len(tools))
}

// pluginSelfTest invokes __health__ on the just-handshaken plugin and
// validates the response shape. Mirrors the runtime health monitor's
// poll but runs synchronously during handshake — failures cause the
// plugin to be removed from rt.tools BEFORE it's exposed in tools/list.
//
// Tolerant of plugins that don't implement __health__ (older / third-
// party): if the call returns "unknown action" we accept the plugin
// (best-effort backwards compat). Hard failures (timeout, decode
// error, plugin_alive=false) reject the plugin.
//
// [ÉPICA 152.E]
func pluginSelfTest(ctx context.Context, rt *pluginRuntime, spec *plugin.PluginSpec) error {
	rt.mu.RLock()
	var conn *plugin.Connected
	for i := range rt.conns {
		if rt.conns[i].Name == spec.Name {
			conn = &rt.conns[i]
			break
		}
	}
	rt.mu.RUnlock()
	if conn == nil {
		return fmt.Errorf("connection not found post-handshake")
	}
	testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	selfTestTool := conn.ToolName
	if selfTestTool == "" {
		selfTestTool = "call"
	}
	raw, err := conn.Client.CallTool(testCtx, selfTestTool, map[string]any{"action": "__health__"})
	if err != nil {
		// Tolerate "unknown action" — plugin doesn't implement __health__.
		// Hard errors (timeout, broken pipe) → reject.
		if strings.Contains(err.Error(), "unknown action") || strings.Contains(err.Error(), "unknown tool") {
			log.Printf("[PLUGIN-LIFECYCLE] %s: self_test_skipped reason=plugin lacks __health__ (tolerated)", spec.Name)
			return nil
		}
		return fmt.Errorf("call: %w", err)
	}
	var parsed struct {
		PluginAlive *bool `json:"plugin_alive"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if parsed.PluginAlive == nil {
		return fmt.Errorf("missing plugin_alive field in __health__ response")
	}
	if !*parsed.PluginAlive {
		return fmt.Errorf("plugin reports plugin_alive=false")
	}
	log.Printf("[PLUGIN-LIFECYCLE] %s: self_test_ok plugin_alive=true", spec.Name)
	return nil
}

// recordError stores a per-plugin error under the runtime mutex.
func (rt *pluginRuntime) recordError(name string, err error) {
	rt.mu.Lock()
	rt.errors[name] = err
	rt.mu.Unlock()
}

// removeConnAndToolsLocked drops a plugin's tools and connection from the
// aggregated state. Caller MUST hold rt.mu.Lock().
func (rt *pluginRuntime) removeConnAndToolsLocked(name string) {
	out := rt.conns[:0]
	for _, c := range rt.conns {
		if c.Name != name {
			out = append(out, c)
		}
	}
	rt.conns = out

	outTools := rt.tools[:0]
	for _, t := range rt.tools {
		if t.PluginName != name {
			outTools = append(outTools, t)
		}
	}
	rt.tools = outTools
}

// reload re-reads the manifest, diffs against the current runtime, stops
// removed/changed plugins, spawns added/changed plugins, and updates the
// aggregated tool list. Called from the SIGHUP handler.
//
// Concurrency: tool routing reads rt.mu.RLock() per request; this method
// briefly RW-locks during state mutations. In-flight tool calls hitting
// a removed plugin will fail (the connection is closed); calls hitting a
// changed plugin race with restart and may fail once before the new
// connection is registered.
func (rt *pluginRuntime) reload(ctx context.Context) error {
	if rt == nil || rt.pool == nil {
		return errors.New("plugin runtime not initialized")
	}

	newManifest, err := plugin.LoadManifest()
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	newSpecs := newManifest.EnabledPlugins()

	rt.mu.RLock()
	var oldSpecs []*plugin.PluginSpec
	if rt.manifest != nil {
		oldSpecs = rt.manifest.EnabledPlugins()
	}
	rt.mu.RUnlock()

	added, removed, changed := diffManifestSpecs(oldSpecs, newSpecs)
	log.Printf("[NEXUS-PLUGINS-RELOAD] manifest diff: +%d new, -%d removed, ~%d changed",
		len(added), len(removed), len(changed))

	for _, s := range removed {
		stopAndForget(rt, s)
	}
	for _, s := range changed {
		stopAndForget(rt, s)
	}
	for _, s := range added {
		startAndHandshake(ctx, rt, s)
	}
	for _, s := range changed {
		startAndHandshake(ctx, rt, s)
	}

	rt.mu.Lock()
	rt.manifest = newManifest
	active := len(rt.conns)
	totalTools := len(rt.tools)
	rt.mu.Unlock()

	log.Printf("[NEXUS-PLUGINS-RELOAD] complete: %d active, %d tool(s) aggregated", active, totalTools)
	return nil
}

// stopAndForget halts a plugin and clears its conn + tools from rt.
func stopAndForget(rt *pluginRuntime, spec *plugin.PluginSpec) {
	if err := rt.pool.Stop(spec.Name); err != nil {
		log.Printf("[NEXUS-PLUGINS-RELOAD] stop %s: %v", spec.Name, err)
	}
	rt.mu.Lock()
	rt.removeConnAndToolsLocked(spec.Name)
	delete(rt.errors, spec.Name)
	if cancel, ok := rt.watchdogCancels[spec.Name]; ok {
		cancel()
		delete(rt.watchdogCancels, spec.Name)
	}
	rt.mu.Unlock()
}

// diffManifestSpecs returns (added, removed, changed) keyed by Name.
// "changed" means same name but different binary, args, env_from_vault,
// or namespace_prefix. Description / enabled flag changes do NOT trigger
// a restart (cosmetic).
func diffManifestSpecs(old, new []*plugin.PluginSpec) (added, removed, changed []*plugin.PluginSpec) {
	oldByName := make(map[string]*plugin.PluginSpec, len(old))
	for _, s := range old {
		if s != nil {
			oldByName[s.Name] = s
		}
	}
	newByName := make(map[string]*plugin.PluginSpec, len(new))
	for _, s := range new {
		if s != nil {
			newByName[s.Name] = s
		}
	}
	for name, ns := range newByName {
		os, exists := oldByName[name]
		if !exists {
			added = append(added, ns)
			continue
		}
		if !specEquivalent(os, ns) {
			changed = append(changed, ns)
		}
	}
	for name, os := range oldByName {
		if _, exists := newByName[name]; !exists {
			removed = append(removed, os)
		}
	}
	return added, removed, changed
}

// specEquivalent compares the runtime-relevant fields of two specs.
func specEquivalent(a, b *plugin.PluginSpec) bool {
	if a.Binary != b.Binary {
		return false
	}
	if a.NamespacePrefix != b.NamespacePrefix {
		return false
	}
	if !stringSliceEqual(a.Args, b.Args) {
		return false
	}
	if !stringSliceEqual(a.EnvFromVault, b.EnvFromVault) {
		return false
	}
	if a.MaxMemoryMB != b.MaxMemoryMB {
		return false
	}
	if a.MaxCPUPercent != b.MaxCPUPercent {
		return false
	}
	return true
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// shutdown stops all running plugins gracefully. Called from the main
// shutdown path. Safe to call on nil runtime.
func (rt *pluginRuntime) shutdown() {
	if rt == nil || rt.pool == nil {
		return
	}
	if err := rt.pool.StopAll(); err != nil {
		log.Printf("[NEXUS-PLUGINS] shutdown error: %v", err)
	}
}

// handlePluginsStatus returns the current plugin runtime state as JSON for
// operator inspection: which plugins are running, what tools they expose
// (namespaced), and any boot errors.
func handlePluginsStatus(rt *pluginRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if rt == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"enabled": false,
				"reason":  "nexus.plugins.enabled is false or no enabled plugins in manifest",
			})
			return
		}
		rt.mu.RLock()
		defer rt.mu.RUnlock()

		toolNames := make([]string, 0, len(rt.tools))
		for _, t := range rt.tools {
			toolNames = append(toolNames, t.PrefixedName())
		}
		errs := make(map[string]string, len(rt.errors))
		for name, err := range rt.errors {
			errs[name] = err.Error()
		}
		processes := make([]map[string]any, 0)
		if rt.pool != nil {
			for _, p := range rt.pool.List() {
				entry := map[string]any{
					"name":   p.Spec.Name,
					"pid":    p.PID,
					"status": p.Status,
				}
				// [ÉPICA 152.C] Embed health snapshot if available so
				// /api/v1/plugins is a one-stop diagnostic. BRIEFING
				// consumes this to flag zombies.
				if h, ok := rt.health[p.Spec.Name]; ok {
					entry["health"] = h
				}
				processes = append(processes, entry)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled":  true,
			"plugins":  processes,
			"tools":    toolNames,
			"errors":   errs,
			"manifest_version": manifestVersion(rt.manifest),
		})
	}
}

// runPluginHealthMonitor polls each connected plugin's __health__
// action every healthPollInterval and stores the snapshot in
// rt.health. Runs as a goroutine started by bootPluginPool; cancels
// cleanly on ctx.Done. [ÉPICA 152.C]
//
// Per-plugin failure isolation: a plugin that doesn't respond to
// __health__ within 5s gets PollErr set; other plugins keep polling.
func runPluginHealthMonitor(ctx context.Context, rt *pluginRuntime) {
	if rt == nil {
		return
	}
	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()
	// Run an immediate poll so the first BRIEFING after boot has data.
	pollAllPlugins(ctx, rt)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollAllPlugins(ctx, rt)
		}
	}
}

// pollAllPlugins iterates the connected plugins and invokes __health__
// on each. Errors per plugin update PollErr only — never propagate to
// other plugins. Held under rt.mu briefly to update the health map;
// the actual RPC happens outside the lock.
func pollAllPlugins(ctx context.Context, rt *pluginRuntime) {
	rt.mu.RLock()
	conns := make([]plugin.Connected, len(rt.conns))
	copy(conns, rt.conns)
	rt.mu.RUnlock()

	for _, c := range conns {
		snap := callPluginHealth(ctx, c)
		rt.mu.Lock()
		if rt.health == nil {
			rt.health = map[string]pluginHealthSnapshot{}
		}
		rt.health[c.Name] = snap
		rt.mu.Unlock()
	}
	// [ÉPICA 152.D] After each poll round, check for zombies that need restart.
	go checkZombiesAndRestart(ctx, rt)
}

// callPluginHealth issues the __health__ tools/call and parses the
// response. PollErr is populated on any failure path (timeout, decode
// error, missing fields) so BRIEFING / handlers see a non-empty marker
// without inspecting the boolean Alive field.
func callPluginHealth(ctx context.Context, c plugin.Connected) pluginHealthSnapshot {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	toolName := c.ToolName
	if toolName == "" {
		toolName = "call"
	}
	args := map[string]any{"action": "__health__"}
	raw, err := c.Client.CallTool(pollCtx, toolName, args)
	now := time.Now().Unix()
	if err != nil {
		return pluginHealthSnapshot{PolledAtUnix: now, PollErr: err.Error()}
	}
	// Client.CallTool returns the inner `result` field of the JSON-RPC
	// envelope. The deepseek plugin's __health__ handler returns:
	//   { "plugin_alive": bool, "tools_registered": [...], "uptime_seconds": ..., ... }
	// Field names match the plugin's exact JSON keys (NOT "alive" — was
	// a bug in the initial impl that flagged every healthy plugin as
	// "unexpected shape" zombie).
	var parsed struct {
		PluginAlive      *bool    `json:"plugin_alive"`
		ToolsRegistered  []string `json:"tools_registered"`
		UptimeSeconds    int64    `json:"uptime_seconds"`
		LastDispatchUnix int64    `json:"last_dispatch_unix"`
		ErrorCount       int64    `json:"error_count"`
		APIKeyPresent    bool     `json:"api_key_present"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return pluginHealthSnapshot{PolledAtUnix: now, PollErr: "decode: " + err.Error()}
	}
	if parsed.PluginAlive == nil {
		// Plugin response missing the contract field — likely an older
		// version without 152.H or a third-party plugin that doesn't
		// implement __health__. Flag zombie so operators see it.
		return pluginHealthSnapshot{PolledAtUnix: now, PollErr: "missing plugin_alive field"}
	}
	return pluginHealthSnapshot{
		Alive:            *parsed.PluginAlive,
		ToolsRegistered:  parsed.ToolsRegistered,
		UptimeSeconds:    parsed.UptimeSeconds,
		LastDispatchUnix: parsed.LastDispatchUnix,
		ErrorCount:       parsed.ErrorCount,
		APIKeyPresent:    parsed.APIKeyPresent,
		PolledAtUnix:     now,
	}
}

func manifestVersion(m *plugin.Manifest) int {
	if m == nil {
		return 0
	}
	return m.ManifestVersion
}

// zombieThreshold is how long a plugin must stay in a zombie state before
// the health monitor triggers an auto-restart. Two health polls at 30s each.
const zombieThreshold = 60 * time.Second

// maxZombieRestartsPerHour rate-limits restarts so a persistently-broken
// plugin does not spin endlessly. Matches the child watchdog default.
const maxZombieRestartsPerHour = 3

// isZombieHealth returns true when the snapshot indicates a zombie state:
// either the poll itself failed (PollErr set) or the plugin reports no tools.
func isZombieHealth(snap pluginHealthSnapshot) bool {
	return snap.PollErr != "" || len(snap.ToolsRegistered) == 0
}

// checkZombiesAndRestart inspects the health map for plugins that have been
// in a zombie state for longer than zombieThreshold and restarts them if
// AutoRestartOnZombie is enabled for their spec. Called after every health
// poll round. [ÉPICA 152.D]
//
// Concurrency: reads health map under rt.mu.RLock, writes zombie tracking
// maps under rt.mu.Lock. The actual stopAndForget + startAndHandshake
// calls happen outside the lock — same pattern as reload().
func checkZombiesAndRestart(ctx context.Context, rt *pluginRuntime) {
	if rt == nil {
		return
	}
	now := time.Now()

	// Snapshot health entries under RLock.
	rt.mu.RLock()
	type entry struct {
		name string
		snap pluginHealthSnapshot
	}
	entries := make([]entry, 0, len(rt.health))
	for name, snap := range rt.health {
		entries = append(entries, entry{name, snap})
	}
	rt.mu.RUnlock()

	for _, e := range entries {
		if !isZombieHealth(e.snap) {
			rt.mu.Lock()
			delete(rt.zombieSince, e.name)
			rt.mu.Unlock()
			continue
		}

		elapsed, past := zombieElapseCheck(rt, e.name, now)
		if !past {
			continue
		}

		// Find spec to check AutoRestartOnZombie.
		rt.mu.RLock()
		var spec *plugin.PluginSpec
		if rt.manifest != nil {
			for _, s := range rt.manifest.EnabledPlugins() {
				if s.Name == e.name {
					spec = s
					break
				}
			}
		}
		rt.mu.RUnlock()

		if spec == nil || !spec.WantsAutoRestart() {
			continue
		}

		attempt, ok := tryClaimZombieRestart(rt, e.name, now)
		if !ok {
			continue
		}

		log.Printf("[NEXUS-PLUGINS-ZOMBIE] %s: zombie for %s (poll_err=%q tools=%d), restarting (attempt %d/%d/h)",
			e.name, elapsed.Round(time.Second), e.snap.PollErr, len(e.snap.ToolsRegistered),
			attempt, maxZombieRestartsPerHour)

		stopAndForget(rt, spec)
		// Clear zombieSince so the threshold timer resets; keep zombiePending
		// until the full handshake completes — clearing it here would allow a
		// concurrent poll goroutine to trigger a second restart before this one
		// finishes. [DS-F1 fix]
		rt.mu.Lock()
		delete(rt.zombieSince, e.name)
		rt.mu.Unlock()

		startAndHandshake(ctx, rt, spec)

		// Only clear the in-flight guard after startAndHandshake returns so no
		// concurrent goroutine can observe a window where the plugin appears
		// restartable while we are mid-handshake.
		rt.mu.Lock()
		delete(rt.zombiePending, e.name)
		rt.mu.Unlock()
	}
}

// zombieElapseCheck records the first zombie observation for name and
// returns (elapsed, pastThreshold). On first observation elapsed==0 and
// pastThreshold==false (caller should skip and wait for next poll cycle).
func zombieElapseCheck(rt *pluginRuntime, name string, now time.Time) (time.Duration, bool) {
	rt.mu.Lock()
	since, tracked := rt.zombieSince[name]
	if !tracked {
		rt.zombieSince[name] = now
		rt.mu.Unlock()
		return 0, false
	}
	elapsed := now.Sub(since)
	rt.mu.Unlock()
	return elapsed, elapsed >= zombieThreshold
}

// tryClaimZombieRestart atomically checks the rate-limit and in-flight
// guard, then claims a restart slot. Returns (attemptNumber, true) on
// success; the caller MUST clear zombiePending[name] after the restart
// completes. Returns (0, false) when another goroutine is mid-restart or
// the hourly limit is reached (rate-limit case is logged here).
func tryClaimZombieRestart(rt *pluginRuntime, name string, now time.Time) (int, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.zombiePending[name] {
		return 0, false
	}
	windowStart, exists := rt.zombieHourTick[name]
	if !exists || now.Sub(windowStart) >= time.Hour {
		rt.zombieHourTick[name] = now
		rt.zombieRestarts[name] = 0
	}
	count := rt.zombieRestarts[name]
	if count >= maxZombieRestartsPerHour {
		log.Printf("[NEXUS-PLUGINS-ZOMBIE] %s: skipping restart — hit rate limit (%d/h)", name, maxZombieRestartsPerHour)
		return 0, false
	}
	rt.zombieRestarts[name]++
	rt.zombiePending[name] = true
	return count + 1, true
}
