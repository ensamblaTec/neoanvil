package main

// daemon_handlers.go — handler methods for DaemonTool.Execute.
// Each action is extracted to keep CC(Execute) < 15.
// [SRE-REFACTOR] CC reduction: 20 → <15 per function.

import (
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
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/federation"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/state"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

func (t *DaemonTool) handlePushTasks(_ context.Context, args map[string]any) (any, error) {
	tasksRaw, ok := args["tasks"].([]any)
	if !ok {
		return nil, fmt.Errorf("tasks array is required for PushTasks")
	}
	// [349.A] scope:"project" pushes to the shared PROJECT_TASKS.md queue so
	// any federation member's daemon can claim. Default scope is workspace-local.
	scope, _ := args["scope"].(string)
	if scope == "project" {
		return t.handlePushTasksProject(tasksRaw)
	}
	var tasks []state.SRETask
	for _, tr := range tasksRaw {
		tMap, _ := tr.(map[string]any)
		desc, _ := tMap["description"].(string)
		file, _ := tMap["target_file"].(string)
		role, _ := tMap["role"].(string) // [SRE-25.1.1] propagate role field
		tasks = append(tasks, state.SRETask{Description: desc, TargetFile: file, Role: role})
	}
	if err := state.EnqueueTasks(tasks); err != nil {
		return nil, err
	}
	return mcpText("Tasks enqueued successfully."), nil
}

// handlePushTasksProject writes tasks to .neo-project/PROJECT_TASKS.md so any
// federation member workspace can claim. [349.A]
func (t *DaemonTool) handlePushTasksProject(tasksRaw []any) (any, error) {
	projDir, ok := federation.FindNeoProjectDir(t.workspace)
	if !ok {
		return nil, fmt.Errorf("scope:\"project\" requires .neo-project/ in walk-up — this workspace is standalone")
	}
	enqueued := 0
	for _, tr := range tasksRaw {
		m, _ := tr.(map[string]any)
		desc, _ := m["description"].(string)
		target, _ := m["target_workspace"].(string)
		role, _ := m["role"].(string)
		file, _ := m["target_file"].(string)
		var tags []string
		if raw, ok := m["affinity_tags"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					tags = append(tags, s)
				}
			}
		}
		if _, err := federation.AppendProjectTask(projDir, federation.ProjectTask{
			Description:     desc,
			TargetWorkspace: target,
			Role:            role,
			TargetFile:      file,
			AffinityTags:    tags,
		}); err != nil {
			return nil, err
		}
		enqueued++
	}
	return mcpText(fmt.Sprintf("Project tasks enqueued: %d (file: %s/PROJECT_TASKS.md)", enqueued, projDir)), nil
}

func (t *DaemonTool) handlePullTasks(_ context.Context, args map[string]any) (any, error) {
	// [SRE-25.1.3] Route by agent_role if provided, else fall back to unroled tasks.
	agentRole, _ := args["agent_role"].(string)
	task, err := state.GetNextTaskByRole(agentRole)
	if err != nil {
		return nil, err
	}
	if task != nil {
		// [362.A] Mark task in_progress so the orphan scanner can detect stalls.
		_ = state.MarkTaskInProgress(task.ID)
		roleLabel := ""
		if task.Role != "" {
			roleLabel = fmt.Sprintf(" [role:%s]", task.Role)
		}
		// [132.C] Enrich response with active_task + queue_summary for HUD visibility.
		sessionID, _ := args["session_id"].(string)
		sessionBudget := 0
		backendMode := "auto"
		if t.cfg != nil {
			sessionBudget = t.cfg.SRE.DaemonTokenBudgetSession
			backendMode = t.cfg.SRE.DaemonBackendMode
		}
		summary, _ := state.GetDaemonQueueSummary(sessionID, sessionBudget)
		// [132.F] Resolve suggested backend for this task.
		hasKey := os.Getenv("DEEPSEEK_API_KEY") != ""
		suggestedBackend, deepseekTool := state.ResolveSuggestedBackend(task, backendMode, hasKey, false)
		type pullResp struct {
			Task             string                  `json:"task"`
			ActiveTask       *state.DaemonActiveTask `json:"active_task,omitempty"`
			QueueSummary     state.DaemonQueueSummary `json:"queue_summary"`
			SuggestedBackend string                  `json:"suggested_backend"`
			DeepSeekTool     string                  `json:"deepseek_tool,omitempty"`
		}
		active, _ := state.GetDaemonActiveTask()
		resp := pullResp{
			Task:             fmt.Sprintf("Next Task: [%s]%s %s (Target: %s)", task.ID, roleLabel, task.Description, task.TargetFile),
			ActiveTask:       active,
			QueueSummary:     summary,
			SuggestedBackend: suggestedBackend,
			DeepSeekTool:     deepseekTool,
		}
		raw, _ := json.Marshal(resp)
		return mcpText(string(raw)), nil
	}
	// [349.A] Local queue empty — try the shared project queue for tasks
	// directed at this workspace (or `*` claimable by anyone).
	if projDir, ok := federation.FindNeoProjectDir(t.workspace); ok {
		wsID := filepath.Base(t.workspace)
		if pt, perr := federation.ClaimProjectTask(projDir, wsID); perr == nil {
			return mcpText(fmt.Sprintf("Next Project Task: [%s] target=%s %s (from shared queue; CompleteProjectTask when done)",
				pt.ID, pt.TargetWorkspace, pt.Description)), nil
		}
	}
	return mcpText("Queue empty."), nil
}

