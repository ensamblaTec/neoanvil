// Command plugin-jira is the Atlassian Jira Cloud MCP plugin for neoanvil.
// PILAR XXIII / Épica 125.2.
//
// Wire format: newline-delimited JSON-RPC over stdio (MCP stdio transport).
// Auth: env vars (injected by Nexus PluginPool from ~/.neo/credentials.json
// via pkg/auth/vault.NewLookupWithContext):
//
//	JIRA_TOKEN              — API token from id.atlassian.com (required)
//	JIRA_EMAIL              — Atlassian account email (required)
//	JIRA_DOMAIN             — e.g. "acme.atlassian.net" (required)
//	JIRA_ACTIVE_SPACE       — active project key (optional context)
//	JIRA_ACTIVE_BOARD       — active board id (optional context)
//
// Tools exposed (Épica 125.4):
//
//	get_context : fetch ticket summary + status + description + last 3 comments
//
// Future (Épica 125.5):
//
//	transition  : move ticket between statuses with audit log entry
//
// See [docs/pilar-xxiii-plugin-architecture.md] for the architecture.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/auth"
	"github.com/ensamblatec/neoanvil/pkg/jira"
)

const (
	protocolVersion = "2024-11-05"
	pluginVersion   = "0.1.0"
)

// state holds the long-lived Jira client + bookkeeping.
//
// activeSpace/activeBoard are the FALLBACK values captured from env vars at
// boot — used only when contexts.json is missing or unreadable. The live
// values come from cachedContext (see currentSpace / currentBoard) which
// re-reads ~/.neo/contexts.json with a short TTL so `neo space use ...`
// changes propagate without restarting Nexus. [Épica 142.1]
type state struct {
	client      *jira.Client
	activeSpace string // env-var fallback only
	activeBoard string // env-var fallback only
	audit       *auth.AuditLog // mandatory audit trail for write operations (PILAR XXIII / 124.6)

	// Multi-tenant config (nil in legacy single-tenant mode).
	pluginCfg *PluginConfig
	pool      *clientPool    // per-tenant client cache with shared rate limiter
	ctx       context.Context // process-level context for rate limiter waits

	// Per-call context cache. mu guards against torn reads when multiple
	// tools/call requests race at the JSON-RPC layer.
	mu               sync.Mutex
	cachedContexts   *auth.ContextStore
	contextsLoadedAt time.Time

	// [ÉPICA 152.H] Local-only health counters consumed by the __health__
	// MCP action. Atomic so the Nexus health poll (every 30s) is lock-free
	// at <10ms target latency and never touches the upstream Jira API.
	startedAtUnix    int64
	lastDispatchUnix int64
	errorCount       int64
}

// callCtx carries per-request metadata extracted from _meta injected by Nexus.
// Populated at the JSON-RPC dispatch boundary before any action handler runs.
type callCtx struct {
	WorkspaceID string
	TraceID     string
}

// contextsCacheTTL bounds disk IO from contexts.json reads. 5s is small
// enough that operator changes via `neo space use` propagate within one
// human reaction time, and large enough to absorb burst tool/call traffic
// during a refactor (50 calls in 2s = 1 disk read, not 50). [142.1]
const contextsCacheTTL = 5 * time.Second

// currentContext returns the active ContextStore, refreshing from disk when
// the cache is past TTL. Always non-nil; falls back to an empty store on
// load failure so currentSpace/currentBoard can fall through to env vars.
func (s *state) currentContext() *auth.ContextStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedContexts != nil && time.Since(s.contextsLoadedAt) < contextsCacheTTL {
		return s.cachedContexts
	}
	store, err := auth.LoadContexts(auth.DefaultContextsPath())
	if err != nil {
		// Disk read failed (file missing, permission, JSON corrupt). Cache
		// an empty store with the current timestamp so we don't hammer the
		// disk on every subsequent call. The TTL still applies — next
		// retry happens contextsCacheTTL later.
		s.cachedContexts = &auth.ContextStore{}
		s.contextsLoadedAt = time.Now()
		return s.cachedContexts
	}
	s.cachedContexts = store
	s.contextsLoadedAt = time.Now()
	return store
}

// currentSpace returns the live active Jira space, falling back to the
// env-var captured at boot when contexts.json doesn't have an active for
// "jira". [142.1]
func (s *state) currentSpace() string {
	store := s.currentContext()
	if active := store.ActiveSpace("jira"); active != nil && active.SpaceID != "" {
		return active.SpaceID
	}
	return s.activeSpace
}

// currentBoard returns the live active Jira board id, falling back to the
// env-var. [142.1]
func (s *state) currentBoard() string {
	store := s.currentContext()
	if active := store.ActiveSpace("jira"); active != nil && active.BoardID != "" {
		return active.BoardID
	}
	return s.activeBoard
}

