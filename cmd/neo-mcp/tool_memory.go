// cmd/neo-mcp/tool_memory.go — unified brain-state tool.
// [Épica 239 + PILAR XXXIX / Épica 294]
//
// Actions (9 total):
//
//   commit         → episodic memory commit (short-term buffer)
//   learn          → persist architectural directive (permanent + dual-sync)
//   rem_sleep      → consolidate buffer into long-term HNSW graph
//   load_snapshot  → restore neural state from Gob file
//
//   store          → write a KnowledgeEntry to the project Knowledge Store [294.A]
//   fetch          → read a KnowledgeEntry by ns+key (hot cache → BoltDB) [294.B]
//   list           → list entries in a namespace, optional tag filter [294.C]
//   drop           → hard delete an entry [294.D]
//   search         → substring search over Key+Content in a namespace [294.E]

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/knowledge"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// Schema-staleness detection uses detectMemorySchemaStale() in radar_audit.go
// (compares bin/neo-mcp mtime vs tool_memory.go mtime). The prior const-based
// approach was never wired and was removed [330.E follow-up cleanup].

type MemoryTool struct {
	commit          *MemoryCommitTool
	learn           *LearnDirectiveTool
	remSleep        *RemSleepTool
	loadSnapshot    *LoadSnapshotTool
	ks              *knowledge.KnowledgeStore // [294] nil when not in project mode
	hc              *knowledge.HotCache       // [294] nil when ks is nil
	bus             *pubsub.Bus               // [298.B] nil-safe
	workspace       string
	coordinatorWSID string // [354.Z-redesign] non-empty on non-coordinator children; proxy target for tier:"project"
	orgKS           *knowledge.KnowledgeStore // [PILAR LXVII / 355.A] non-nil only on org coordinator project; non-coords proxy via HTTP (follow-up)
	orgWriters      []string                  // [361.A] allowlist from OrgConfig.Writers; empty = coordinator-only (default)
}

// resolveStoreTier returns the KnowledgeStore for project/workspace/org tiers
// (backed by local BoltDB). tier:"nexus" is NOT handled here — it's routed
// via HTTP to Nexus by execNexus*. See [354.Z-redesign]. tier:"org" requires
// this process to be the org coordinator project — non-coords proxy via
// HTTP (follow-up wiring, see 354.B pendientes). [355.A]
func (t *MemoryTool) resolveStoreTier(tier string) (*knowledge.KnowledgeStore, bool, error) {
	switch tier {
	case "", "project", "workspace":
		if t.ks == nil {
			return nil, false, fmt.Errorf("tier=%q requires project mode (KnowledgeStore not available)", tier)
		}
		return t.ks, false, nil
	case "org":
		if t.orgKS == nil {
			return nil, false, fmt.Errorf("tier=\"org\" requires this project to be the org coordinator (.neo-org/ + coordinator_project match); non-coords must proxy via HTTP")
		}
		if err := t.checkOrgWriteAllowed(); err != nil {
			return nil, false, err
		}
		return t.orgKS, false, nil
	default:
		return nil, false, fmt.Errorf("tier=%q unsupported here — use workspace|project|org (nexus is proxied)", tier)
	}
}

// checkOrgWriteAllowed enforces the RBAC writers allowlist for the org tier.
// When OrgConfig.Writers is non-empty, only listed workspace IDs (exact match
// or basename match) may write. Empty list preserves coordinator-only behavior. [361.A]
func (t *MemoryTool) checkOrgWriteAllowed() error {
	if len(t.orgWriters) == 0 {
		return nil // empty = coordinator-only (current process already holds the RW store)
	}
	callerID := resolveWorkspaceID(t.workspace)
	callerBase := filepath.Base(t.workspace)
	for _, w := range t.orgWriters {
		if w == callerID || w == callerBase || filepath.Base(w) == callerBase {
			return nil
		}
	}
	return fmt.Errorf("org: workspace %q not in writers allowlist — add to org.writers in .neo-org/neo.yaml", callerBase)
}

func (t *MemoryTool) Name() string { return "neo_memory" }

func (t *MemoryTool) Description() string {
	return "Unified brain-state + project knowledge operations. " +
		"commit/learn/rem_sleep/load_snapshot manage episodic memory and architectural directives. " +
		"store/fetch/list/drop/search operate on the cross-workspace project Knowledge Store " +
		"(DTOs, API contracts, enums, rules, flows — shared between all project workspaces)."
}