func (t *DaemonTool) handleVacuumMemory(ctx context.Context, args map[string]any) (any, error) {
	// [348.A] Project scatter: coordinate Vacuum_Memory across all running member workspaces.
	if scope, _ := args["scope"].(string); scope == "project" {
		return t.scatterProjectVacuum(ctx)
	}
	go func() {
		var ignoreDirs []string
		ttlHours := 24
		cfgPath := filepath.Join(t.workspace, "neo.yaml")
		if cfg, errCfg := config.LoadConfig(cfgPath); errCfg == nil {
			ignoreDirs = cfg.Workspace.IgnoreDirs
			if cfg.SRE.SessionStateTTLHours > 0 {
				ttlHours = cfg.SRE.SessionStateTTLHours
			}
		}
		_, _ = t.wal.Vacuum(context.Background(), t.workspace, ignoreDirs)
		// [SRE-108.B] Purge session_state entries older than configured TTL.
		if err := t.wal.PurgeOldSessions(time.Duration(ttlHours) * time.Hour); err != nil {
			log.Printf("[SRE-108.B] PurgeOldSessions error: %v", err)
		}
	}()
	return mcpText("[SRE-DAEMON] Vacuum_Memory dispatched. WAL defragmentation running in background."), nil
}

// scatterProjectVacuum dispatches Vacuum_Memory to all running project member workspaces
// via Nexus and runs a local vacuum in background. [348.A]
func (t *DaemonTool) scatterProjectVacuum(ctx context.Context) (any, error) {
	if t.cfg == nil || t.cfg.Server.NexusDispatcherPort == 0 {
		return nil, fmt.Errorf("[348.A] nexus_dispatcher_port not configured; cannot scatter project vacuum")
	}
	nexusPort := t.cfg.Server.NexusDispatcherPort
	entries, err := fetchNexusWorkspaces(ctx, nexusPort)
	if err != nil {
		return nil, err
	}
	absWs, _ := filepath.Abs(t.workspace)
	type vacTarget struct{ id string }
	var targets []vacTarget
	for _, e := range entries {
		if e.Status != "running" {
			continue
		}
		if absEntry, _ := filepath.Abs(e.Path); absEntry == absWs {
			continue // skip self — runs local vacuum in background below
		}
		targets = append(targets, vacTarget{id: e.ID})
	}
	type vacResult struct {
		wsID string
		msg  string
	}
	results := make([]vacResult, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, tgt := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, id string) {
			defer func() { <-sem; wg.Done() }()
			msg, fwdErr := t.forwardVacuumToNexus(ctx, nexusPort, id)
			if fwdErr != nil {
				msg = fmt.Sprintf("❌ %s: %v", id, fwdErr)
			}
			results[idx] = vacResult{wsID: id, msg: msg}
		}(i, tgt.id)
	}
	wg.Wait()

	var sb strings.Builder
	fmt.Fprintf(&sb, "### 🧹 PROJECT VACUUM SCATTER — %d remote workspace(s)\n\n", len(targets))
	for _, r := range results {
		fmt.Fprintf(&sb, "**%s:** %s\n", r.wsID, r.msg)
	}
	// Local vacuum runs in background concurrently.
	go t.localVacuumBackground()
	if len(targets) == 0 {
		sb.WriteString("_No other running workspaces found._\n")
	}
	sb.WriteString("**[self]:** local Vacuum_Memory dispatched in background.\n")
	return mcpText(sb.String()), nil
}

// localVacuumBackground runs WAL defragmentation + session purge for this workspace. [348.A]
func (t *DaemonTool) localVacuumBackground() {
	var ignoreDirs []string
	ttlHours := 24
	cfgPath := filepath.Join(t.workspace, "neo.yaml")
	if c, errCfg := config.LoadConfig(cfgPath); errCfg == nil {
		ignoreDirs = c.Workspace.IgnoreDirs
		if c.SRE.SessionStateTTLHours > 0 {
			ttlHours = c.SRE.SessionStateTTLHours
		}
	}
	_, _ = t.wal.Vacuum(context.Background(), t.workspace, ignoreDirs)
	if err := t.wal.PurgeOldSessions(time.Duration(ttlHours) * time.Hour); err != nil {
		log.Printf("[SRE-348.A] PurgeOldSessions error: %v", err)
	}
}

// forwardVacuumToNexus POSTs a Vacuum_Memory scatter request to Nexus for a specific child. [348.A]
func (t *DaemonTool) forwardVacuumToNexus(ctx context.Context, nexusPort int, wsID string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/internal/vacuum/begin/%s", nexusPort, wsID) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is from NexusDispatcherPort config
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("build vacuum forward request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := sre.SafeInternalHTTPClient(20)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("nexus vacuum %s: %w", wsID, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("nexus vacuum %s: HTTP %d: %s", wsID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, _ := io.ReadAll(resp.Body)
	var nr struct {
		WorkspaceID string `json:"workspace_id"`
		Message     string `json:"message"`
	}
	if jsonErr := json.Unmarshal(body, &nr); jsonErr != nil {
		return string(body), nil
	}
	return nr.Message, nil
}

// fetchNexusWorkspaces retrieves all workspace statuses from the Nexus dispatcher. [348.A]
func fetchNexusWorkspaces(ctx context.Context, nexusPort int) ([]nexusChaosStatus, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/status", nexusPort) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: SafeInternalHTTPClient allows only loopback; port is from NexusDispatcherPort config
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build nexus status request: %w", err)
	}
	client := sre.SafeInternalHTTPClient(5)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nexus /status: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	var entries []nexusChaosStatus
	if jsonErr := json.Unmarshal(body, &entries); jsonErr != nil {
		return nil, fmt.Errorf("parse nexus /status: %w", jsonErr)
	}
	return entries, nil
}

