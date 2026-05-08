package main

// tool_debt.go — neo_debt MCP tool: 4-tier debt access (workspace/project/
// nexus/org). [PILAR LXVI / 351.C]
//
// Tiers and backing storage:
//   workspace → <workspace>/.neo/technical_debt.md (kanban package)
//   project   → <projectRoot>/.neo-project/SHARED_DEBT.md (federation package)
//   nexus     → HTTP /internal/nexus/debt on Nexus dispatcher (PILAR LXVI)
//   org       → reserved for PILAR LXVII — returns "unavailable" for now
//
// Actions:
//   list           → render table for the given scope+filter
//   record         → append to workspace debt (MVP; project/nexus/org unsupported)
//   resolve        → only nexus tier in MVP; posts to dispatcher with auth
//   affecting_me   → shortcut: list nexus debt whose affected workspaces include us
//   fetch          → not yet implemented (MVP defers to list)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/kanban"
	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

type DebtTool struct {
	workspace   string
	workspaceID string // ID under Nexus (for affecting_me)
	cfg         *config.NeoConfig
	nexusURL    string // base URL, default http://127.0.0.1:9000
}

func NewDebtTool(workspace, workspaceID string, cfg *config.NeoConfig) *DebtTool {
	baseURL := "http://127.0.0.1:9000"
	// Allow override via env var (doctrine: no hardcoded constants in prod).
	if v := os.Getenv("NEO_NEXUS_URL"); v != "" {
		baseURL = v
	}
	return &DebtTool{
		workspace:   workspace,
		workspaceID: workspaceID,
		cfg:         cfg,
		nexusURL:    baseURL,
	}
}

func (t *DebtTool) Name() string { return "neo_debt" }

func (t *DebtTool) Description() string {
	return "SRE Tool: unified debt registry access across 4 tiers (workspace/project/nexus/org). Actions: list (rendered table), record (workspace only in MVP), resolve (nexus only in MVP, requires X-Nexus-Token when configured), affecting_me (shortcut: open nexus events impacting this workspace), fetch (reserved). Use affecting_me at session start to see if Nexus detected issues blocking this workspace."
}

func (t *DebtTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "list | record | resolve | affecting_me | fetch",
				"enum":        []string{"list", "record", "resolve", "affecting_me", "fetch"},
			},
			"scope": map[string]any{
				"type":        "string",
				"description": "workspace (default) | project | nexus | org. For affecting_me, scope is always nexus.",
				"enum":        []string{"workspace", "project", "nexus", "org"},
			},
			"filter": map[string]any{
				"type":        "string",
				"description": "[list] open (default) | all",
				"enum":        []string{"open", "all"},
			},
			"priority": map[string]any{
				"type":        "string",
				"description": "[list] filter by priority",
				"enum":        []string{"P0", "P1", "P2", "P3"},
			},
			"id": map[string]any{
				"type":        "string",
				"description": "[resolve|fetch] event ID to act on",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "[record] short summary line",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "[record] full body with file:line + recommended fix",
			},
			"resolution": map[string]any{
				"type":        "string",
				"description": "[resolve] resolution note appended to the event",
			},
		},
		Required: []string{"action"},
	}
}

func (t *DebtTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	action, _ := args["action"].(string)
	scope := strArg(args, "scope", "workspace")

	switch action {
	case "list":
		return t.doList(ctx, scope, args)
	case "affecting_me":
		return t.doAffectingMe(ctx)
	case "resolve":
		return t.doResolve(ctx, scope, args)
	case "record":
		return t.doRecord(scope, args)
	case "fetch":
		return mcpText("fetch is not implemented yet — use list + grep the output"), nil
	default:
		return nil, fmt.Errorf("unknown action %q", action)
	}
}

func (t *DebtTool) doList(ctx context.Context, scope string, args map[string]any) (any, error) {
	switch scope {
	case "workspace":
		return t.listWorkspace()
	case "project":
		return t.listProject()
	case "nexus":
		return t.listNexus(ctx, args)
	case "org":
		return t.listOrg()
	default:
		return nil, fmt.Errorf("unknown scope %q", scope)
	}
}