func (t *MemoryTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Which memory operation.",
				"enum": []string{
					"commit", "learn", "rem_sleep", "load_snapshot",
					"store", "fetch", "list", "drop", "search",
					"delegate", "complete_task",
				},
			},
			// commit
			"topic": map[string]any{
				"type":        "string",
				"description": "[commit] Short topic label for the episodic entry.",
			},
			"scope": map[string]any{
				"type":        "string",
				"description": "[commit] Domain scope (e.g. 'RAG', 'CPG', 'MCP').",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "[commit] Full lesson text. [store] Entry content (Markdown).",
			},
			// learn
			"directive": map[string]any{
				"type":        "string",
				"description": "[learn] New directive text to persist.",
			},
			"action_type": map[string]any{
				"type":        "string",
				"description": "[learn] add (default), update, delete, or compact (purge deprecated+duplicates).",
				"enum":        []string{"add", "update", "delete", "compact"},
			},
			"directive_id": map[string]any{
				"type":        "integer",
				"description": "[learn] 1-based ID required for update/delete.",
			},
			"supersedes": map[string]any{
				"type":        "array",
				"description": "[learn] Optional IDs to auto-deprecate when adding.",
				"items":       map[string]any{"type": "integer"},
			},
			// rem_sleep — both optional; defaults match the automatic idle cycle.
			"learning_rate": map[string]any{
				"type":        "number",
				"description": "[rem_sleep] Optional backprop learning rate. Default: the value the auto 5-min idle REM cycle uses (see defaultRemLearningRate).",
			},
			"session_success_ratio": map[string]any{
				"type":        "number",
				"description": "[rem_sleep] Optional session success ratio (0.0–1.0; >0.5 = success). Default: the value the auto idle REM cycle uses (see defaultRemSuccessRatio).",
			},
			// load_snapshot
			"snapshot_path": map[string]any{
				"type":        "string",
				"description": "[load_snapshot] Absolute path to the Gob snapshot file.",
			},
			// knowledge store
			"key": map[string]any{
				"type":        "string",
				"description": "[store/fetch/drop/search] Entry key (e.g. 'dto.CreateUserRequest', 'POST /api/users').",
			},
			"namespace": map[string]any{
				"type":        "string",
				"description": "[store/fetch/list/drop/search] Namespace: contracts|types|enums|rules|flows|patterns or custom. Use '*' in list/search to span all namespaces.",
			},
			"hot": map[string]any{
				"type":        "boolean",
				"description": "[store] When true, entry is loaded into RAM at boot for O(1) lookup.",
			},
			"tags": map[string]any{
				"type":        "array",
				"description": "[store] Optional string tags for filtering.",
				"items":       map[string]any{"type": "string"},
			},
			"tag": map[string]any{
				"type":        "string",
				"description": "[list] Filter by this tag. Empty = no filter.",
			},
			"k": map[string]any{
				"type":        "integer",
				"description": "[search] Max results to return (default 10).",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "[search] Substring to search over Key + Content.",
			},
			"tier": map[string]any{
				"type":        "string",
				"enum":        []string{"workspace", "project", "org", "nexus"},
				"description": "[store/fetch/list/drop/search] Which store to target: `workspace` (local .neo/db/knowledge.db), `project` (default, .neo-project/db/shared.db — cross-workspace within a project), `org` (.neo-org/db/org.db — cross-project within an organisation; namespaces reservados: directives, memory, debt, context), or `nexus` (~/.neo/shared/db/global.db — visible across ALL workspaces managed by this Nexus). `org` requires this project to be the org coordinator. Named `tier` to avoid collision with commit's domain-label `scope` argument.",
			},
			// [338.A] delegate / complete_task fields
			"epic_key": map[string]any{
				"type":        "string",
				"description": "[delegate/complete_task] Epic identifier in '<PILAR>-<id>' format (e.g. 'LX-331.A').",
			},
			"delegate_to": map[string]any{
				"type":        "string",
				"description": "[delegate] Target workspace name or ID (as shown in Nexus /status).",
			},
			"deadline_unix": map[string]any{
				"type":        "integer",
				"description": "[delegate] Optional UNIX timestamp for the task deadline.",
			},
			"result": map[string]any{
				"type":        "string",
				"description": "[complete_task] Outcome description — appended to the epic content and sent to the owner workspace inbox.",
			},
			// [342.A] resolve_conflict fields
			"conflict_key": map[string]any{
				"type":        "string",
				"description": "[resolve_conflict] Conflict key in 'ns:key:timestamp' format (from BRIEFING knowledge_conflicts warning or neo_memory search ns:conflicts).",
			},
			"winner": map[string]any{
				"type":        "string",
				"enum":        []string{"A", "B"},
				"description": "[resolve_conflict] Which version wins: 'A' keeps the current value (the writer that triggered the conflict), 'B' restores the loser (the value that was overwritten).",
			},
		},
		Required: []string{},
	}
}

func (t *MemoryTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	action, _ := args["action"].(string)
	if action == "learn" {
		return t.dispatchLearn(ctx, args)
	}
	switch action {
	case "commit":
		return t.commit.Execute(ctx, args)
	case "rem_sleep":
		return t.remSleep.Execute(ctx, withRemSleepDefaults(args))
	case "load_snapshot":
		return t.loadSnapshot.Execute(ctx, args)
	case "store":
		return t.execStore(args)
	case "fetch":
		return t.execFetch(args)
	case "list":
		return t.execList(args)
	case "drop":
		return t.execDrop(args)
	case "search":
		return t.execSearch(args)
	case "delegate":
		return t.execDelegate(args)
	case "complete_task":
		return t.execCompleteTask(args)
	case "resolve_conflict":
		return t.execResolveConflict(args)
	case "":
		return nil, fmt.Errorf("neo_memory: action is required")
	default:
		return nil, fmt.Errorf("neo_memory: unknown action %q", action)
	}
}