func (t *DaemonTool) handleSetStage(_ context.Context, args map[string]any) (any, error) {
	stageVal, ok := args["stage"].(float64)
	if !ok {
		return nil, fmt.Errorf("stage integer is required for SetStage")
	}
	if err := state.AdvanceTo(state.CognitiveStage(uint32(stageVal))); err != nil {
		return nil, err
	}
	return mcpText(fmt.Sprintf("Cognitive stage advanced to %d.", int(stageVal))), nil
}

func (t *DaemonTool) handleFlushPMEM(_ context.Context, _ map[string]any) (any, error) {
	// [SRE-20.3.3] Inject FLUSH signal to /tmp/neo-control.sock
	telemetry.SendCommand("FLUSH", "")
	return mcpText("[SRE-DAEMON] FLUSH_PMEM signal dispatched to control socket."), nil
}

func (t *DaemonTool) handleQuarantineIP(_ context.Context, args map[string]any) (any, error) {
	ip, _ := args["target_ip"].(string)
	if ip == "" {
		return nil, fmt.Errorf("target_ip is required for QUARANTINE_IP")
	}
	// [SRE-20.3.3] Inject QUARANTINE signal with IP to /tmp/neo-control.sock
	telemetry.SendCommand("QUARANTINE", ip)
	return mcpText(fmt.Sprintf("[SRE-DAEMON] QUARANTINE_IP signal for %s dispatched.", ip)), nil
}

// [275.B] handleMarkDone marks one or more epics as done in .neo/master_plan.md.
// Finds all lines matching `- [ ] **{epic_id}` and flips them to `- [x]`.
// Allowed in all modes (pair/fast/daemon) — only edits the plan file, no system side-effects.
func (t *DaemonTool) handleMarkDone(_ context.Context, args map[string]any) (any, error) {
	epicID, _ := args["epic_id"].(string)
	if epicID == "" {
		return nil, fmt.Errorf("epic_id is required for MARK_DONE (e.g. \"272.G.1\")")
	}
	planPath := filepath.Join(t.workspace, ".neo", "master_plan.md")
	data, err := os.ReadFile(planPath) //nolint:gosec // G304-WORKSPACE-CANON: path via filepath.Join(t.workspace,...)
	if err != nil {
		return nil, fmt.Errorf("MARK_DONE: cannot read master_plan.md: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	prefix := "- [ ] **" + epicID
	marked := 0
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			lines[i] = "- [x]" + line[5:]
			marked++
		}
	}
	if marked == 0 {
		return mcpText(fmt.Sprintf("MARK_DONE: no open task found matching epic_id %q in master_plan.md", epicID)), nil
	}
	if err := os.WriteFile(planPath, []byte(strings.Join(lines, "\n")), 0644); err != nil { //nolint:gosec // G306: plan file is not sensitive
		return nil, fmt.Errorf("MARK_DONE: write failed: %w", err)
	}
	log.Printf("[MARK_DONE] Marked %d task(s) as done for epic_id=%q", marked, epicID)
	return mcpText(fmt.Sprintf("MARK_DONE: %d task(s) marked [x] for %q in master_plan.md", marked, epicID)), nil
}

// ExecuteNextResponse is the contract returned by the execute_next action.
// PILAR XXVII / 138.C.1.
//
// This is the daemon's iterative MCP-driven entry point: it claims the
// next task from the queue, resolves which backend should execute it,
// computes the trust score for the (pattern, scope) bucket, and emits
// a suggested_action that the operator can use to decide whether to
// auto-approve, prompt, or reject.
//
// The current implementation is a SKELETON — it pulls + classifies +
// suggests, but does not actually dispatch to DeepSeek nor run the
// audit pipeline. Those wires land in 138.C.2 (DeepSeek dispatch) and
// 138.C.3 (audit pipeline integration).
type ExecuteNextResponse struct {
	TaskID          string  `json:"task_id"`
	TaskDescription string  `json:"task_description,omitempty"`
	TargetFile      string  `json:"target_file,omitempty"`
	Backend         string  `json:"backend"`
	DeepSeekTool    string  `json:"deepseek_tool,omitempty"`
	Pattern         string  `json:"pattern"`
	Scope           string  `json:"scope"`
	TrustAlpha      float64 `json:"trust_alpha"`
	TrustBeta       float64 `json:"trust_beta"`
	TrustLowerBound float64 `json:"trust_lower_bound"`
	Tier            string  `json:"tier"`
	TotalExecutions int     `json:"total_executions"`
	SuggestedAction string  `json:"suggested_action"`

	// [138.F.3] DeepSeek dispatch result, populated when backend=deepseek.
	TokensUsed    int    `json:"tokens_used,omitempty"`
	OutputSummary string `json:"output_summary,omitempty"`
	DispatchError string `json:"dispatch_error,omitempty"`

	// PipelinePhase advertises which lifecycle stage produced this response.
	// Values: "dispatched" (deepseek call ok, audit pending),
	// "dispatch_failed" (deepseek call returned error),
	// "auditing" (138.G real audit pipeline ran),
	// "skeleton: backend=claude" (no dispatch path for claude yet),
	// "queue_empty" (no task pulled).
	PipelinePhase string `json:"pipeline_phase,omitempty"`
}