func main() {
	// 3.4.D — buildStateSafe wraps buildState with edge-case error
	// reporting so plugin boot failures get a consistent "plugin-jira
	// init: <root cause>" prefix in stderr.
	st, err := buildStateSafe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	atomic.StoreInt64(&st.startedAtUnix, time.Now().Unix())

	// 3.4.D — surface legacy config artifacts so the operator knows
	// to migrate them to ~/.neo/plugins/jira.json.
	checkLegacyDeprecation()

	// 3.4.B — boot-time connectivity check per configured api_key.
	// Logs OK/FAIL once per tenant; subsequent dispatch calls fast-path
	// past the in-process result cache (5min TTL inside checkConnectivity).
	runBootConnectivityChecks(st)

	// 3.4.C — graceful drain on SIGTERM/SIGINT. The handler closes the
	// drain context after waiting up to 5s for in-flight RPCs to
	// complete; the scanner loop exits when ctx is cancelled.
	drain := newShutdownDrain(5 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	installShutdownHandler(drain, cancel)
	st.ctx = ctx

	// 3.4.D follow-up — SIGHUP triggers per-tenant pool invalidation
	// so credential rotations + plugin manifest edits take effect
	// without a full plugin restart.
	if st.pool != nil {
		installPoolReloadHandler(st)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	enc := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var req map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-jira: bad json:", err)
			continue
		}
		// Wrap handle() in a closure so a panic in any action handler
		// still releases the drain counter — without `defer`, a crashed
		// handler would leak the track() and drain.waitOrTimeout would
		// stall the full 5s before forcing shutdown. [DS-AUDIT 3.4 Finding 4]
		resp := func() (resp map[string]any) {
			drain.track()
			defer drain.done()
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "plugin-jira: panic in handle: %v\n", r)
					resp = rpcErr(req["id"], -32603, "internal panic")
				}
			}()
			return st.handle(req)
		}()
		if resp == nil {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-jira: encode:", err)
			return
		}
	}
}

// runBootConnectivityChecks fires checkConnectivity for each configured
// project's api_key in single-tenant or multi-tenant mode. Errors are
// logged but non-fatal — Jira may be temporarily down at boot, and the
// per-call retry path will reattempt.
// [3.4.B]
func runBootConnectivityChecks(st *state) {
	if st.pluginCfg == nil {
		// Legacy single-tenant: client is already constructed. Skip
		// the explicit ping; the first real GetIssue surfaces failures.
		return
	}
	for projName, proj := range st.pluginCfg.Projects {
		key, ok := st.pluginCfg.APIKeys[proj.APIKeyRef]
		if !ok {
			continue
		}
		token, terr := resolveToken(key)
		if terr != nil {
			fmt.Fprintf(os.Stderr, "plugin-jira: connectivity skip %q: %v\n", projName, terr)
			continue
		}
		_ = checkConnectivity(projName+"/"+proj.APIKeyRef, key, token)
	}
}

// installPoolReloadHandler wires SIGHUP to clientPool.invalidateAll so
// operators can rotate credentials or edit the plugin manifest and have
// the next request use the fresh state without restarting the whole
// plugin process. [3.4.D]
func installPoolReloadHandler(st *state) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	go func() {
		for range sigCh {
			st.pool.invalidateAll()
			fmt.Fprintln(os.Stderr, "plugin-jira: SIGHUP — client pool invalidated, next request rebuilds")
		}
	}()
}

// buildState reads required env vars and constructs the Jira client +
// per-plugin audit log. The audit log is mandatory — failure to open it
// aborts plugin boot rather than silently shipping unaudited mutations.
func buildState() (*state, error) {
	// Dual-boot: try jira.json first, fall back to legacy env vars.
	cfg, err := loadPluginConfig(defaultConfigPath)
	if err == nil {
		fmt.Fprintf(os.Stderr, "plugin-jira: loaded config from %s (project=%s)\n", defaultConfigPath, cfg.ActiveProject)
		return buildStateFromConfig(cfg)
	}

	if !os.IsNotExist(err) {
		// jira.json exists but is invalid — try auto-migration from legacy
		fmt.Fprintf(os.Stderr, "plugin-jira: jira.json invalid (%v), attempting migration\n", err)
		migCfg, migErr := migrateToPluginConfig(defaultConfigPath)
		if migErr == nil {
			return buildStateFromConfig(migCfg)
		}
		fmt.Fprintf(os.Stderr, "plugin-jira: migration failed: %v, falling back to env vars\n", migErr)
	} else {
		fmt.Fprintf(os.Stderr, "plugin-jira: %s not found, using legacy env vars\n", defaultConfigPath)
	}

	return buildStateFromLegacy()
}

// pluginAuditPath returns ~/.neo/audit-jira.log (per-plugin file isolates
// hash chains across processes — Nexus + plugin-jira do NOT share an
// audit log because pkg/auth/audit uses sync.Mutex, not file locks).
func pluginAuditPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "audit-jira.log" // best-effort fallback in cwd
	}
	return filepath.Join(home, ".neo", "audit-jira.log")
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
		"serverInfo":      map[string]any{"name": "plugin-jira", "version": pluginVersion},
	})
}