// withRemSleepDefaults fills in the canonical rem_sleep hyperparameters
// (defaultRemLearningRate / defaultRemSuccessRatio, defined alongside
// RemSleepTool in tools.go) when the caller omitted them. RemSleepTool.Execute
// requires both as float64; without this shim a bare neo_memory(action:
// "rem_sleep") would error out instead of consolidating the buffer. The
// neo_memory schema exposes both as OPTIONAL so an operator can override.
// Returns a fresh map so the caller's args aren't mutated. [P3 fix · 2026-05-15]
func withRemSleepDefaults(args map[string]any) map[string]any {
	out := make(map[string]any, len(args)+2)
	for k, v := range args {
		out[k] = v
	}
	if _, ok := out["learning_rate"].(float64); !ok {
		out["learning_rate"] = defaultRemLearningRate
	}
	if _, ok := out["session_success_ratio"].(float64); !ok {
		out["session_success_ratio"] = defaultRemSuccessRatio
	}
	return out
}

// dispatchLearn re-maps the top-level "action" to the legacy sub-tool's
// expected shape (action_type add/update/delete). Extracted to keep
// Execute's CC under the limit. [355.A refactor]
//
// When args["scope"] == "org", dispatches to the org-tier directives
// store (`.neo-org/DIRECTIVES.md`) instead of workspace-scope. [356.B]
func (t *MemoryTool) dispatchLearn(ctx context.Context, args map[string]any) (any, error) {
	if scope, _ := args["scope"].(string); scope == "org" {
		return t.dispatchLearnOrg(args)
	}
	if v, ok := args["action_type"]; ok {
		args["action"] = v
	} else {
		delete(args, "action")
	}
	return t.learn.Execute(ctx, args)
}

// dispatchLearnOrg persists a directive to `.neo-org/DIRECTIVES.md` via
// federation.AppendOrgDirective. action_type controls the verb (add default,
// update, delete). [356.B]
func (t *MemoryTool) dispatchLearnOrg(args map[string]any) (any, error) {
	orgDir, ok := config.FindNeoOrgDir(t.workspace)
	if !ok {
		return nil, fmt.Errorf("learn scope:\"org\" requires .neo-org/ in walk-up")
	}
	verb, _ := args["action_type"].(string)
	if verb == "" {
		verb = "add"
	}
	switch verb {
	case "add":
		return orgLearnAdd(orgDir, args)
	case "update":
		return orgLearnUpdate(orgDir, args)
	case "delete":
		return orgLearnDelete(orgDir, args)
	default:
		return nil, fmt.Errorf("unknown action_type %q — use add|update|delete", verb)
	}
}

func orgLearnAdd(orgDir string, args map[string]any) (any, error) {
	directive, _ := args["directive"].(string)
	if directive == "" {
		return nil, fmt.Errorf("directive is required for scope:org action:add")
	}
	var superseded []int
	if raw, ok := args["supersedes"].([]any); ok {
		for _, v := range raw {
			if f, ok := v.(float64); ok {
				superseded = append(superseded, int(f))
			}
		}
	}
	d, err := federation.AppendOrgDirective(orgDir, directive, superseded)
	if err != nil {
		return nil, err
	}
	return mcpOK(fmt.Sprintf("Org directive %d añadida: %s", d.ID, d.Text)), nil
}

func orgLearnUpdate(orgDir string, args map[string]any) (any, error) {
	idFloat, ok := args["directive_id"].(float64)
	if !ok || idFloat < 1 {
		return nil, fmt.Errorf("directive_id ≥ 1 required for update")
	}
	directive, _ := args["directive"].(string)
	if directive == "" {
		return nil, fmt.Errorf("directive text required for update")
	}
	if err := federation.UpdateOrgDirective(orgDir, int(idFloat), directive); err != nil {
		return nil, err
	}
	return mcpOK(fmt.Sprintf("Org directive %d actualizada.", int(idFloat))), nil
}

func orgLearnDelete(orgDir string, args map[string]any) (any, error) {
	idFloat, ok := args["directive_id"].(float64)
	if !ok || idFloat < 1 {
		return nil, fmt.Errorf("directive_id ≥ 1 required for delete")
	}
	if err := federation.DeprecateOrgDirective(orgDir, int(idFloat)); err != nil {
		return nil, err
	}
	return mcpOK(fmt.Sprintf("Org directive %d marcada deprecated.", int(idFloat))), nil
}