func (t *DebtTool) listWorkspace() (any, error) {
	path := fmt.Sprintf("%s/.neo/technical_debt.md", t.workspace)
	data, err := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON: workspace pinned at boot
	if err != nil {
		if os.IsNotExist(err) {
			return mcpText("## Workspace Technical Debt\n\n_empty — no deficiencies detected_\n"), nil
		}
		return nil, err
	}
	return mcpText(string(data)), nil
}

func (t *DebtTool) listProject() (any, error) {
	projDir, ok := federation.FindNeoProjectDir(t.workspace)
	if !ok {
		return mcpText("## Project Debt\n\n_no .neo-project/ found in walk-up — this workspace is not part of a federation_\n"), nil
	}
	rows, err := federation.ParseSharedDebt(projDir)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return mcpText("## Project Debt (SHARED_DEBT.md)\n\n_empty_\n"), nil
	}
	var sb strings.Builder
	sb.WriteString("## Project Debt (SHARED_DEBT.md)\n\n")
	sb.WriteString("| Endpoint | Caller | Workspace |\n|----------|--------|-----------|\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "| %s | %s | %s |\n", r.Endpoint, r.Caller, r.Workspace)
	}
	return mcpText(sb.String()), nil
}

// listOrg reads `.neo-org/DEBT.md` via walk-up from the workspace. Returns
// an empty-marker message when the workspace is not part of any org. [356.A]
func (t *DebtTool) listOrg() (any, error) {
	orgDir, ok := config.FindNeoOrgDir(t.workspace)
	if !ok {
		return mcpText("## Org Debt\n\n_no .neo-org/ found in walk-up — this workspace is not part of an organisation_\n"), nil
	}
	entries, err := federation.ListOrgDebt(orgDir)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return mcpText("## Org Debt (.neo-org/DEBT.md)\n\n_empty_\n"), nil
	}
	var sb strings.Builder
	sb.WriteString("## Org Debt (.neo-org/DEBT.md)\n\n")
	sb.WriteString("| Priority | ID | Title | Affected Projects | Detected | Status |\n")
	sb.WriteString("|----------|----|-------|-------------------|----------|--------|\n")
	for _, e := range entries {
		status := "open"
		if !e.ResolvedAt.IsZero() {
			status = "resolved"
		}
		fmt.Fprintf(&sb, "| %s | `%s` | %s | %s | %s | %s |\n",
			e.Priority, e.ID, e.Title,
			strings.Join(e.AffectedProjects, ","),
			e.DetectedAt.Format("2006-01-02 15:04"), status)
	}
	return mcpText(sb.String()), nil
}

func (t *DebtTool) listNexus(ctx context.Context, args map[string]any) (any, error) {
	url := t.nexusURL + "/internal/nexus/debt"
	if pri, ok := args["priority"].(string); ok && pri != "" {
		url += "?priority=" + pri
	}
	events, err := t.fetchNexusEvents(ctx, url)
	if err != nil {
		return nil, err
	}
	return mcpText(renderNexusDebtTable(events, "## Nexus Debt")), nil
}

// doAffectingMe aggregates open debt from all 4 tiers that affects this workspace. [357.A]
// Tiers queried in parallel (best-effort — tier error → empty section, not fatal):
//   workspace → technical_debt.md (local open entries)
//   project   → SHARED_DEBT.md filtered by this workspace name
//   org       → .neo-org/DEBT.md all open cross-project entries
//   nexus     → HTTP /internal/nexus/debt/affecting (dispatcher events)
func (t *DebtTool) doAffectingMe(ctx context.Context) (any, error) {
	var all []affectingEntry

	// 1. workspace tier — technical_debt.md always targets this workspace.
	debtPath := filepath.Join(t.workspace, ".neo", "technical_debt.md")
	if raw, err := os.ReadFile(debtPath); err == nil { //nolint:gosec // G304-WORKSPACE-CANON: workspace pinned at boot
		all = append(all, parseWorkspaceDebt(raw)...)
	}

	// 2. project tier — SHARED_DEBT.md rows where Workspace matches this ws.
	wsName := filepath.Base(t.workspace)
	all = append(all, t.collectProjectAffecting(wsName)...)

	// 3. org tier — all open entries (cross-project by nature).
	all = append(all, t.collectOrgAffecting()...)

	// 4. nexus tier — workspace-specific events from dispatcher.
	nexusNote := ""
	if t.workspaceID != "" {
		url := fmt.Sprintf("%s/internal/nexus/debt/affecting?workspace_id=%s", t.nexusURL, t.workspaceID)
		if events, err := t.fetchNexusEvents(ctx, url); err == nil {
			for _, e := range events {
				rec := e.Recommended
				if rec == "" {
					rec = "—"
				}
				all = append(all, affectingEntry{
					tier:     "nexus",
					priority: e.Priority,
					id:       e.ID,
					title:    e.Title,
					detected: e.DetectedAt.Format("2006-01-02 15:04"),
					info:     "Recommended: " + rec,
				})
			}
		}
	} else {
		nexusNote = "\n> ⚠️ nexus tier skipped: workspace_id unknown — set workspaceID to enable Nexus debt events.\n"
	}

	wsLabel := t.workspaceID
	if wsLabel == "" {
		wsLabel = wsName
	}
	title := fmt.Sprintf("## Debt affecting `%s` (4 tiers)", wsLabel)
	return mcpText(renderUnifiedAffectingTable(all, title) + nexusNote), nil
}