// handleToolsList exposes a SINGLE jira macro-tool with an `action` field
// that dispatches to internal handlers. Same pattern as neo_memory /
// neo_radar / neo_cache in the core. Reduces tool inventory bloat.
func handleToolsList(id any) map[string]any {
	return ok(id, map[string]any{
		"tools": []map[string]any{
			{
				"name":        "jira",
				"description": "Atlassian Jira macro-tool. Actions: get_context (read ticket), transition (move status with audit), create_issue (Epic/Story/Bug/...), update_issue (PUT partial — backfill description/summary/labels/dates/assignee on existing tickets), link_issue (relates/blocks/etc.), attach_artifact (zip a local folder and upload as attachment), prepare_doc_pack (one-shot: read repo files, build README, render PNGs, attach — all local, no content passed through Claude context).",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": map[string]any{
							"type":        "string",
							"enum":        []string{"get_context", "transition", "create_issue", "update_issue", "link_issue", "attach_artifact", "prepare_doc_pack"},
							"description": "Operation to perform. Each action consumes a different subset of the fields below.",
						},
						"ticket_id":          map[string]any{"type": "string", "description": "[get_context, transition] Issue key (e.g. MCPI-1)."},
						"target_status":      map[string]any{"type": "string", "description": "[transition] Target status name (case-insensitive) or transition name."},
						"resolution_comment": map[string]any{"type": "string", "description": "[transition] Comment added atomically with the transition."},
						"project_key":        map[string]any{"type": "string", "description": "[create_issue] Project key (e.g. MCPI). Defaults to JIRA_ACTIVE_SPACE when empty."},
						"issue_type":         map[string]any{"type": "string", "description": "[create_issue] Epic | Story | Bug | Task | etc."},
						"summary":            map[string]any{"type": "string", "description": "[create_issue, prepare_doc_pack] Issue summary (title) for create, or README override for prepare_doc_pack."},
						"description":        map[string]any{"type": "string", "description": "[create_issue] Body in Markdown — converted to ADF (headings, bullets)."},
						"parent_key":         map[string]any{"type": "string", "description": "[create_issue] For Story: Epic key. Sets the parent hierarchy."},
						"assignee_email":     map[string]any{"type": "string", "description": "[create_issue] Optional assignee. Resolved to accountId via /user/search."},
						"reporter_email":     map[string]any{"type": "string", "description": "[create_issue] Optional reporter. Resolved to accountId."},
						"labels":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "[create_issue] Tags like architecture, jira, bug, feature."},
						"due_date":           map[string]any{"type": "string", "description": "[create_issue] End date YYYY-MM-DD."},
						"start_date":         map[string]any{"type": "string", "description": "[create_issue] Start date YYYY-MM-DD (custom field)."},
						"story_points":       map[string]any{"type": "number", "description": "[create_issue] Story points (Asana scale: 1, 2, 3, 5, 8, 13)."},
						"from_key":           map[string]any{"type": "string", "description": "[link_issue] Inward issue key."},
						"to_key":             map[string]any{"type": "string", "description": "[link_issue] Outward issue key."},
						"folder_path":        map[string]any{"type": "string", "description": "[attach_artifact] Local folder to zip and upload (default: ~/.neo/jira-docs/<ticket_id>)."},
						"auto_render":        map[string]any{"type": "boolean", "description": "[attach_artifact, prepare_doc_pack] When true, scan code/ and render code-snap PNGs to images/ before zipping. Idempotent. Default: false."},
						"repo_root":          map[string]any{"type": "string", "description": "[prepare_doc_pack] Absolute path to the git repo whose files to include. Required."},
						"files":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "[prepare_doc_pack] Repo-relative paths to include. Optional when commit_hash set."},
						"commit_hash":        map[string]any{"type": "string", "description": "[prepare_doc_pack] Auto-derive files + summary from `git show <hash>`. Operator passes only ticket_id + commit_hash + repo_root."},
						"commit_range":       map[string]any{"type": "string", "description": "[prepare_doc_pack] git log range, e.g. 'HEAD~10..HEAD'. Empty = full file history."},
						"exclude_paths":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "[prepare_doc_pack] Substrings to drop from the file list (auto-managed metadata, etc.). When omitted: applies sensible defaults (.neo/master_plan.md, .neo/master_done.md, .neo/technical_debt.md, go.sum, .gitignore). Pass empty array to opt out."},
						"auto_attach":        map[string]any{"type": "boolean", "description": "[prepare_doc_pack] When true, zip + upload after building. Default: false (build only)."},
						"link_type":          map[string]any{"type": "string", "description": "[link_issue] Relates | Blocks | Duplicates | Cloners | etc."},
					},
					"required": []string{"action"},
				},
			},
		},
	})
}

// dispatchWrapAudit calls the underlying handler and emits a
// single multi-tenant `tool_call` audit ledger entry per dispatch
// using the existing `auditMultiTenant` helper. Detailed action-
// specific audit entries (auditTransition, auditAttachment, etc.)
// continue to fire from each handler — this gives the lightweight
// cross-cutting log per call. [3.4.A]
func (s *state) dispatchWrapAudit(action string, args map[string]any, cc callCtx, resp map[string]any) {
	if s.audit == nil {
		return
	}
	issueKey, _ := args["ticket_id"].(string)
	result := "ok"
	if _, isErr := resp["error"]; isErr {
		result = "error"
	}
	projName := ""
	if s.pluginCfg != nil {
		projName = s.pluginCfg.ActiveProject
	}
	s.auditMultiTenant(cc, projName, action, issueKey, result)
}

func (s *state) handleToolsCall(id any, req map[string]any) map[string]any {
	params, _ := req["params"].(map[string]any)
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)

	// Extract _meta injected by Nexus (workspace_id, trace_id, idempotency_key).
	cc := extractCallCtx(params)

	if name != "jira" {
		return rpcErr(id, -32602, "unknown tool: "+name+" (this plugin exposes 'jira')")
	}
	action, _ := args["action"].(string)

	// [ÉPICA 152.H] __health__ short-circuits before bookkeeping. Local
	// liveness probe — never touches the Jira API, returns instantly.
	if action == "__health__" {
		return s.handleHealth(id)
	}

	atomic.StoreInt64(&s.lastDispatchUnix, time.Now().Unix())
	resp := s.dispatchAction(id, action, args, cc)
	if _, isErr := resp["error"]; isErr {
		atomic.AddInt64(&s.errorCount, 1)
	}
	// [3.4.A] Cross-cutting tool_call audit entry. Per-action helpers
	// (auditTransition, auditAttachment) keep firing for full details;
	// this adds the multi-tenant overlay (TenantID + Tool + result).
	s.dispatchWrapAudit(action, args, cc, resp)
	return resp
}