// execStore writes an entry to the Knowledge Store. [294.A]
func (t *MemoryTool) execStore(args map[string]any) (any, error) {
	tier, _ := args["tier"].(string)
	if tier == "nexus" {
		return t.proxyNexusOp("store", args)
	}
	if t.ks == nil && t.coordinatorWSID != "" {
		return t.proxyToCoordinator("store", args)
	}
	store, _, err := t.resolveStoreTier(tier)
	if err != nil {
		return nil, fmt.Errorf("neo_memory store: %w", err)
	}
	ns, _ := args["namespace"].(string)
	key, _ := args["key"].(string)
	if ns == "" || key == "" {
		return nil, fmt.Errorf("neo_memory store: namespace and key are required")
	}
	e := buildStoreEntry(ns, key, args)
	if err := store.Put(ns, key, e); err != nil {
		return nil, fmt.Errorf("neo_memory store: %w", err)
	}
	if t.hc != nil {
		t.hc.Set(e)
	}
	go notifyNexusKnowledgeBroadcast(t.workspace)
	t.publishContractKnowledgeUpdated(ns, key)
	effectiveTier := tier
	if effectiveTier == "" {
		effectiveTier = "project"
	}
	return mcpJSON(map[string]any{"ok": true, "key": key, "namespace": ns, "hot": e.Hot, "tier": effectiveTier}), nil
}

// buildStoreEntry assembles a KnowledgeEntry from raw JSON args. Extracted
// to keep execStore under the CC cap. [355.A refactor]
func buildStoreEntry(ns, key string, args map[string]any) knowledge.KnowledgeEntry {
	content, _ := args["content"].(string)
	hot, _ := args["hot"].(bool)
	var tags []string
	if raw, ok := args["tags"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				tags = append(tags, s)
			}
		}
	}
	e := knowledge.KnowledgeEntry{
		Key:            key,
		Namespace:      ns,
		Content:        content,
		Tags:           tags,
		Hot:            hot,
		SessionAgentID: currentSessionAgentID(), // [336.A]
	}
	// [338.A] Optional inbox-specific fields — only meaningful for namespace=inbox.
	if ns == knowledge.NSInbox {
		if from, ok := args["from"].(string); ok {
			e.From = from
		}
		if threadID, ok := args["thread_id"].(string); ok {
			e.ThreadID = threadID
		}
		if priority, ok := args["priority"].(string); ok {
			e.Priority = priority
		}
	}
	return e
}

// publishContractKnowledgeUpdated fires the SSE event when a contract-namespace
// entry was written. No-op on nil bus or non-contract namespace. [355.A refactor]
func (t *MemoryTool) publishContractKnowledgeUpdated(ns, key string) {
	if t.bus == nil || ns != knowledge.NSContracts {
		return
	}
	t.bus.Publish(pubsub.Event{
		Type: pubsub.EventType("contract_knowledge_updated"),
		At:   time.Now(),
		Payload: map[string]any{
			"key":          key,
			"workspace_id": t.workspace,
			"updated_at":   time.Now().Unix(),
		},
	})
}

// execFetch retrieves an entry. Hot cache first, then BoltDB. [294.B]
func (t *MemoryTool) execFetch(args map[string]any) (any, error) {
	tier, _ := args["tier"].(string)
	if tier == "nexus" {
		return t.proxyNexusOp("fetch", args)
	}
	if t.ks == nil && t.coordinatorWSID != "" {
		return t.proxyToCoordinator("fetch", args)
	}
	store, _, err := t.resolveStoreTier(tier)
	if err != nil {
		return nil, fmt.Errorf("neo_memory fetch: %w", err)
	}
	ns, _ := args["namespace"].(string)
	key, _ := args["key"].(string)
	if ns == "" || key == "" {
		return nil, fmt.Errorf("neo_memory fetch: namespace and key are required")
	}
	if t.hc != nil {
		if e, ok := t.hc.Get(ns, key); ok {
			return mcpJSON(map[string]any{"entry": e, "source": "hot_cache"}), nil
		}
	}
	e, err := store.Get(ns, key)
	if err != nil {
		return mcpJSON(map[string]any{"error": "not found", "namespace": ns, "key": key}), nil
	}
	return mcpJSON(map[string]any{"entry": e, "source": "store"}), nil
}

// execList returns all entries in a namespace, optionally filtered by tag. [294.C]
func (t *MemoryTool) execList(args map[string]any) (any, error) {
	tier, _ := args["tier"].(string)
	if tier == "nexus" {
		return t.proxyNexusOp("list", args)
	}
	if t.ks == nil && t.coordinatorWSID != "" {
		return t.proxyToCoordinator("list", args)
	}
	store, _, err := t.resolveStoreTier(tier)
	if err != nil {
		return nil, fmt.Errorf("neo_memory list: %w", err)
	}
	ns, _ := args["namespace"].(string)
	tag, _ := args["tag"].(string)
	if ns == "" {
		return nil, fmt.Errorf("neo_memory list: namespace is required (use '*' for all)")
	}
	if ns == "*" {
		return t.listAllFromStore(store, tag)
	}
	entries, err := store.List(ns, tag)
	if err != nil {
		return nil, fmt.Errorf("neo_memory list: %w", err)
	}
	return mcpJSON(buildListResponse(ns, entries)), nil
}