func (t *DebtTool) doResolve(ctx context.Context, scope string, args map[string]any) (any, error) {
	switch scope {
	case "nexus":
		return t.resolveNexus(ctx, args)
	case "org":
		return t.resolveOrg(args)
	default:
		return nil, fmt.Errorf("resolve not implemented for scope=%q (supported: nexus, org)", scope)
	}
}

func (t *DebtTool) resolveNexus(ctx context.Context, args map[string]any) (any, error) {
	id, _ := args["id"].(string)
	resolution, _ := args["resolution"].(string)
	if id == "" {
		return nil, fmt.Errorf("id is required for resolve")
	}
	body, _ := json.Marshal(map[string]string{"id": id, "resolution": resolution})
	req, err := http.NewRequestWithContext(ctx, "POST", t.nexusURL+"/internal/nexus/debt/resolve", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("NEO_NEXUS_TOKEN"); tok != "" {
		req.Header.Set("X-Nexus-Token", tok)
	}
	client := sre.SafeInternalHTTPClient(5)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nexus resolve: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nexus resolve returned %d: %s", resp.StatusCode, bodyBytes)
	}
	t.appendResolutionAuditLog("nexus", id, resolution)
	return mcpText(fmt.Sprintf("✅ Nexus debt %s resolved\n\nNote: %s\n", id, resolution)), nil
}

// resolveOrg marks a `.neo-org/DEBT.md` entry resolved. Uses basename of
// current workspace as resolvedBy. [356.A]
func (t *DebtTool) resolveOrg(args map[string]any) (any, error) {
	orgDir, ok := config.FindNeoOrgDir(t.workspace)
	if !ok {
		return nil, fmt.Errorf("resolve scope=org requires .neo-org/ in walk-up")
	}
	id, _ := args["id"].(string)
	resolution, _ := args["resolution"].(string)
	if id == "" {
		return nil, fmt.Errorf("id is required for resolve")
	}
	resolvedBy := t.workspaceID
	if resolvedBy == "" {
		resolvedBy = t.workspace
	}
	if err := federation.ResolveOrgDebt(orgDir, id, resolvedBy, resolution); err != nil {
		return nil, err
	}
	t.appendResolutionAuditLog("org", id, resolution)
	return mcpText(fmt.Sprintf("✅ Org debt %s resolved\n\nNote: %s\n", id, resolution)), nil
}

func (t *DebtTool) doRecord(scope string, args map[string]any) (any, error) {
	switch scope {
	case "workspace":
		return t.recordWorkspace(args)
	case "org":
		return t.recordOrg(args)
	default:
		return nil, fmt.Errorf("record not implemented for scope=%q (supported: workspace, org)", scope)
	}
}

func (t *DebtTool) recordWorkspace(args map[string]any) (any, error) {
	title, _ := args["title"].(string)
	description, _ := args["description"].(string)
	priority := strArg(args, "priority", "P2")
	if title == "" {
		return nil, fmt.Errorf("title is required for record")
	}
	if err := kanban.AppendTechDebt(t.workspace, title, description, priority); err != nil {
		return nil, err
	}
	return mcpText(fmt.Sprintf("✅ Recorded workspace debt: %s [%s]\n", title, priority)), nil
}