// handleExecuteNext implements the execute_next action [138.C.1].
//
// Flow (skeleton stage):
//  1. Pull next task from BoltDB (with optional agent_role filter)
//  2. Resolve backend via 132.F (deepseek vs claude vs explicit override)
//  3. Resolve (pattern, scope) via 138.B.5
//  4. Read current TrustScore — fresh prior if none exists
//  5. Compute suggested_action from tier alone (audit not yet wired)
//  6. Return composite ExecuteNextResponse
//
// The suggested_action defaults to "prompt-operator" until 138.C.5 lands
// the full tier+audit logic. Skeleton callers see the contract shape and
// can validate the wiring before the dispatch + audit steps activate.
func (t *DaemonTool) handleExecuteNext(ctx context.Context, args map[string]any) (any, error) {
	agentRole, _ := args["agent_role"].(string)

	task, err := state.GetNextTaskByRole(agentRole)
	if err != nil {
		return nil, fmt.Errorf("execute_next: pull task: %w", err)
	}
	if task == nil {
		return mcpText(`{"status":"queue_empty","message":"no tasks pending"}`), nil
	}

	_ = state.MarkTaskInProgress(task.ID)

	backendMode := "auto"
	if t.cfg != nil {
		backendMode = t.cfg.SRE.DaemonBackendMode
	}
	hasKey := os.Getenv("DEEPSEEK_API_KEY") != ""
	backend, deepseekTool := state.ResolveSuggestedBackend(task, backendMode, hasKey, false)

	pattern, scope := state.ResolvePatternScope(*task)

	trustScore, terr := state.TrustGet(pattern, scope)
	if terr != nil {
		log.Printf("[EXECUTE-NEXT] trust read fallback (%v) — using fresh prior", terr)
	}

	// [138.F.3] Dispatch to DeepSeek when the resolver picked it as backend.
	// On success we capture tokens + output_summary into the response and the
	// persisted DaemonResult; on failure we log and continue — the operator
	// still sees the suggested_action and decides retry vs reject.
	//
	// AuditPipeline wiring (build → AST → tests → certify-dry-run → metrics)
	// is deferred to follow-up 138.G — needs an AuditExecutor implementation
	// wrapping local radar/certify infra. For now Severity stays empty and
	// SuggestAction's empty-severity guard routes any tier to prompt-operator
	// (safe default).
	var (
		dispatchTokens  int
		dispatchOutput  string
		dispatchSummary string
		dispatchErr     error
	)
	if backend == "deepseek" && deepseekTool != "" && hasKey {
		dr, derr := dispatchToDeepSeek(ctx, t.workspace, *task, deepseekTool)
		if derr != nil {
			log.Printf("[EXECUTE-NEXT] deepseek dispatch failed task=%s tool=%s: %v", task.ID, deepseekTool, derr)
			dispatchErr = derr
		} else {
			dispatchTokens = dr.TokensUsed
			dispatchOutput = dr.Output
			dispatchSummary = summarizeDispatchOutput(dr.Output)
		}
	}

	stubAudit := state.AuditVerdict{}
	suggestedAction := string(state.SuggestAction(trustScore.CurrentTier, stubAudit))

	resp := ExecuteNextResponse{
		TaskID:          task.ID,
		TaskDescription: task.Description,
		TargetFile:      task.TargetFile,
		Backend:         backend,
		DeepSeekTool:    deepseekTool,
		Pattern:         pattern,
		Scope:           scope,
		TrustAlpha:      trustScore.Alpha,
		TrustBeta:       trustScore.Beta,
		TrustLowerBound: trustScore.LowerBound(time.Now()),
		Tier:            string(trustScore.CurrentTier),
		TotalExecutions: trustScore.TotalExecutions,
		SuggestedAction: suggestedAction,
		TokensUsed:      dispatchTokens,
		OutputSummary:   dispatchSummary,
		PipelinePhase:   computePipelinePhase(backend, dispatchTokens, dispatchErr),
	}
	if dispatchErr != nil {
		resp.DispatchError = dispatchErr.Error()
	}

	// [138.C.4] Persist DaemonResult so approve/reject can close the loop
	// later without re-deriving pattern/scope/backend.
	persistErr := state.PersistDaemonResult(state.DaemonResult{
		TaskID:           task.ID,
		TaskDescription:  task.Description,
		TargetFile:       task.TargetFile,
		Backend:          backend,
		DeepSeekTool:     deepseekTool,
		Pattern:          pattern,
		Scope:            scope,
		TrustAlphaBefore: trustScore.Alpha,
		TrustBetaBefore:  trustScore.Beta,
		TrustTierBefore:  string(trustScore.CurrentTier),
		AuditPassed:      false, // [138.G TODO] audit pipeline pending
		TokensUsed:       dispatchTokens,
		OutputSummary:    dispatchOutput,
		SuggestedAction:  suggestedAction,
		Status:           state.ResultPendingReview,
		CreatedAt:        time.Now(),
	})
	if persistErr != nil {
		log.Printf("[EXECUTE-NEXT] persist daemon_result failed (%v) — approve/reject will need fallback", persistErr)
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("execute_next: marshal: %w", err)
	}
	return mcpText(string(raw)), nil
}

// ApproveResponse is the contract returned by the approve action [138.C.2].
//
// approve closes the loop on a previously-suggested task: records the
// success outcome in the trust system, marks the task DONE, charges the
// session budget, and transitions the daemon_results entry to approved.
type ApproveResponse struct {
	TaskID         string  `json:"task_id"`
	Pattern        string  `json:"pattern"`
	Scope          string  `json:"scope"`
	TrustAlphaPost float64 `json:"trust_alpha_post"`
	TrustBetaPost  float64 `json:"trust_beta_post"`
	TierPost       string  `json:"tier_post"`
	OperatorNote   string  `json:"operator_note,omitempty"`
	Status         string  `json:"status"`
}