// dispatchAction routes a real action to its handler. Extracted from
// handleToolsCall so __health__ can short-circuit before the bookkeeping
// updates (last_dispatch_unix, error_count). [ÉPICA 152.H]
func (s *state) dispatchAction(id any, action string, args map[string]any, cc callCtx) map[string]any {
	switch action {
	case "get_context":
		return s.callGetContext(id, args, cc)
	case "transition":
		return s.callTransition(id, args, cc)
	case "create_issue":
		return s.callCreateIssue(id, args, cc)
	case "update_issue":
		return s.callUpdateIssue(id, args, cc)
	case "link_issue":
		return s.callLinkIssue(id, args, cc)
	case "attach_artifact":
		return s.callAttachArtifact(id, args, cc)
	case "prepare_doc_pack":
		return s.callPrepareDocPack(id, args, cc)
	case "":
		return rpcErr(id, -32602, "action is required (get_context | transition | create_issue | update_issue | link_issue | attach_artifact | prepare_doc_pack)")
	}
	return rpcErr(id, -32602, "unknown action: "+action)
}

// handleHealth returns the plugin's self-reported liveness snapshot. Local
// state only — never invokes the upstream Jira API. Schema (ÉPICA 152.H):
//
//	plugin_alive       — always true if this code runs
//	tools_registered   — tool names this plugin handles
//	uptime_seconds     — wall-clock since plugin started
//	last_dispatch_unix — Unix ts of last real tools/call (0 = never)
//	error_count        — cumulative error responses since boot
//	api_key_present    — does the plugin have credentials loaded
//	active_space       — current Jira project key (live, via contexts.json)
//	active_board       — current Jira board id (live, via contexts.json)
//
// Polled every 30s by Nexus's plugin manager. Zombies (process alive but
// tools_registered=[]) are detected by comparing against the initial set.
func (s *state) handleHealth(id any) map[string]any {
	started := atomic.LoadInt64(&s.startedAtUnix)
	uptime := int64(0)
	if started > 0 {
		uptime = time.Now().Unix() - started
	}
	return ok(id, map[string]any{
		"plugin_alive":       true,
		"tools_registered":   []string{"jira"},
		"uptime_seconds":     uptime,
		"last_dispatch_unix": atomic.LoadInt64(&s.lastDispatchUnix),
		"error_count":        atomic.LoadInt64(&s.errorCount),
		"api_key_present":    s.client != nil,
		"active_space":       s.currentSpace(),
		"active_board":       s.currentBoard(),
	})
}

func extractCallCtx(params map[string]any) callCtx {
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		return callCtx{}
	}
	wsID, _ := meta["workspace_id"].(string)
	traceID, _ := meta["trace_id"].(string)
	return callCtx{
		WorkspaceID: strings.TrimSpace(wsID),
		TraceID:     traceID,
	}
}