// recordOrg appends a new entry to `.neo-org/DEBT.md`. Requires the calling
// workspace to be under an org (walk-up finds `.neo-org/`). [356.A]
func (t *DebtTool) recordOrg(args map[string]any) (any, error) {
	orgDir, ok := config.FindNeoOrgDir(t.workspace)
	if !ok {
		return nil, fmt.Errorf("record scope=org requires .neo-org/ in walk-up")
	}
	title, _ := args["title"].(string)
	description, _ := args["description"].(string)
	priority := strArg(args, "priority", "P2")
	if title == "" {
		return nil, fmt.Errorf("title is required for record")
	}
	var affected []string
	if raw, ok := args["affected_projects"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				affected = append(affected, s)
			}
		}
	}
	e, err := federation.AppendOrgDebt(orgDir, federation.OrgDebtEntry{
		Title:            title,
		Description:      description,
		Priority:         priority,
		AffectedProjects: affected,
		Source:           "manual",
	})
	if err != nil {
		return nil, err
	}
	return mcpText(fmt.Sprintf("✅ Recorded org debt: %s [%s] id=%s\n", title, priority, e.ID)), nil
}

func (t *DebtTool) fetchNexusEvents(ctx context.Context, url string) ([]nexus.NexusDebtEvent, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	client := sre.SafeInternalHTTPClient(5)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nexus debt query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // disabled → treat as empty
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("nexus debt query returned %d: %s", resp.StatusCode, body)
	}
	var events []nexus.NexusDebtEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}
	return events, nil
}

// ---------------------------------------------------------------------------
// 357.A — Unified affecting_me helpers
// ---------------------------------------------------------------------------

// collectProjectAffecting returns SHARED_DEBT.md rows that mention this workspace.
func (t *DebtTool) collectProjectAffecting(wsName string) []affectingEntry {
	projDir, ok := federation.FindNeoProjectDir(t.workspace)
	if !ok {
		return nil
	}
	rows, err := federation.ParseSharedDebt(projDir)
	if err != nil {
		return nil
	}
	var out []affectingEntry
	for _, r := range rows {
		if strings.Contains(r.Workspace, wsName) || strings.Contains(r.Workspace, t.workspace) {
			out = append(out, affectingEntry{
				tier:  "project",
				title: r.Endpoint + " (caller: " + r.Caller + ")",
			})
		}
	}
	return out
}

// collectOrgAffecting returns all open .neo-org/DEBT.md entries (org-wide by nature).
func (t *DebtTool) collectOrgAffecting() []affectingEntry {
	orgDir, ok := config.FindNeoOrgDir(t.workspace)
	if !ok {
		return nil
	}
	entries, err := federation.ListOrgDebt(orgDir)
	if err != nil {
		return nil
	}
	var out []affectingEntry
	for _, e := range entries {
		if !e.ResolvedAt.IsZero() {
			continue
		}
		out = append(out, affectingEntry{
			tier:     "org",
			priority: e.Priority,
			id:       e.ID,
			title:    e.Title,
			detected: e.DetectedAt.Format("2006-01-02"),
			info:     "projects: " + strings.Join(e.AffectedProjects, ", "),
		})
	}
	return out
}

// affectingEntry is a tier-agnostic debt item for the unified affecting_me view.
type affectingEntry struct {
	tier     string // workspace | project | org | nexus
	priority string // P0..P3 or ""
	id       string
	title    string
	detected string
	info     string // freeform: recommended action, project list, caller, etc.
}

// debtPriorityRank maps P0-P3 to sort keys; unknowns sort last.
func debtPriorityRank(p string) int {
	switch p {
	case "P0":
		return 0
	case "P1":
		return 1
	case "P2":
		return 2
	case "P3":
		return 3
	default:
		return 4
	}
}