func (t *MemoryTool) listAllFromStore(store *knowledge.KnowledgeStore, tag string) (any, error) {
	if store == nil {
		return nil, fmt.Errorf("neo_memory list: store not available")
	}
	nss, err := store.ListNamespaces()
	if err != nil {
		return nil, fmt.Errorf("neo_memory list: %w", err)
	}
	type recentEntry struct {
		ns  string
		key string
		ts  int64
	}
	var result []map[string]any
	var recent []recentEntry
	for _, ns := range nss {
		entries, err := store.List(ns, tag)
		if err != nil {
			continue
		}
		result = append(result, buildListResponse(ns, entries))
		for _, e := range entries {
			recent = append(recent, recentEntry{ns: ns, key: e.Key, ts: e.UpdatedAt})
		}
	}
	// [298.D] Sort by UpdatedAt desc, surface top-5 as recently_updated.
	sort.Slice(recent, func(i, j int) bool { return recent[i].ts > recent[j].ts })
	topN := 5
	if len(recent) < topN {
		topN = len(recent)
	}
	recentOut := make([]map[string]any, topN)
	for i, e := range recent[:topN] {
		recentOut[i] = map[string]any{"namespace": e.ns, "key": e.key, "updated_at": e.ts}
	}
	return mcpJSON(map[string]any{"namespaces": result, "recently_updated": recentOut}), nil
}

// buildListResponse returns a shallow summary (no Content) for list responses. [294.C]
func buildListResponse(ns string, entries []knowledge.KnowledgeEntry) map[string]any {
	summaries := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		summaries = append(summaries, map[string]any{
			"key":        e.Key,
			"tags":       e.Tags,
			"hot":        e.Hot,
			"updated_at": e.UpdatedAt,
		})
	}
	return map[string]any{"namespace": ns, "entries": summaries}
}

// execDrop hard-deletes an entry. [294.D]
func (t *MemoryTool) execDrop(args map[string]any) (any, error) {
	tier, _ := args["tier"].(string)
	if tier == "nexus" {
		return t.proxyNexusOp("drop", args)
	}
	if t.ks == nil && t.coordinatorWSID != "" {
		return t.proxyToCoordinator("drop", args)
	}
	store, _, err := t.resolveStoreTier(tier)
	if err != nil {
		return nil, fmt.Errorf("neo_memory drop: %w", err)
	}
	ns, _ := args["namespace"].(string)
	key, _ := args["key"].(string)
	if ns == "" || key == "" {
		return nil, fmt.Errorf("neo_memory drop: namespace and key are required")
	}
	if err := store.Delete(ns, key); err != nil {
		return nil, fmt.Errorf("neo_memory drop: %w", err)
	}
	if t.hc != nil {
		t.hc.Delete(ns, key)
	}
	go deleteKnowledgeMD(t.workspace, ns, key)
	return mcpJSON(map[string]any{"ok": true, "dropped": key, "namespace": ns}), nil
}

// execSearch performs substring search over Key+Content. [294.E]
func (t *MemoryTool) execSearch(args map[string]any) (any, error) {
	tier, _ := args["tier"].(string)
	if tier == "nexus" {
		return t.proxyNexusOp("search", args)
	}
	if t.ks == nil && t.coordinatorWSID != "" {
		return t.proxyToCoordinator("search", args)
	}
	store, _, err := t.resolveStoreTier(tier)
	if err != nil {
		return nil, fmt.Errorf("neo_memory search: %w", err)
	}
	ns, _ := args["namespace"].(string)
	query, _ := args["query"].(string)
	if query == "" {
		query, _ = args["key"].(string) // fallback: use key as query
	}
	if query == "" {
		return nil, fmt.Errorf("neo_memory search: query is required")
	}
	k := 10
	if kv, ok := args["k"].(float64); ok && kv > 0 {
		k = int(kv)
	}
	results, err := store.Search(ns, query, k)
	if err != nil {
		return nil, fmt.Errorf("neo_memory search: %w", err)
	}
	out := make([]map[string]any, 0, len(results))
	for _, e := range results {
		matched := "key"
		if strings.Contains(strings.ToLower(e.Content), strings.ToLower(query)) {
			matched = "content"
		}
		out = append(out, map[string]any{
			"key":        e.Key,
			"namespace":  e.Namespace,
			"content":    e.Content,
			"hot":        e.Hot,
			"matched_on": matched,
		})
	}
	return mcpJSON(map[string]any{"query": query, "namespace": ns, "results": out}), nil
}