// handleApprove implements the approve action [138.C.2].
//
// Inputs:
//   task_id        — required; identifies the daemon_results entry to close
//   operator_note  — optional free-form note (audit trail)
//   session_id     — optional; if present, budget tokens are recorded
//
// Side effects:
//   1. TrustRecord(pattern, scope, OutcomeSuccess) — α += 1, streak reset
//   2. MarkTaskCompleted(task_id)                  — task → DONE
//   3. DaemonBudgetRecordTask(session_id, tokens)  — session token rollup
//   4. UpdateDaemonResult(task_id) → status=approved + operator_note
//
// Pure-Pair-mode caveat: this action is gated by the same daemon-only
// rule as the rest of neo_daemon. To exercise it in pair (after 138.E
// lands), the operator can flip NEO_SERVER_MODE=daemon temporarily.
func (t *DaemonTool) handleApprove(_ context.Context, args map[string]any) (any, error) {
	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return nil, fmt.Errorf("approve: task_id required")
	}
	operatorNote, _ := args["operator_note"].(string)
	sessionID, _ := args["session_id"].(string)

	result, err := state.GetDaemonResult(taskID)
	if err != nil {
		return nil, fmt.Errorf("approve: load daemon_result: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("approve: no daemon_result for task_id=%s — must run execute_next first", taskID)
	}
	if result.Status != state.ResultPendingReview {
		return nil, fmt.Errorf("approve: task_id=%s already in status=%s; cannot re-approve", taskID, result.Status)
	}

	// Side effects must succeed before transitioning daemon_result to
	// approved — otherwise we'd advertise "approved" while the trust
	// counter never moved or the task stayed TODO. The operator can
	// retry approve when these surface; daemon_result stays in
	// pending_review until everything succeeds. [DeepSeek VULN-005]
	//
	// Idempotency: TrustApplied flag prevents double-billing if a partial
	// failure leaves the daemon_result in pending_review and the operator
	// retries. [DeepSeek DOUBLE-PENALTY-RETRY]
	if !result.TrustApplied {
		if rerr := state.TrustRecord(result.Pattern, result.Scope, state.OutcomeSuccess); rerr != nil {
			return nil, fmt.Errorf("approve: trust record: %w", rerr)
		}
		if uerr := state.UpdateDaemonResult(taskID, func(r *state.DaemonResult) { r.TrustApplied = true }); uerr != nil {
			return nil, fmt.Errorf("approve: mark trust applied: %w", uerr)
		}
	}
	if merr := state.MarkTaskCompleted(taskID); merr != nil {
		return nil, fmt.Errorf("approve: mark task completed: %w", merr)
	}
	// Budget record is non-fatal: the rollup is observability, not
	// correctness. A flaky budget bucket shouldn't block approval.
	if sessionID != "" && result.TokensUsed > 0 {
		if berr := state.DaemonBudgetRecordTask(sessionID, result.TokensUsed); berr != nil {
			log.Printf("[APPROVE] budget record failed (%v) — non-fatal", berr)
		}
	}

	now := time.Now()
	if uerr := state.UpdateDaemonResult(taskID, func(r *state.DaemonResult) {
		r.Status = state.ResultApproved
		r.OperatorNote = operatorNote
		r.CompletedAt = &now
	}); uerr != nil {
		return nil, fmt.Errorf("approve: update daemon_result: %w", uerr)
	}

	postScore, _ := state.TrustGet(result.Pattern, result.Scope)
	resp := ApproveResponse{
		TaskID:         taskID,
		Pattern:        result.Pattern,
		Scope:          result.Scope,
		TrustAlphaPost: postScore.Alpha,
		TrustBetaPost:  postScore.Beta,
		TierPost:       string(postScore.CurrentTier),
		OperatorNote:   operatorNote,
		Status:         string(state.ResultApproved),
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("approve: marshal: %w", err)
	}
	return mcpText(string(raw)), nil
}

// RejectReasonKind classifies why the operator rejected the suggestion.
// Each kind has different trust + retry semantics so the system doesn't
// punish the model for issues that aren't its fault. [138.C.8]
type RejectReasonKind string

const (
	// RejectQuality — model output was wrong or hallucinated. Strongest
	// trust penalty (β += 5 via OutcomeOperatorOverride). Default when
	// reason_kind omitted (backward compat with original 138.C.3 schema).
	RejectQuality RejectReasonKind = "quality"
	// RejectTiming — model was correct but executed at the wrong moment
	// (TTL expired, dependency not ready). No trust penalty, re-queue
	// without retry++ — task gets a fresh chance.
	RejectTiming RejectReasonKind = "timing"
	// RejectScope — task is out of scope for this workspace; route to
	// project-level backlog instead. No trust penalty since the
	// classification was an operator decision, not a model failure.
	RejectScope RejectReasonKind = "scope"
)

// rejectMaxRetriesDefault is the cap when no daemon.task_max_retries
// config is wired. Tasks rejected with RejectQuality more than this
// many times become FailedPermanent. [138.C.3]
const rejectMaxRetriesDefault = 3