func (s *state) callGetContext(id any, args map[string]any, _ callCtx) map[string]any {
	ticketID, _ := args["ticket_id"].(string)
	ticketID = strings.TrimSpace(ticketID)
	// Reject anything that doesn't match the Jira issue-key shape so a
	// crafted input like "MCPI-1/../rest/api/3/serverInfo" can't bypass
	// issue-scoped routing in the upstream client. [DS-AUDIT 3.4 Finding 3]
	if err := validateTicketID(ticketID); err != nil {
		return rpcErr(id, -32602, err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue, err := s.client.GetIssue(ctx, ticketID)
	if err != nil {
		return rpcErr(id, -32603, fmt.Sprintf("get_context %s: %v", ticketID, err))
	}
	return ok(id, textContent(formatIssueMarkdown(issue, s.currentSpace(), s.currentBoard())))
}

// callTransition implements the jira/transition tool. Flow:
//  1. Validate inputs.
//  2. ListTransitions on the issue to discover available targets.
//  3. FindTransitionByStatus to map operator's status name → transition ID.
//  4. DoTransition with the resolution comment.
//  5. Mandatory audit log append (failure aborts the response — the
//     mutation is allowed but not silenced).
//  6. Return Markdown summary of the action.
func (s *state) callTransition(id any, args map[string]any, _ callCtx) map[string]any {
	ticketID, _ := args["ticket_id"].(string)
	targetStatus, _ := args["target_status"].(string)
	comment, _ := args["resolution_comment"].(string)

	ticketID = strings.TrimSpace(ticketID)
	targetStatus = strings.TrimSpace(targetStatus)
	if err := validateTicketID(ticketID); err != nil {
		return rpcErr(id, -32602, err.Error())
	}
	if targetStatus == "" {
		return rpcErr(id, -32602, "target_status is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	transitions, err := s.client.ListTransitions(ctx, ticketID)
	if err != nil {
		return rpcErr(id, -32603, fmt.Sprintf("list transitions %s: %v", ticketID, err))
	}
	target := jira.FindTransitionByStatus(transitions, targetStatus)
	if target == nil {
		return rpcErr(id, -32602, fmt.Sprintf(
			"no transition matches %q for %s. Available: %s",
			targetStatus, ticketID, summarizeTransitions(transitions)))
	}

	if err := s.client.DoTransition(ctx, ticketID, target.ID, comment); err != nil {
		return rpcErr(id, -32603, fmt.Sprintf("transition %s → %s: %v", ticketID, target.ToStatus, err))
	}

	if auditErr := s.auditTransition(ticketID, target, comment); auditErr != nil {
		// Mutation succeeded; audit failure is loud but does not roll
		// back the Jira-side change (idempotency would require GetIssue
		// pre-check + revert attempt — out of scope for MVP).
		fmt.Fprintf(os.Stderr, "plugin-jira: AUDIT FAILED for %s → %s: %v\n",
			ticketID, target.ToStatus, auditErr)
		return rpcErr(id, -32603, fmt.Sprintf(
			"transition succeeded but AUDIT LOG WRITE FAILED: %v — investigate ~/.neo/audit-jira.log", auditErr))
	}
	return ok(id, textContent(formatTransitionResult(ticketID, target, comment)))
}

// callCreateIssue creates an issue (Epic/Story/Bug/...) and emits an audit
// log entry. Resolves assignee/reporter emails to accountId via the
// /user/search endpoint. Defaults project_key to JIRA_ACTIVE_SPACE when
// the caller omits it.
func (s *state) callCreateIssue(id any, args map[string]any, _ callCtx) map[string]any {
	in := buildCreateInput(args, s.currentSpace())
	if err := validateCreateInput(in); err != nil {
		return rpcErr(id, -32602, err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if email, _ := args["assignee_email"].(string); strings.TrimSpace(email) != "" {
		acc, err := s.client.LookupAccountByEmail(ctx, email)
		if err != nil {
			return rpcErr(id, -32603, fmt.Sprintf("lookup assignee %s: %v", email, err))
		}
		in.AssigneeAccountID = acc
	}
	if email, _ := args["reporter_email"].(string); strings.TrimSpace(email) != "" {
		acc, err := s.client.LookupAccountByEmail(ctx, email)
		if err != nil {
			return rpcErr(id, -32603, fmt.Sprintf("lookup reporter %s: %v", email, err))
		}
		in.ReporterAccountID = acc
	}

	out, err := s.client.CreateIssue(ctx, in)
	if err != nil {
		return rpcErr(id, -32603, fmt.Sprintf("create_issue: %v", err))
	}

	if auditErr := s.auditCreate(in, out); auditErr != nil {
		fmt.Fprintf(os.Stderr, "plugin-jira: AUDIT FAILED for create %s: %v\n", out.Key, auditErr)
		return rpcErr(id, -32603, fmt.Sprintf("issue %s created but AUDIT LOG WRITE FAILED: %v", out.Key, auditErr))
	}
	return ok(id, textContent(formatCreateResult(in, out)))
}

// callUpdateIssue patches an existing issue. Empty fields are NOT touched
// (PATCH semantics) — caller controls which subset to update. Resolves
// assignee_email → accountId via /user/search same as create_issue.
// [Épica 140 — fixes MCPI-57/58 backfill, MCPI-59 sync at Done]
func (s *state) callUpdateIssue(id any, args map[string]any, _ callCtx) map[string]any {
	ticketID, _ := args["ticket_id"].(string)
	ticketID = strings.TrimSpace(ticketID)
	if err := validateTicketID(ticketID); err != nil {
		return rpcErr(id, -32602, err.Error())
	}

	in := buildUpdateInput(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if email, _ := args["assignee_email"].(string); strings.TrimSpace(email) != "" {
		acc, err := s.client.LookupAccountByEmail(ctx, email)
		if err != nil {
			return rpcErr(id, -32603, fmt.Sprintf("lookup assignee %s: %v", email, err))
		}
		in.AssigneeAccountID = acc
	}

	if err := s.client.UpdateIssue(ctx, ticketID, in); err != nil {
		return rpcErr(id, -32603, fmt.Sprintf("update_issue %s: %v", ticketID, err))
	}

	if auditErr := s.auditUpdate(ticketID, in); auditErr != nil {
		fmt.Fprintf(os.Stderr, "plugin-jira: AUDIT FAILED for update %s: %v\n", ticketID, auditErr)
		return rpcErr(id, -32603, fmt.Sprintf("issue %s updated but AUDIT LOG WRITE FAILED: %v", ticketID, auditErr))
	}
	return ok(id, textContent(formatUpdateResult(ticketID, in)))
}

// buildUpdateInput maps tool arguments to jira.UpdateIssueInput. Same arg
// names as create_issue where applicable. Labels semantics: missing arg
// → nil (skip); arg present → replace.
func buildUpdateInput(args map[string]any) jira.UpdateIssueInput {
	in := jira.UpdateIssueInput{
		Summary:     strFromArgs(args, "summary"),
		Description: strFromArgs(args, "description"),
		DueDate:     strFromArgs(args, "due_date"),
		StartDate:   strFromArgs(args, "start_date"),
	}
	// Labels: distinguish "not in args" (nil = skip) from "in args but empty"
	// (empty slice = clear all). The plugin-jira input is JSON via JSON-RPC,
	// so an absent key is map miss → ok=false; explicit empty array is ok=true len=0.
	if labelsRaw, ok := args["labels"].([]any); ok {
		labels := make([]string, 0, len(labelsRaw))
		for _, l := range labelsRaw {
			if s, ok := l.(string); ok && strings.TrimSpace(s) != "" {
				labels = append(labels, s)
			}
		}
		in.Labels = labels
	}
	return in
}

// formatUpdateResult builds the Markdown summary returned to the agent.
func formatUpdateResult(ticketID string, in jira.UpdateIssueInput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "✓ Updated `%s`\n\n", ticketID)
	if in.Summary != "" {
		fmt.Fprintf(&sb, "- summary: `%s`\n", in.Summary)
	}
	if in.Description != "" {
		fmt.Fprintf(&sb, "- description: %d chars (Markdown→ADF)\n", len(in.Description))
	}
	if in.Labels != nil {
		fmt.Fprintf(&sb, "- labels: %v (replaced)\n", in.Labels)
	}
	if in.AssigneeAccountID != "" {
		fmt.Fprintf(&sb, "- assignee: accountId=%s\n", in.AssigneeAccountID)
	}
	if in.StartDate != "" {
		fmt.Fprintf(&sb, "- start_date: %s\n", in.StartDate)
	}
	if in.DueDate != "" {
		fmt.Fprintf(&sb, "- due_date: %s\n", in.DueDate)
	}
	return sb.String()
}

// buildCreateInput maps tool arguments to the typed jira.CreateIssueInput.
// Numeric coercion is defensive — JSON-RPC sometimes delivers numbers as
// float64 or as JSON-encoded strings depending on the client.
func buildCreateInput(args map[string]any, defaultProject string) jira.CreateIssueInput {
	projectKey, _ := args["project_key"].(string)
	if strings.TrimSpace(projectKey) == "" {
		projectKey = defaultProject
	}
	in := jira.CreateIssueInput{
		ProjectKey:  projectKey,
		IssueType:   strFromArgs(args, "issue_type"),
		Summary:     strFromArgs(args, "summary"),
		Description: strFromArgs(args, "description"),
		ParentKey:   strFromArgs(args, "parent_key"),
		DueDate:     strFromArgs(args, "due_date"),
		StartDate:   strFromArgs(args, "start_date"),
	}
	if labelsRaw, ok := args["labels"].([]any); ok {
		for _, l := range labelsRaw {
			if s, ok := l.(string); ok && strings.TrimSpace(s) != "" {
				in.Labels = append(in.Labels, s)
			}
		}
	}
	if pts, ok := args["story_points"].(float64); ok {
		in.StoryPoints = pts
	}
	return in
}

func strFromArgs(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func validateCreateInput(in jira.CreateIssueInput) error {
	if in.ProjectKey == "" {
		return fmt.Errorf("project_key is required (set arg or JIRA_ACTIVE_SPACE)")
	}
	if in.IssueType == "" {
		return fmt.Errorf("issue_type is required (Epic | Story | Bug | Task)")
	}
	if in.Summary == "" {
		return fmt.Errorf("summary is required")
	}
	return nil
}

// callLinkIssue creates a relates/blocks/etc. link between two existing
// tickets. Distinct from create_issue ParentKey which is for Epic→Story
// hierarchy.
func (s *state) callLinkIssue(id any, args map[string]any, _ callCtx) map[string]any {
	from := strFromArgs(args, "from_key")
	to := strFromArgs(args, "to_key")
	linkType := strFromArgs(args, "link_type")
	if from == "" || to == "" || linkType == "" {
		return rpcErr(id, -32602, "from_key, to_key and link_type are required")
	}
	if err := validateTicketID(from); err != nil {
		return rpcErr(id, -32602, "from_key: "+err.Error())
	}
	if err := validateTicketID(to); err != nil {
		return rpcErr(id, -32602, "to_key: "+err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.client.LinkIssue(ctx, from, to, linkType); err != nil {
		return rpcErr(id, -32603, fmt.Sprintf("link_issue %s -[%s]-> %s: %v", from, linkType, to, err))
	}

	if auditErr := s.auditLink(from, to, linkType); auditErr != nil {
		fmt.Fprintf(os.Stderr, "plugin-jira: AUDIT FAILED for link %s->%s: %v\n", from, to, auditErr)
		return rpcErr(id, -32603, fmt.Sprintf("link created but AUDIT LOG WRITE FAILED: %v", auditErr))
	}
	return ok(id, textContent(fmt.Sprintf("✅ Linked %s -[%s]→ %s\n\n_Audit log appended to ~/.neo/audit-jira.log_", from, linkType, to)))
}

// callAttachArtifact zips a local folder and POST-s it to the issue's
// /attachments endpoint. Operator workflow:
//
//	1. mkdir -p ~/.neo/jira-docs/MCPI-7   (or any path)
//	2. drop README.md, code/, images/ inside
//	3. jira_jira(action:"attach_artifact", ticket_id:"MCPI-7")
//	4. plugin zips → uploads → audit logs
//
// folder_path defaults to ~/.neo/jira-docs/<ticket_id>. The folder must
// exist and contain at least one file.
func (s *state) callAttachArtifact(id any, args map[string]any, _ callCtx) map[string]any {
	ticketID := strFromArgs(args, "ticket_id")
	if err := validateTicketID(ticketID); err != nil {
		return rpcErr(id, -32602, err.Error())
	}
	// Anchor folder_path under ~/.neo/jira-docs/ to prevent the
	// documented exfiltration vector where a client requests
	// `folder_path: /etc/ssh` to upload host secrets to a Jira ticket.
	// [DS-AUDIT 3.4 Finding 2]
	folderPath, err := validateSafeFolderPath(strFromArgs(args, "folder_path"), ticketID)
	if err != nil {
		return rpcErr(id, -32602, err.Error())
	}

	autoRender, _ := args["auto_render"].(bool)
	// auto-render adds Chrome boot time per snippet — bump timeout when on.
	timeout := 60 * time.Second
	if autoRender {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	zipPath, err := s.client.AttachZipFolder(ctx, ticketID, folderPath, jira.AttachOptions{
		AutoRender: autoRender,
	})
	if err != nil {
		return rpcErr(id, -32603, fmt.Sprintf("attach_artifact %s: %v", ticketID, err))
	}

	if auditErr := s.auditAttachment(ticketID, folderPath, zipPath); auditErr != nil {
		fmt.Fprintf(os.Stderr, "plugin-jira: AUDIT FAILED for attach %s: %v\n", ticketID, auditErr)
		return rpcErr(id, -32603, fmt.Sprintf("attached but AUDIT LOG WRITE FAILED: %v", auditErr))
	}
	return ok(id, textContent(formatAttachResult(ticketID, folderPath, zipPath)))
}

// callPrepareDocPack is the high-level "do everything locally" action:
// reads repo files, derives descriptors, runs git log, writes README,
// optionally renders PNGs and uploads. Operator passes repo_root +
// files, plugin does the rest. Zero file content passes through the
// agent's context.
func (s *state) callPrepareDocPack(id any, args map[string]any, _ callCtx) map[string]any {
	in := buildPrepareDocPackInput(args)
	// Validate ticket_id and repo_root at the dispatch boundary —
	// repo_root must be an existing absolute directory (no traversal,
	// no /etc/passwd because not-a-dir is rejected).
	// [DS-AUDIT 3.4 Findings 2 + 3]
	if err := validateTicketID(in.TicketID); err != nil {
		return rpcErr(id, -32602, err.Error())
	}
	cleanRoot, err := validateSafeRepoRoot(in.RepoRoot)
	if err != nil {
		return rpcErr(id, -32602, err.Error())
	}
	in.RepoRoot = cleanRoot
	timeout := 120 * time.Second
	if in.AutoRender {
		timeout = 240 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := s.client.PrepareDocPack(ctx, in)
	if err != nil {
		return rpcErr(id, -32603, fmt.Sprintf("prepare_doc_pack %s: %v", in.TicketID, err))
	}

	if auditErr := s.auditPrepareDocPack(in, res); auditErr != nil {
		fmt.Fprintf(os.Stderr, "plugin-jira: AUDIT FAILED for prepare %s: %v\n", in.TicketID, auditErr)
	}
	return ok(id, textContent(formatPrepareResult(in, res)))
}

func buildPrepareDocPackInput(args map[string]any) jira.PrepareDocPackInput {
	in := jira.PrepareDocPackInput{
		TicketID:    strFromArgs(args, "ticket_id"),
		RepoRoot:    strFromArgs(args, "repo_root"),
		CommitHash:  strFromArgs(args, "commit_hash"),
		CommitRange: strFromArgs(args, "commit_range"),
		Summary:     strFromArgs(args, "summary"),
	}
	if filesRaw, ok := args["files"].([]any); ok {
		for _, f := range filesRaw {
			if s, ok := f.(string); ok && strings.TrimSpace(s) != "" {
				in.Files = append(in.Files, s)
			}
		}
	}
	if exRaw, ok := args["exclude_paths"].([]any); ok {
		// Empty array (length 0) is meaningful — operator opting out
		// of the defaults — so we set the field even when empty.
		in.ExcludePaths = make([]string, 0, len(exRaw))
		for _, e := range exRaw {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				in.ExcludePaths = append(in.ExcludePaths, s)
			}
		}
	}
	if v, ok := args["auto_render"].(bool); ok {
		in.AutoRender = v
	}
	if v, ok := args["auto_attach"].(bool); ok {
		in.AutoAttach = v
	}
	return in
}

func (s *state) auditPrepareDocPack(in jira.PrepareDocPackInput, res *jira.PrepareDocPackResult) error {
	if s.audit == nil {
		return errors.New("audit log not initialized")
	}
	_, err := s.audit.Append(auth.Event{
		Kind:     "jira_prepare_doc_pack",
		Actor:    "plugin-jira",
		Provider: "jira",
		Tool:     "jira/prepare_doc_pack",
		Details: map[string]any{
			"ticket_id":   in.TicketID,
			"repo_root":   in.RepoRoot,
			"file_count":  len(in.Files),
			"render":      in.AutoRender,
			"attach":      in.AutoAttach,
			"uploaded":    res.Uploaded,
			"zip_path":    res.ZipPath,
		},
	})
	return err
}

func formatPrepareResult(in jira.PrepareDocPackInput, res *jira.PrepareDocPackResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📦 Doc pack ready for **%s**\n\n", in.TicketID)
	fmt.Fprintf(&sb, "Folder: `%s`\n", res.FolderPath)
	fmt.Fprintf(&sb, "README: `%s`\n", res.ReadmePath)
	fmt.Fprintf(&sb, "Files (%d):\n", len(res.CodeFiles))
	for _, f := range res.CodeFiles {
		fmt.Fprintf(&sb, "  • code/%s\n", f)
	}
	if len(res.Renders) > 0 {
		fmt.Fprintf(&sb, "Renders: %d PNG(s) in images/\n", len(res.Renders))
	}
	if res.Uploaded {
		fmt.Fprintf(&sb, "\n✅ Attached as `%s` to Atlassian.\n", filepath.Base(res.ZipPath))
	} else if res.ZipPath != "" {
		fmt.Fprintf(&sb, "\nZip: `%s` (not uploaded — set auto_attach:true to upload)\n", res.ZipPath)
	} else {
		sb.WriteString("\n_Build only. Set auto_attach:true to zip + upload, or call attach_artifact separately._\n")
	}
	sb.WriteString("\n_Audit log appended to ~/.neo/audit-jira.log_")
	return sb.String()
}

func (s *state) auditAttachment(ticketID, folderPath, zipPath string) error {
	if s.audit == nil {
		return errors.New("audit log not initialized")
	}
	_, err := s.audit.Append(auth.Event{
		Kind:     "jira_attach_artifact",
		Actor:    "plugin-jira",
		Provider: "jira",
		Tool:     "jira/attach_artifact",
		Details: map[string]any{
			"ticket_id":    ticketID,
			"folder_path":  folderPath,
			"zip_path":     zipPath,
			"active_space": s.currentSpace(),
		},
	})
	return err
}

func formatAttachResult(ticketID, folderPath, zipPath string) string {
	return fmt.Sprintf("✅ Attached artifact to **%s**\n\nSource folder: `%s`\nZip created: `%s`\n\n_Audit log appended to ~/.neo/audit-jira.log_",
		ticketID, folderPath, zipPath)
}

func (s *state) auditCreate(in jira.CreateIssueInput, out *jira.CreateIssueOutput) error {
	if s.audit == nil {
		return errors.New("audit log not initialized")
	}
	_, err := s.audit.Append(auth.Event{
		Kind:     "jira_create_issue",
		Actor:    "plugin-jira",
		Provider: "jira",
		Tool:     "jira/create_issue",
		Details: map[string]any{
			"created_key":    out.Key,
			"project_key":    in.ProjectKey,
			"issue_type":     in.IssueType,
			"parent_key":     in.ParentKey,
			"summary":        in.Summary,
			"labels":         in.Labels,
			"story_points":   in.StoryPoints,
			"due_date":       in.DueDate,
			"start_date":     in.StartDate,
			"active_space":   s.currentSpace(),
			"active_board":   s.currentBoard(),
		},
	})
	return err
}

// auditUpdate records a jira_update_issue event with the field set that was
// actually mutated. Same shape as auditCreate but only includes fields that
// the caller passed (mirrors PATCH semantics in audit trail). [Épica 140.3]
func (s *state) auditUpdate(ticketID string, in jira.UpdateIssueInput) error {
	if s.audit == nil {
		return errors.New("audit log not initialized")
	}
	details := map[string]any{
		"ticket_id":    ticketID,
		"active_space": s.currentSpace(),
		"active_board": s.currentBoard(),
	}
	if in.Summary != "" {
		details["summary"] = in.Summary
	}
	if in.Description != "" {
		details["description_chars"] = len(in.Description)
	}
	if in.Labels != nil {
		details["labels"] = in.Labels
	}
	if in.AssigneeAccountID != "" {
		details["assignee_account_id"] = in.AssigneeAccountID
	}
	if in.DueDate != "" {
		details["due_date"] = in.DueDate
	}
	if in.StartDate != "" {
		details["start_date"] = in.StartDate
	}
	_, err := s.audit.Append(auth.Event{
		Kind:     "jira_update_issue",
		Actor:    "plugin-jira",
		Provider: "jira",
		Tool:     "jira/update_issue",
		Details:  details,
	})
	return err
}

func (s *state) auditLink(from, to, linkType string) error {
	if s.audit == nil {
		return errors.New("audit log not initialized")
	}
	_, err := s.audit.Append(auth.Event{
		Kind:     "jira_link_issue",
		Actor:    "plugin-jira",
		Provider: "jira",
		Tool:     "jira/link_issue",
		Details:  map[string]any{"from": from, "to": to, "link_type": linkType, "active_space": s.currentSpace()},
	})
	return err
}

func formatCreateResult(in jira.CreateIssueInput, out *jira.CreateIssueOutput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Created **%s** (%s) — %s\n\n", out.Key, in.IssueType, in.Summary)
	if in.ParentKey != "" {
		fmt.Fprintf(&sb, "Parent: %s\n", in.ParentKey)
	}
	if in.StoryPoints > 0 {
		fmt.Fprintf(&sb, "Story points: %.0f\n", in.StoryPoints)
	}
	if len(in.Labels) > 0 {
		fmt.Fprintf(&sb, "Labels: %s\n", strings.Join(in.Labels, ", "))
	}
	if in.DueDate != "" || in.StartDate != "" {
		fmt.Fprintf(&sb, "Dates: start=%s end=%s\n", in.StartDate, in.DueDate)
	}
	fmt.Fprintf(&sb, "\nURL: %s\n\n_Audit log appended to ~/.neo/audit-jira.log_", out.Self)
	return sb.String()
}

func (s *state) auditTransition(ticketID string, t *jira.Transition, comment string) error {
	if s.audit == nil {
		return errors.New("audit log not initialized")
	}
	_, err := s.audit.Append(auth.Event{
		Kind:     "jira_transition",
		Actor:    "plugin-jira",
		Provider: "jira",
		Tool:     "jira/transition",
		Details: map[string]any{
			"ticket_id":     ticketID,
			"to_status":     t.ToStatus,
			"transition_id": t.ID,
			"transition_name": t.Name,
			"has_comment":   strings.TrimSpace(comment) != "",
			"active_space":  s.currentSpace(),
			"active_board":  s.currentBoard(),
		},
	})
	return err
}

func summarizeTransitions(ts []jira.Transition) string {
	if len(ts) == 0 {
		return "(none — issue may be terminal in its workflow)"
	}
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, fmt.Sprintf("%q→%q", t.Name, t.ToStatus))
	}
	return strings.Join(names, ", ")
}

func formatTransitionResult(ticketID string, t *jira.Transition, comment string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ %s transitioned via %q → **%s**", ticketID, t.Name, t.ToStatus)
	if strings.TrimSpace(comment) != "" {
		fmt.Fprintf(&sb, "\n\nComment added:\n> %s", strings.TrimSpace(comment))
	}
	sb.WriteString("\n\n_Audit log appended to ~/.neo/audit-jira.log_")
	return sb.String()
}

// formatIssueMarkdown renders an Issue as clean Markdown for the LLM.
// Adds context preamble when active space/board are known.
func formatIssueMarkdown(issue *jira.Issue, space, board string) string {
	var sb strings.Builder
	if space != "" || board != "" {
		sb.WriteString("> _context: ")
		if space != "" {
			fmt.Fprintf(&sb, "space=%s", space)
		}
		if board != "" {
			if space != "" {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "board=%s", board)
		}
		sb.WriteString("_\n\n")
	}
	fmt.Fprintf(&sb, "## %s — %s\n", issue.Key, issue.Summary)
	fmt.Fprintf(&sb, "**Status:** %s\n\n", issue.Status)
	if issue.Description != "" {
		sb.WriteString("### Description\n")
		sb.WriteString(issue.Description)
		sb.WriteString("\n\n")
	}
	if len(issue.Comments) > 0 {
		fmt.Fprintf(&sb, "### Last %d comments\n", len(issue.Comments))
		for _, c := range issue.Comments {
			fmt.Fprintf(&sb, "- **%s** _(%s)_: %s\n", c.Author, c.Created, strings.TrimSpace(c.Body))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
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