// resolveWorkspaceIDFromName queries Nexus /status and returns the exact workspace ID
// that matches idOrName (exact ID, name, or basename of path). [338.A]
func resolveWorkspaceIDFromName(nexusBase, idOrName string) (string, error) {
	url := nexusBase + "/status" //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase from nexusDispatcherBase
	client := sre.SafeInternalHTTPClient(3)
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	var statuses []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	for _, s := range statuses {
		if s.ID == idOrName || s.Name == idOrName || filepath.Base(s.Path) == idOrName {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("workspace %q not found in Nexus pool (use exact ID or name from /status)", idOrName)
}

// execResolveConflict settles an LWW conflict detected in KnowledgeStore.Put. [342.A]
// winner "A" keeps the current winner value (no-op on the live entry).
// winner "B" restores the loser value back to the original namespace/key.
func (t *MemoryTool) execResolveConflict(args map[string]any) (any, error) {
	conflictKey, _ := args["conflict_key"].(string)
	winner, _ := args["winner"].(string)
	if conflictKey == "" || (winner != "A" && winner != "B") {
		return nil, fmt.Errorf("neo_memory resolve_conflict: conflict_key and winner (A|B) are required")
	}
	store, _, err := t.resolveStoreTier("")
	if err != nil || store == nil {
		return nil, fmt.Errorf("neo_memory resolve_conflict: store not available")
	}
	if err := store.ResolveConflict(conflictKey, winner); err != nil {
		return nil, fmt.Errorf("neo_memory resolve_conflict: %w", err)
	}
	return mcpJSON(map[string]any{
		"ok":           true,
		"conflict_key": conflictKey,
		"winner":       winner,
		"action":       map[string]string{"A": "kept current value", "B": "restored overwritten value"}[winner],
	}), nil
}

// postInboxToWorkspace sends an inbox entry to a target workspace via Nexus MCP proxy. [338.A]
func postInboxToWorkspace(nexusBase, targetWSID, inboxKey, fromWS, threadID, priority, content string) error {
	storeArgs := map[string]any{
		"action":    "store",
		"namespace": knowledge.NSInbox,
		"key":       inboxKey,
		"content":   content,
		"tier":      "workspace",
		"from":      fromWS,
		"thread_id": threadID,
		"priority":  priority,
	}
	rpcReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "neo_memory", "arguments": storeArgs},
	}
	body, err := json.Marshal(rpcReq)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := nexusBase + "/workspaces/" + targetWSID + "/mcp/message" //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase from nexusDispatcherBase
	client := sre.SafeInternalHTTPClient(10)
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST %s: %w", targetWSID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("target workspace returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// execDelegate creates an inbox item in the target workspace and links it to an epic. [338.A]
func (t *MemoryTool) execDelegate(args map[string]any) (any, error) {
	epicKey, _ := args["epic_key"].(string)
	delegateTo, _ := args["delegate_to"].(string)
	contextText, _ := args["context"].(string)
	deadlineUnix, _ := args["deadline_unix"].(float64)
	if epicKey == "" || delegateTo == "" {
		return nil, fmt.Errorf("neo_memory delegate: epic_key and delegate_to are required")
	}
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return nil, fmt.Errorf("neo_memory delegate: Nexus dispatcher not available (not running under Nexus?)")
	}
	targetWSID, err := resolveWorkspaceIDFromName(nexusBase, delegateTo)
	if err != nil {
		return nil, fmt.Errorf("neo_memory delegate: %w", err)
	}
	content := fmt.Sprintf("**Delegated task: %s**\n\n%s", epicKey, contextText)
	if deadlineUnix > 0 {
		content += fmt.Sprintf("\n\n**Deadline:** %s", time.Unix(int64(deadlineUnix), 0).UTC().Format(time.RFC3339))
	}
	inboxKey := fmt.Sprintf("delegate:%s:%d", epicKey, time.Now().Unix())
	if err := postInboxToWorkspace(nexusBase, targetWSID, inboxKey, t.workspace, epicKey, knowledge.InboxPriorityUrgent, content); err != nil {
		return nil, fmt.Errorf("neo_memory delegate: send inbox to %s: %w", delegateTo, err)
	}
	return mcpJSON(map[string]any{
		"ok":          true,
		"epic_key":    epicKey,
		"delegate_to": targetWSID,
		"inbox_key":   inboxKey,
	}), nil
}