// RejectResponse is the contract returned by the reject action.
type RejectResponse struct {
	TaskID         string  `json:"task_id"`
	Pattern        string  `json:"pattern"`
	Scope          string  `json:"scope"`
	ReasonKind     string  `json:"reason_kind"`
	Reason         string  `json:"reason,omitempty"`
	Requeued       bool    `json:"requeued"`
	FailedPermanent bool   `json:"failed_permanent,omitempty"`
	TrustAlphaPost float64 `json:"trust_alpha_post"`
	TrustBetaPost  float64 `json:"trust_beta_post"`
	TierPost       string  `json:"tier_post"`
	Status         string  `json:"status"`
}

// handleReject implements the reject action [138.C.3 + C.8].
//
// Inputs:
//   task_id      — required
//   reason       — operator note explaining the rejection (audit trail)
//   reason_kind  — quality | timing | scope (default: quality)
//   requeue      — true (default) → re-queue if under max-retries
//
// Side effects per reason_kind:
//   quality → TrustRecord(OperatorOverride) [β += 5] · requeue if under
//             max-retries · FailedPermanent if exceeded
//   timing  → no trust update · requeue with Retries reset to 0 (the
//             model wasn't wrong, just early)
//   scope   → no trust update · no requeue (operator should re-route
//             to project backlog manually) · marks daemon_result rejected
func (t *DaemonTool) handleReject(_ context.Context, args map[string]any) (any, error) {
	taskID, reason, reasonKind, requeue, err := parseRejectArgs(args)
	if err != nil {
		return nil, err
	}

	result, err := loadPendingResultForReject(taskID)
	if err != nil {
		return nil, err
	}

	// Trust update only for RejectQuality, and only if not already applied
	// (idempotency guard against partial-failure retries —
	// [DeepSeek DOUBLE-PENALTY-RETRY]).
	if reasonKind == RejectQuality && !result.TrustApplied {
		if rerr := state.TrustRecord(result.Pattern, result.Scope, state.OutcomeOperatorOverride); rerr != nil {
			return nil, fmt.Errorf("reject: trust record: %w", rerr)
		}
		if uerr := state.UpdateDaemonResult(taskID, func(r *state.DaemonResult) { r.TrustApplied = true }); uerr != nil {
			return nil, fmt.Errorf("reject: mark trust applied: %w", uerr)
		}
	}

	requeued, failedPermanent, err := applyRejectRequeue(taskID, reasonKind, requeue, reason)
	if err != nil {
		return nil, err
	}

	// Quality reject + requeue=false: explicitly mark FailedPermanent so
	// the orphan scanner doesn't re-claim the task and create a reject-
	// loop where each cycle re-applies the trust penalty.
	// [DeepSeek QUALITY-DEAD-LETTER fix]
	if reasonKind == RejectQuality && !requeue {
		if mferr := state.MarkTaskFailedPermanent(taskID, fmt.Sprintf("operator reject:quality (no-requeue) — %s", reason)); mferr != nil {
			log.Printf("[REJECT] mark FailedPermanent failed (%v) — orphan scanner may re-queue", mferr)
		}
		failedPermanent = true
	}

	now := time.Now()
	if uerr := state.UpdateDaemonResult(taskID, func(r *state.DaemonResult) {
		r.Status = state.ResultRejected
		r.OperatorNote = fmt.Sprintf("[%s] %s", reasonKind, reason)
		r.CompletedAt = &now
	}); uerr != nil {
		return nil, fmt.Errorf("reject: update daemon_result: %w", uerr)
	}

	postScore, _ := state.TrustGet(result.Pattern, result.Scope)
	resp := RejectResponse{
		TaskID:          taskID,
		Pattern:         result.Pattern,
		Scope:           result.Scope,
		ReasonKind:      string(reasonKind),
		Reason:          reason,
		Requeued:        requeued,
		FailedPermanent: failedPermanent,
		TrustAlphaPost:  postScore.Alpha,
		TrustBetaPost:   postScore.Beta,
		TierPost:        string(postScore.CurrentTier),
		Status:          string(state.ResultRejected),
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("reject: marshal: %w", err)
	}
	return mcpText(string(raw)), nil
}

// parseRejectArgs validates and extracts the four reject inputs. Returns
// a typed reasonKind (default quality, error if outside the enum) and a
// requeue bool defaulting to true. [138.C.3 helper]
func parseRejectArgs(args map[string]any) (taskID, reason string, reasonKind RejectReasonKind, requeue bool, err error) {
	taskID, _ = args["task_id"].(string)
	if taskID == "" {
		return "", "", "", false, fmt.Errorf("reject: task_id required")
	}
	reason, _ = args["reason"].(string)

	reasonKind = RejectReasonKind(strings.TrimSpace(getString(args, "reason_kind")))
	if reasonKind == "" {
		reasonKind = RejectQuality
	}
	switch reasonKind {
	case RejectQuality, RejectTiming, RejectScope:
		// valid
	default:
		return "", "", "", false, fmt.Errorf("reject: invalid reason_kind=%q (want quality|timing|scope)", reasonKind)
	}

	requeue = true
	if v, ok := args["requeue"].(bool); ok {
		requeue = v
	}
	return taskID, reason, reasonKind, requeue, nil
}