// parseWorkspaceDebt extracts open entries from technical_debt.md.
// Entry format written by kanban.AppendTechDebt:
//
//	## [2006-01-02T15:04:05] title
//	**Prioridad:** alta|media|baja|crítica
func parseWorkspaceDebt(data []byte) []affectingEntry {
	var entries []affectingEntry
	lines := strings.Split(string(data), "\n")
	inResolved := false
	for i, line := range lines {
		if strings.HasPrefix(line, "## Deuda resuelta") {
			inResolved = true
			continue
		}
		if inResolved || !strings.HasPrefix(line, "## [") {
			continue
		}
		rest := line[4:] // strip "## ["
		ts, title, ok := strings.Cut(rest, "]")
		if !ok {
			continue
		}
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		priority := ""
		if i+1 < len(lines) {
			if after, ok := strings.CutPrefix(lines[i+1], "**Prioridad:** "); ok {
				priority = normalizeDebtPriority(strings.TrimSpace(after))
			}
		}
		detected := ts
		if len(detected) > 10 {
			detected = detected[:10]
		}
		entries = append(entries, affectingEntry{
			tier:     "workspace",
			priority: priority,
			title:    title,
			detected: detected,
		})
	}
	return entries
}

// normalizeDebtPriority maps Spanish priority words to P0-P3 scale.
func normalizeDebtPriority(s string) string {
	switch strings.ToLower(s) {
	case "crítica", "critica", "bloqueante":
		return "P0"
	case "alta":
		return "P1"
	case "media":
		return "P2"
	case "baja":
		return "P3"
	default:
		return s // pass through P0-P3 literals unchanged
	}
}

// renderUnifiedAffectingTable renders a sorted, tier-badged Markdown table.
func renderUnifiedAffectingTable(entries []affectingEntry, title string) string {
	if len(entries) == 0 {
		return title + "\n\n✅ No open debt affecting this workspace across all 4 tiers.\n"
	}
	sort.Slice(entries, func(i, j int) bool {
		ri, rj := debtPriorityRank(entries[i].priority), debtPriorityRank(entries[j].priority)
		if ri != rj {
			return ri < rj
		}
		return entries[i].tier < entries[j].tier
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n\n", title)
	sb.WriteString("| Tier | Priority | Title | Detected | Info |\n")
	sb.WriteString("|------|----------|-------|----------|------|\n")
	for _, e := range entries {
		prio := e.priority
		if prio == "" {
			prio = "—"
		}
		det := e.detected
		if det == "" {
			det = "—"
		}
		info := e.info
		if info == "" {
			info = "—"
		}
		id := e.id
		if id != "" {
			id = "`" + id + "` "
		}
		fmt.Fprintf(&sb, "| %s | %s | %s%s | %s | %s |\n",
			e.tier, prio, id, e.title, det, info)
	}
	return sb.String()
}

func renderNexusDebtTable(events []nexus.NexusDebtEvent, title string) string {
	if len(events) == 0 {
		return fmt.Sprintf("%s\n\n_no open events_\n", title)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n\n", title)
	sb.WriteString("| Priority | ID | Title | Affected | Detected | Count |\n")
	sb.WriteString("|----------|----|-------|----------|----------|-------|\n")
	for _, e := range events {
		fmt.Fprintf(&sb, "| %s | `%s` | %s | %s | %s | %d |\n",
			e.Priority, e.ID, e.Title,
			strings.Join(e.AffectedWorkspaces, ","),
			e.DetectedAt.Format("2006-01-02 15:04:05"),
			e.OccurrenceCount)
		if e.Recommended != "" {
			fmt.Fprintf(&sb, "| | | _Recommended: %s_ | | | |\n", e.Recommended)
		}
	}
	return sb.String()
}

// appendResolutionAuditLog writes a JSONL entry to .neo/db/debt_resolution_log.jsonl
// when sre.debt_audit_log is enabled. Best-effort: errors are logged but not fatal. [361.A]
func (t *DebtTool) appendResolutionAuditLog(scope, id, resolution string) {
	if t.cfg == nil || !t.cfg.SRE.DebtAuditLog {
		return
	}
	logPath := filepath.Join(t.workspace, ".neo", "db", "debt_resolution_log.jsonl")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("[DEBT-AUDIT] open %s: %v", logPath, err)
		return
	}
	defer f.Close()
	entry := map[string]string{
		"ts":                   time.Now().UTC().Format(time.RFC3339),
		"scope":                scope,
		"id":                   id,
		"resolution":           resolution,
		"resolved_by_workspace": filepath.Base(t.workspace),
	}
	line, _ := json.Marshal(entry)
	_, err = fmt.Fprintf(f, "%s\n", line)
	if err != nil {
		log.Printf("[DEBT-AUDIT] write: %v", err)
	}
}

func strArg(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return def
}