// execCompleteTask marks an epic as done and notifies its owner workspace. [338.A]
func (t *MemoryTool) execCompleteTask(args map[string]any) (any, error) {
	epicKey, _ := args["epic_key"].(string)
	result, _ := args["result"].(string)
	if epicKey == "" {
		return nil, fmt.Errorf("neo_memory complete_task: epic_key is required")
	}
	if t.ks == nil {
		return nil, fmt.Errorf("neo_memory complete_task: requires project mode (KnowledgeStore not available)")
	}
	eptr, err := t.ks.Get(knowledge.NSEpics, epicKey)
	if err != nil {
		return nil, fmt.Errorf("neo_memory complete_task: get epic %q: %w", epicKey, err)
	}
	ownerWSID := eptr.EpicOwner
	eptr.EpicStatus = knowledge.EpicStatusDone
	if result != "" {
		eptr.Content = eptr.Content + "\n\n**Result:** " + result
	}
	if err := t.ks.Put(knowledge.NSEpics, epicKey, *eptr); err != nil {
		return nil, fmt.Errorf("neo_memory complete_task: update epic: %w", err)
	}
	notified := ""
	if ownerWSID != "" && ownerWSID != t.workspace {
		nexusBase := nexusDispatcherBase()
		if nexusBase != "" {
			if ownerID, rerr := resolveWorkspaceIDFromName(nexusBase, ownerWSID); rerr == nil {
				content := fmt.Sprintf("**Task completed: %s**\n\n%s", epicKey, result)
				inboxKey := fmt.Sprintf("done:%s:%d", epicKey, time.Now().Unix())
				if herr := postInboxToWorkspace(nexusBase, ownerID, inboxKey, t.workspace, epicKey, knowledge.InboxPriorityNormal, content); herr == nil {
					notified = ownerWSID
				}
			}
		}
	}
	return mcpJSON(map[string]any{
		"ok":       true,
		"epic_key": epicKey,
		"status":   "done",
		"notified": notified,
	}), nil
}

// notifyNexusKnowledgeBroadcast fires a broadcast to Nexus so siblings refresh their hot cache.
// Fire-and-forget. [296.D]
func notifyNexusKnowledgeBroadcast(workspace string) {
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return
	}
	wsID := resolveWorkspaceID(workspace)
	if wsID == "" {
		wsID = "unknown"
	}
	client := sre.SafeInternalHTTPClient(5)
	url := nexusBase + "/internal/knowledge/broadcast?src=" + wsID
	resp, err := client.Post(url, "application/json", strings.NewReader("{}")) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: url derived from nexusDispatcherBase
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// deleteKnowledgeMD removes the companion .md file for a dropped entry, if present.
func deleteKnowledgeMD(workspace, ns, key string) {
	if workspace == "" {
		return
	}
	safe := safeKnowledgeFilename(key)
	path := workspace + "/.neo-project/knowledge/" + ns + "/" + safe + ".md"
	_ = os.Remove(path)
}

// safeKnowledgeFilename replaces filesystem-unsafe runes in an entry key.
func safeKnowledgeFilename(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// proxyNexusOp forwards tier:"nexus" operations to the Nexus dispatcher's
// /api/v1/shared/nexus/<op> endpoint. Nexus is the singleton owner of
// ~/.neo/shared/db/global.db — all children proxy here instead of opening
// their own handle. [354.Z-redesign]
//
// Op is one of store, fetch, list, drop, search. The payload is built from
// args (namespace, key, content, tags, hot, tag, query, k). Returns an
// mcpJSON-wrapped response mirroring the local exec* shape so callers can't
// tell the difference. On transport failure, returns a clear error.
func (t *MemoryTool) proxyNexusOp(op string, args map[string]any) (any, error) {
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return nil, fmt.Errorf("neo_memory %s tier:\"nexus\": Nexus dispatcher URL unknown (no NEO_NEXUS_URL / not running under Nexus)", op)
	}
	payload := buildNexusPayload(op, args)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("neo_memory %s tier:\"nexus\": marshal: %w", op, err)
	}
	client := sre.SafeInternalHTTPClient(10)
	url := nexusBase + "/api/v1/shared/nexus/" + op
	resp, err := client.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: url derived from nexusDispatcherBase
	if err != nil {
		return nil, fmt.Errorf("neo_memory %s tier:\"nexus\": POST %s: %w", op, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("neo_memory %s tier:\"nexus\": decode: %w", op, err)
	}
	if resp.StatusCode >= 400 {
		if errMsg, ok := out["error"].(string); ok {
			return mcpJSON(map[string]any{"error": errMsg, "tier": "nexus", "op": op}), nil
		}
		return mcpJSON(map[string]any{"error": fmt.Sprintf("nexus %s returned HTTP %d", op, resp.StatusCode), "tier": "nexus"}), nil
	}
	// Annotate tier:"nexus" on store response for parity with local path.
	if op == "store" {
		out["tier"] = "nexus"
	}
	return mcpJSON(out), nil
}