// loadPendingResultForReject fetches the daemon_result for taskID and
// validates it's in pending_review status. Returns errors on missing
// or wrong-status entries with messages tailored to the reject flow.
func loadPendingResultForReject(taskID string) (*state.DaemonResult, error) {
	result, err := state.GetDaemonResult(taskID)
	if err != nil {
		return nil, fmt.Errorf("reject: load daemon_result: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("reject: no daemon_result for task_id=%s — must run execute_next first", taskID)
	}
	if result.Status != state.ResultPendingReview {
		return nil, fmt.Errorf("reject: task_id=%s already in status=%s; cannot re-reject", taskID, result.Status)
	}
	return result, nil
}

// applyRejectRequeue applies the per-reasonKind requeue policy:
//
//	quality → requeue with Retries++, FailedPermanent if maxRetries hit
//	timing  → requeue with Retries reset (not the model's fault)
//	scope   → no requeue (operator should re-route manually)
//
// requeue=false bypasses all of the above. [138.C.8 helper]
func applyRejectRequeue(taskID string, reasonKind RejectReasonKind, requeue bool, reason string) (requeued, failedPermanent bool, err error) {
	// scope rejects always short-circuit: the task belongs to another
	// workspace, so requeueing locally would just put it back in a queue
	// it shouldn't be in. requeue=true on scope is silently honored as
	// "no, don't requeue" — operator intent ("get this off my queue") is
	// preserved even if their flag was wrong.
	// [DeepSeek SCOPE-SILENT-OVERRIDE: documented, not erroring]
	if !requeue || reasonKind == RejectScope {
		return false, false, nil
	}
	skipRetryIncrement := reasonKind == RejectTiming
	errMsg := fmt.Sprintf("reject:%s — %s", reasonKind, reason)
	requeued, err = state.RequeueTaskOrFail(taskID, errMsg, rejectMaxRetriesDefault, skipRetryIncrement)
	if err != nil {
		return false, false, fmt.Errorf("reject: requeue: %w", err)
	}
	failedPermanent = !requeued && reasonKind == RejectQuality
	return requeued, failedPermanent, nil
}

// PairAuditEmitResponse is the contract returned by the pair_audit_emit
// action. Each emit creates exactly one event; the response surfaces
// the EventID for correlation. [138.E.1]
type PairAuditEmitResponse struct {
	EventID  string   `json:"event_id"`
	Scope    string   `json:"scope"`
	Severity int      `json:"severity"`
	Files    []string `json:"files,omitempty"`
}

// handlePairAuditEmit implements the pair_audit_emit action [138.E.1].
//
// Inputs:
//   scope       — required; "pattern:file_ext:dir_root" matching TrustScore.Key()
//   finding_id  — required; model-supplied finding identifier
//   claim_text  — short description (truncated at 240 chars by storage layer)
//   severity    — 1-10, model's self-rated severity
//   files       — array of file paths the finding references
//
// Read-only-ish: writes to pair_audit_events bucket but doesn't touch
// trust scores directly. The certify-time hook (138.E.2) handles the
// outcome inference. Exempted from pair-mode prohibition like
// trust_status — operators must be able to feed the loop while
// developing in pair.
func (t *DaemonTool) handlePairAuditEmit(_ context.Context, args map[string]any) (any, error) {
	scope := getString(args, "scope")
	if scope == "" {
		return nil, fmt.Errorf("pair_audit_emit: scope required")
	}
	findingID := getString(args, "finding_id")
	claimText := getString(args, "claim_text")

	// Severity: omit field → default 5; explicit value must be 1-10.
	// Distinguishing "not provided" from "explicit 0" surfaces a clear
	// error rather than letting 0 trip the storage-layer validation
	// with a less descriptive message. [DeepSeek VULN-004]
	severity := 5
	if v, ok := args["severity"].(float64); ok {
		if v < 1 || v > 10 {
			return nil, fmt.Errorf("pair_audit_emit: severity=%v out of [1,10] (omit field for default 5)", v)
		}
		severity = int(v)
	}

	// Files: every element must be a string. Silent drop on non-string
	// items would mask schema misuse and cause biased intersect checks
	// later in the certify hook. [DeepSeek VULN-005]
	var files []string
	if raw, ok := args["files"].([]any); ok {
		files = make([]string, 0, len(raw))
		for i, item := range raw {
			s, isStr := item.(string)
			if !isStr {
				return nil, fmt.Errorf("pair_audit_emit: files[%d] is %T, must be string", i, item)
			}
			if s != "" {
				files = append(files, s)
			}
		}
	}

	eventID, err := state.EmitPairAuditEvent(scope, findingID, claimText, severity, files)
	if err != nil {
		return nil, fmt.Errorf("pair_audit_emit: %w", err)
	}

	resp := PairAuditEmitResponse{
		EventID:  eventID,
		Scope:    scope,
		Severity: severity,
		Files:    files,
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("pair_audit_emit: marshal: %w", err)
	}
	return mcpText(string(raw)), nil
}

// getString safely extracts a string from args[key], returning "" when
// absent or wrong type. Avoids the inline `_, _ := args[k].(string)`
// noise across handler entry points.
func getString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// TrustStatusEntry is one row of the trust_status report. Per (pattern,
// scope) bucket: counters, current tier, lower bound (the metric that
// drives tier decisions), and last_update for freshness inspection.
type TrustStatusEntry struct {
	Pattern         string  `json:"pattern"`
	Scope           string  `json:"scope"`
	Alpha           float64 `json:"alpha"`
	Beta            float64 `json:"beta"`
	TotalExecutions int     `json:"total_executions"`
	Tier            string  `json:"tier"`
	LowerBound      float64 `json:"lower_bound"`
	PointEstimate   float64 `json:"point_estimate"`
	ManualWarmup    bool    `json:"manual_warmup,omitempty"`
	// LastUpdateUnix is omitted when LastUpdate is zero-value (never
	// recorded). Otherwise consumers parsing this would see -62135596800
	// (year 0001 in Unix seconds) and misinterpret it as a real timestamp.
	// [DeepSeek TRUST-STATUS-003]
	LastUpdateUnix int64 `json:"last_update_unix,omitempty"`
}

// TrustStatusResponse is the contract for the trust_status action [138.C.6].
//
// SkippedCorrupt surfaces the count of unmarshal failures from
// ListTrustScores so the operator sees data integrity issues — a
// non-zero count means the daemon_trust bucket has corrupt entries
// and an investigation is warranted (concurrent write race, on-disk
// corruption, schema drift). [DeepSeek TRUST-STATUS-001]
type TrustStatusResponse struct {
	TotalScores    int                `json:"total_scores"`
	Returned       int                `json:"returned"`
	SkippedCorrupt int                `json:"skipped_corrupt,omitempty"`
	FilterPattern  string             `json:"filter_pattern,omitempty"`
	Entries        []TrustStatusEntry `json:"entries"`
}

// trustStatusDefaultTop is the default top-N when caller omits the
// `top` field. Operators usually want the worst-performing or best-
// trusted patterns at a glance — 10 is enough to spot trends without
// drowning the response.
const trustStatusDefaultTop = 10

// handleTrustStatus implements the trust_status action [138.C.6].
//
// Inputs:
//   filter_pattern  — optional substring; only scores whose Pattern
//                     contains this string are returned (case-insensitive)
//   top             — max entries to return, default 10
//
// Output: scores sorted by LowerBound DESC (most-trusted first), so
// the operator sees which patterns are closest to auto-approval and
// which are languishing in L0.
func (t *DaemonTool) handleTrustStatus(_ context.Context, args map[string]any) (any, error) {
	filterPattern := strings.ToLower(getString(args, "filter_pattern"))
	// top semantics: omitted → trustStatusDefaultTop (10) · explicit 0 →
	// no limit (return everything) · positive int → cap at N. Distinguishing
	// "not set" from "explicitly 0" requires checking presence first.
	// [DeepSeek TRUST-STATUS-002]
	top := trustStatusDefaultTop
	if v, ok := args["top"].(float64); ok {
		if v == 0 {
			top = 0 // explicit "no limit"
		} else if v > 0 {
			top = int(v)
		}
	}

	all, skipped, err := state.ListTrustScores()
	if err != nil {
		return nil, fmt.Errorf("trust_status: list scores: %w", err)
	}

	now := time.Now()
	filtered := filterAndProjectTrustScores(all, filterPattern, now)
	sortTrustEntriesByLowerBound(filtered)

	if top > 0 && len(filtered) > top {
		filtered = filtered[:top]
	}

	resp := TrustStatusResponse{
		TotalScores:    len(all),
		Returned:       len(filtered),
		SkippedCorrupt: skipped,
		FilterPattern:  filterPattern,
		Entries:        filtered,
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("trust_status: marshal: %w", err)
	}
	return mcpText(string(raw)), nil
}

// filterAndProjectTrustScores converts raw TrustScore records to
// TrustStatusEntry projections, applying the optional pattern filter.
// Computing PointEstimate + LowerBound here means the response shows
// decay-aware values (not the raw stored α/β).
func filterAndProjectTrustScores(scores []state.TrustScore, filterPattern string, now time.Time) []TrustStatusEntry {
	out := make([]TrustStatusEntry, 0, len(scores))
	for _, s := range scores {
		if filterPattern != "" && !strings.Contains(strings.ToLower(s.Pattern), filterPattern) {
			continue
		}
		var lastUpdateUnix int64
		if !s.LastUpdate.IsZero() {
			lastUpdateUnix = s.LastUpdate.Unix()
		}
		out = append(out, TrustStatusEntry{
			Pattern:         s.Pattern,
			Scope:           s.Scope,
			Alpha:           s.Alpha,
			Beta:            s.Beta,
			TotalExecutions: s.TotalExecutions,
			Tier:            string(s.CurrentTier),
			LowerBound:      s.LowerBound(now),
			PointEstimate:   s.PointEstimate(now),
			ManualWarmup:    s.ManualWarmup,
			LastUpdateUnix:  lastUpdateUnix,
		})
	}
	return out
}

// sortTrustEntriesByLowerBound sorts in place by LowerBound DESC. Ties
// broken by TotalExecutions DESC (more evidence first when tier is
// borderline).
func sortTrustEntriesByLowerBound(entries []TrustStatusEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].LowerBound != entries[j].LowerBound {
			return entries[i].LowerBound > entries[j].LowerBound
		}
		return entries[i].TotalExecutions > entries[j].TotalExecutions
	})
}

// pairAuditReaperInterval is how often the background goroutine
// scans for stale unresolved PairAuditEvents. 5 minutes is the
// design-doc cadence — frequent enough that operators see drift
// quickly, infrequent enough that bucket scans don't chew CPU.
const pairAuditReaperInterval = 5 * time.Minute

// runPairAuditReaper is the background goroutine [138.E.3]. Lives
// for the process lifetime; intentional leak since neo-mcp is
// long-running. Each tick calls state.ReapStalePairAuditEvents()
// which marks events past PairAuditEventTTL as OutcomeSuccess.
//
// Errors from the reaper are logged but never abort the loop —
// transient bbolt or DB issues should not kill a background helper
// that's not on a critical path.
func runPairAuditReaper() {
	ticker := time.NewTicker(pairAuditReaperInterval)
	defer ticker.Stop()
	for range ticker.C {
		if _, err := state.ReapStalePairAuditEvents(); err != nil {
			log.Printf("[PAIR-FEEDBACK] reaper tick failed: %v", err)
		}
	}
}