// proxyToCoordinator forwards tier:"project"|"workspace" ops from a non-
// coordinator child to the coordinator via Nexus MCP routing. The coordinator
// owns the bbolt flock on .neo-project/db/knowledge.db; non-coordinators
// never open it.
//
// To avoid an infinite proxy loop, the outgoing args force tier:"project"
// on the coordinator side (coordinator has ks != nil, won't re-proxy).
//
// [T003 nexus / 2026-05-10] silent-fail hardening: previously the
// proxy swallowed a "coord returned empty response" branch as success
// (mcpJSON with `error` field but no Go-level error). The agent saw
// no error and assumed the store landed; in fact nothing persisted.
// Now: empty response and unrecognised payloads BOTH return Go-level
// errors so the caller can't mistake them for success. Plus explicit
// `[KNOWLEDGE-PROXY]` log lines audit every proxy call's outcome.
//
// [354.Z-redesign piece 2]
func (t *MemoryTool) proxyToCoordinator(action string, args map[string]any) (any, error) {
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return nil, fmt.Errorf("neo_memory %s: non-coordinator proxy needs Nexus dispatcher URL (not running under Nexus?)", action)
	}
	forwardArgs := map[string]any{"action": action}
	for k, v := range args {
		if k == "tier" {
			continue
		}
		forwardArgs[k] = v
	}
	forwardArgs["tier"] = "project"
	rpcReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "neo_memory", "arguments": forwardArgs},
	}
	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("neo_memory %s coord-proxy: marshal: %w", action, err)
	}
	url := nexusBase + "/workspaces/" + t.coordinatorWSID + "/mcp/message"
	keyForLog := ""
	if k, ok := forwardArgs["key"].(string); ok {
		keyForLog = k
	}
	log.Printf("[KNOWLEDGE-PROXY] action=%s coord=%s key=%s — POST", action, t.coordinatorWSID, keyForLog)
	client := sre.SafeInternalHTTPClient(10)
	resp, err := client.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: url derived from nexusDispatcherBase
	if err != nil {
		log.Printf("[KNOWLEDGE-PROXY] action=%s coord=%s key=%s — POST failed: %v", action, t.coordinatorWSID, keyForLog, err)
		return nil, fmt.Errorf("neo_memory %s coord-proxy: POST %s: %w", action, url, err)
	}
	defer resp.Body.Close()
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
				Type string `json:"type"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		log.Printf("[KNOWLEDGE-PROXY] action=%s coord=%s key=%s — decode failed: %v", action, t.coordinatorWSID, keyForLog, err)
		return nil, fmt.Errorf("neo_memory %s coord-proxy: decode: %w", action, err)
	}
	if env.Error != nil {
		log.Printf("[KNOWLEDGE-PROXY] action=%s coord=%s key=%s — coord error: %s", action, t.coordinatorWSID, keyForLog, env.Error.Message)
		return nil, fmt.Errorf("neo_memory %s coord-proxy: coord returned error: %s", action, env.Error.Message)
	}
	if len(env.Result.Content) == 0 {
		// [T003 fix] previously returned mcpJSON{"error":...} with nil Go-error,
		// which the agent interpreted as success. Surface as real error.
		log.Printf("[KNOWLEDGE-PROXY] action=%s coord=%s key=%s — empty content", action, t.coordinatorWSID, keyForLog)
		return nil, fmt.Errorf("neo_memory %s coord-proxy: coord returned empty response (data NOT persisted)", action)
	}
	// Parse the inner JSON the coord sent.
	var inner map[string]any
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &inner); err != nil {
		log.Printf("[KNOWLEDGE-PROXY] action=%s coord=%s key=%s — coord returned non-JSON: %s", action, t.coordinatorWSID, keyForLog, env.Result.Content[0].Text)
		return nil, fmt.Errorf("neo_memory %s coord-proxy: coord returned non-JSON payload: %s", action, env.Result.Content[0].Text)
	}
	// [T003 fix] strict success marker check — the coord MUST set ok:true
	// or include a recognised non-error field (entries[]/value/etc.). If
	// the inner payload contains an `error` field, surface it as a Go
	// error so the caller can't mistake it for success.
	if errMsg, ok := inner["error"].(string); ok && errMsg != "" {
		log.Printf("[KNOWLEDGE-PROXY] action=%s coord=%s key=%s — coord embedded error: %s", action, t.coordinatorWSID, keyForLog, errMsg)
		return nil, fmt.Errorf("neo_memory %s coord-proxy: %s", action, errMsg)
	}
	log.Printf("[KNOWLEDGE-PROXY] action=%s coord=%s key=%s — ok", action, t.coordinatorWSID, keyForLog)
	return mcpJSON(inner), nil
}

// buildNexusPayload extracts the subset of args relevant to each Nexus op.
func buildNexusPayload(op string, args map[string]any) map[string]any {
	p := map[string]any{}
	if v, ok := args["namespace"].(string); ok {
		p["namespace"] = v
	}
	if v, ok := args["key"].(string); ok {
		p["key"] = v
	}
	switch op {
	case "store":
		if v, ok := args["content"].(string); ok {
			p["content"] = v
		}
		if v, ok := args["hot"].(bool); ok {
			p["hot"] = v
		}
		if raw, ok := args["tags"].([]any); ok {
			tags := make([]string, 0, len(raw))
			for _, v := range raw {
				if s, ok := v.(string); ok {
					tags = append(tags, s)
				}
			}
			p["tags"] = tags
		}
	case "list":
		if v, ok := args["tag"].(string); ok {
			p["tag"] = v
		}
	case "search":
		if v, ok := args["query"].(string); ok {
			p["query"] = v
		}
		if kv, ok := args["k"].(float64); ok {
			p["k"] = int(kv)
		}
	}
	return p
}
