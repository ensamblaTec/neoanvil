// cmd/neo-mcp/daemon_execute_next_test.go — tests for execute_next action [138.C.1]
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/state"
)

// setupExecuteNextPlanner bootstraps a fresh planner DB in a temp dir.
// No explicit close: the next InitPlanner call replaces the global handle
// and the temp dir cleanup runs after; the dangling handle is reaped by
// GC. Mirrors the looseness already in pkg/state's package-private helper.
func setupExecuteNextPlanner(t *testing.T) {
	t.Helper()
	if err := state.InitPlanner(t.TempDir()); err != nil {
		t.Fatalf("InitPlanner: %v", err)
	}
}

// extractMcpText pulls the JSON payload out of an mcpText() response so
// tests can assert on the wire shape.
func extractMcpText(t *testing.T, resp any) string {
	t.Helper()
	m, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("response is not map: %T", resp)
	}
	content, ok := m["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("response missing content: %+v", m)
	}
	text, _ := content[0]["text"].(string)
	return text
}

// TestExecuteNext_QueueEmpty — no pending tasks → "queue_empty" status.
// Daemon has nothing to do; returns gracefully without error.
func TestExecuteNext_QueueEmpty(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())

	resp, err := tool.handleExecuteNext(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("handleExecuteNext: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"status":"queue_empty"`) {
		t.Errorf("expected queue_empty status, got %q", body)
	}
}

// TestExecuteNext_PullsTaskAndResolvesPattern — push a refactor task,
// execute_next should return the full ExecuteNextResponse contract with
// pattern="refactor", scope=".go:pkg/state", trust at fresh prior.
func TestExecuteNext_PullsTaskAndResolvesPattern(t *testing.T) {
	setupExecuteNextPlanner(t)

	if err := state.EnqueueTasks([]state.SRETask{{
		ID:          "t1",
		Description: "refactor logger en pkg/state/planner.go",
		TargetFile:  "pkg/state/planner.go",
	}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	tool := NewDaemonTool(nil, t.TempDir())
	resp, err := tool.handleExecuteNext(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("handleExecuteNext: %v", err)
	}
	body := extractMcpText(t, resp)

	var got ExecuteNextResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if got.Pattern != "refactor" {
		t.Errorf("pattern=%q, want refactor", got.Pattern)
	}
	if got.Scope != ".go:pkg/state" {
		t.Errorf("scope=%q, want .go:pkg/state", got.Scope)
	}
	// Fresh prior: α=1, β=1, no executions yet.
	if got.TrustAlpha != 1 || got.TrustBeta != 1 {
		t.Errorf("fresh prior: want α=1 β=1, got α=%v β=%v", got.TrustAlpha, got.TrustBeta)
	}
	if got.Tier != "L0" {
		t.Errorf("tier=%q, want L0 (new pattern)", got.Tier)
	}
	if got.SuggestedAction != "prompt-operator" {
		t.Errorf("suggested_action=%q, want prompt-operator (skeleton default)", got.SuggestedAction)
	}
	// [138.F.3] SkeletonStatus replaced by PipelinePhase. With backend=claude
	// (DEEPSEEK_API_KEY unset in tests) we expect the claude-skeleton label.
	// dispatched/dispatch_failed apply only when backend=deepseek.
	if got.Backend == "claude" && got.PipelinePhase != "skeleton: backend=claude" {
		t.Errorf("pipeline_phase=%q, want %q for claude backend", got.PipelinePhase, "skeleton: backend=claude")
	}
}

// TestExecuteNext_AgentRoleFilter — agent_role filter is honored.
// Push two tasks (frontend + backend), claim with agent_role=backend,
// verify the backend one comes back.
func TestExecuteNext_AgentRoleFilter(t *testing.T) {
	setupExecuteNextPlanner(t)

	if err := state.EnqueueTasks([]state.SRETask{
		{Description: "audit React components", TargetFile: "web/src/App.tsx", Role: "frontend"},
		{Description: "audit Go handlers", TargetFile: "cmd/neo-mcp/main.go", Role: "backend"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	tool := NewDaemonTool(nil, t.TempDir())
	resp, err := tool.handleExecuteNext(context.Background(), map[string]any{
		"agent_role": "backend",
	})
	if err != nil {
		t.Fatalf("handleExecuteNext: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"target_file":"cmd/neo-mcp/main.go"`) {
		t.Errorf("agent_role=backend should claim the backend task, got %q", body)
	}
}

// TestApprove_FullCycle — happy path: execute_next seeds the daemon_result
// in pending_review; approve transitions to approved, calls TrustRecord
// with OutcomeSuccess (α += 1), MarkTaskCompleted, and persists the
// operator note. Verifies the trust score moves and the bucket entry
// is updated. [138.C.2]
func assertDaemonResultStatus(t *testing.T, taskID string, wantStatus state.DaemonResultStatus) *state.DaemonResult {
	t.Helper()
	res, err := state.GetDaemonResult(taskID)
	if err != nil || res == nil {
		t.Fatalf("daemon_result should exist for %s: err=%v res=%v", taskID, err, res)
	}
	if res.Status != wantStatus {
		t.Fatalf("daemon_result %s: Status=%q, want %q", taskID, res.Status, wantStatus)
	}
	return res
}

func assertTaskDone(t *testing.T, taskID string) {
	t.Helper()
	for _, task := range state.GetAllTasks() {
		if task.ID == taskID && task.Status != "DONE" {
			t.Errorf("task %s Status=%q, want DONE", taskID, task.Status)
		}
	}
}

func assertTrustAlphaIncreasedBy1(t *testing.T, pattern, scope string, preAlpha float64) {
	t.Helper()
	postTrust, _ := state.TrustGet(pattern, scope)
	if postTrust.Alpha != preAlpha+1 {
		t.Errorf("trust α post=%v, want pre+1=%v", postTrust.Alpha, preAlpha+1)
	}
}

func TestApprove_FullCycle(t *testing.T) {
	setupExecuteNextPlanner(t)

	if err := state.EnqueueTasks([]state.SRETask{{
		Description: "refactor logger en pkg/state/planner.go",
		TargetFile:  "pkg/state/planner.go",
	}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	tool := NewDaemonTool(nil, t.TempDir())

	// Step 1: execute_next seeds the bucket.
	if _, err := tool.handleExecuteNext(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("handleExecuteNext: %v", err)
	}

	pre := assertDaemonResultStatus(t, "TASK-001", state.ResultPendingReview)
	preTrust, _ := state.TrustGet(pre.Pattern, pre.Scope)
	preAlpha := preTrust.Alpha

	// Step 2: approve transitions the entry, records success, marks done.
	resp, err := tool.handleApprove(context.Background(), map[string]any{
		"task_id":       "TASK-001",
		"operator_note": "verified manually",
	})
	if err != nil {
		t.Fatalf("handleApprove: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"status":"approved"`) {
		t.Errorf("approve response missing status=approved: %s", body)
	}

	post, _ := state.GetDaemonResult("TASK-001")
	if post.Status != state.ResultApproved {
		t.Errorf("post.Status=%q, want approved", post.Status)
	}
	if post.OperatorNote != "verified manually" {
		t.Errorf("OperatorNote=%q, want \"verified manually\"", post.OperatorNote)
	}
	if post.CompletedAt == nil || post.CompletedAt.IsZero() {
		t.Error("CompletedAt should be set on approve")
	}
	assertTrustAlphaIncreasedBy1(t, pre.Pattern, pre.Scope, preAlpha)
	assertTaskDone(t, "TASK-001")
}

// TestApprove_RequiresTaskID — empty task_id is rejected at the entry
// point, before any state mutation.
func TestApprove_RequiresTaskID(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())
	if _, err := tool.handleApprove(context.Background(), map[string]any{}); err == nil {
		t.Error("approve without task_id should error")
	}
}

// TestApprove_RejectsMissingResult — approving a task that was never
// run through execute_next surfaces a clear error rather than silently
// auto-creating a daemon_result entry.
func TestApprove_RejectsMissingResult(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())
	_, err := tool.handleApprove(context.Background(), map[string]any{
		"task_id": "TASK-NEVER-EXECUTED",
	})
	if err == nil {
		t.Error("approve on never-executed task should error")
	}
}

// TestApprove_DoubleApproveErrors — once approved, a second approve
// rejects with "already in status=approved". Defends against operator
// double-clicks corrupting the trust counter.
func TestApprove_DoubleApproveErrors(t *testing.T) {
	setupExecuteNextPlanner(t)
	if err := state.EnqueueTasks([]state.SRETask{{
		Description: "audit pkg/state/planner.go",
		TargetFile:  "pkg/state/planner.go",
	}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	tool := NewDaemonTool(nil, t.TempDir())
	if _, err := tool.handleExecuteNext(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("execute_next: %v", err)
	}
	if _, err := tool.handleApprove(context.Background(), map[string]any{"task_id": "TASK-001"}); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if _, err := tool.handleApprove(context.Background(), map[string]any{"task_id": "TASK-001"}); err == nil {
		t.Error("second approve on same task should error")
	}
}

// rejectFlowSetup is shared boilerplate: planner + one queued task +
// execute_next so the daemon_result is in pending_review.
func rejectFlowSetup(t *testing.T) *DaemonTool {
	t.Helper()
	setupExecuteNextPlanner(t)
	if err := state.EnqueueTasks([]state.SRETask{{
		Description: "refactor pkg/state/planner.go",
		TargetFile:  "pkg/state/planner.go",
	}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	tool := NewDaemonTool(nil, t.TempDir())
	if _, err := tool.handleExecuteNext(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("execute_next: %v", err)
	}
	return tool
}

// TestReject_QualityRequeues — quality reason: TrustRecord(OperatorOverride)
// fires (β += 5), task re-queued with Retries=1, daemon_result transitions
// to rejected. [138.C.3+C.8]
func TestReject_QualityRequeues(t *testing.T) {
	tool := rejectFlowSetup(t)

	pre, _ := state.GetDaemonResult("TASK-001")
	preBeta, _ := func() (float64, error) {
		s, e := state.TrustGet(pre.Pattern, pre.Scope)
		return s.Beta, e
	}()

	resp, err := tool.handleReject(context.Background(), map[string]any{
		"task_id":     "TASK-001",
		"reason":      "output hallucinated a non-existent function",
		"reason_kind": "quality",
		"requeue":     true,
	})
	if err != nil {
		t.Fatalf("handleReject: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"requeued":true`) {
		t.Errorf("expected requeued:true, got %s", body)
	}

	// β increased by 5 (OperatorOverride weight).
	postScore, _ := state.TrustGet(pre.Pattern, pre.Scope)
	if postScore.Beta != preBeta+5 {
		t.Errorf("β post=%v, want pre+5=%v", postScore.Beta, preBeta+5)
	}

	// Task re-queued: Status back to TODO, Retries=1.
	for _, task := range state.GetAllTasks() {
		if task.ID == "TASK-001" {
			if task.Status != "TODO" {
				t.Errorf("Status=%q, want TODO (re-queued)", task.Status)
			}
			if task.Retries != 1 {
				t.Errorf("Retries=%d, want 1", task.Retries)
			}
		}
	}
}

// TestReject_TimingDoesNotPenalize — timing reason: no β change, task
// re-queued with Retries=0 (the model wasn't wrong, just early). [138.C.8]
func TestReject_TimingDoesNotPenalize(t *testing.T) {
	tool := rejectFlowSetup(t)

	pre, _ := state.GetDaemonResult("TASK-001")
	preScore, _ := state.TrustGet(pre.Pattern, pre.Scope)

	if _, err := tool.handleReject(context.Background(), map[string]any{
		"task_id":     "TASK-001",
		"reason":      "TTL expired before merge",
		"reason_kind": "timing",
		"requeue":     true,
	}); err != nil {
		t.Fatalf("handleReject: %v", err)
	}

	// Trust untouched.
	postScore, _ := state.TrustGet(pre.Pattern, pre.Scope)
	if postScore.Alpha != preScore.Alpha || postScore.Beta != preScore.Beta {
		t.Errorf("timing should not change trust, got α=%v β=%v (pre α=%v β=%v)",
			postScore.Alpha, postScore.Beta, preScore.Alpha, preScore.Beta)
	}

	// Re-queued WITHOUT incrementing Retries — timing rejects don't burn
	// a retry slot since the model wasn't wrong, just early.
	// [DeepSeek RESET-RETRIES fix: skipRetryIncrement=true]
	for _, task := range state.GetAllTasks() {
		if task.ID == "TASK-001" && task.Retries != 0 {
			t.Errorf("Retries=%d after timing reject (expect 0: no increment for timing)", task.Retries)
		}
	}
}

// TestReject_ScopeNeverRequeues — scope reason: no β change, task NOT
// re-queued (operator should manually route to project backlog). [138.C.8]
func TestReject_ScopeNeverRequeues(t *testing.T) {
	tool := rejectFlowSetup(t)

	pre, _ := state.GetDaemonResult("TASK-001")
	preScore, _ := state.TrustGet(pre.Pattern, pre.Scope)

	resp, err := tool.handleReject(context.Background(), map[string]any{
		"task_id":     "TASK-001",
		"reason":      "belongs to strategosia, not neoanvil",
		"reason_kind": "scope",
	})
	if err != nil {
		t.Fatalf("handleReject: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"requeued":false`) {
		t.Errorf("expected requeued:false for scope, got %s", body)
	}

	// Trust untouched.
	postScore, _ := state.TrustGet(pre.Pattern, pre.Scope)
	if postScore.Alpha != preScore.Alpha || postScore.Beta != preScore.Beta {
		t.Errorf("scope should not change trust, got α=%v β=%v", postScore.Alpha, postScore.Beta)
	}
}

// TestReject_QualityHitsMaxRetries — after rejectMaxRetriesDefault
// quality rejections, the task transitions to FailedPermanent and does
// NOT re-queue. [138.C.3]
func TestReject_QualityHitsMaxRetries(t *testing.T) {
	setupExecuteNextPlanner(t)
	if err := state.EnqueueTasks([]state.SRETask{{
		Description: "refactor pkg/state/planner.go",
		TargetFile:  "pkg/state/planner.go",
	}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	tool := NewDaemonTool(nil, t.TempDir())

	// Loop: execute_next + reject(quality) for rejectMaxRetriesDefault+1 cycles.
	// Each cycle increments Retries until FailedPermanent fires.
	for i := range 4 { // rejectMaxRetriesDefault is 3, so 4 cycles trips it
		if _, err := tool.handleExecuteNext(context.Background(), map[string]any{}); err != nil {
			// On the FailedPermanent cycle the queue should be empty for this task.
			if i == 3 {
				break // expected: queue exhausted after FailedPermanent
			}
			t.Fatalf("execute_next cycle %d: %v", i, err)
		}
		resp, err := tool.handleReject(context.Background(), map[string]any{
			"task_id":     "TASK-001",
			"reason":      "wrong output again",
			"reason_kind": "quality",
		})
		if err != nil {
			t.Fatalf("reject cycle %d: %v", i, err)
		}
		body := extractMcpText(t, resp)
		if i < 3 && !strings.Contains(body, `"requeued":true`) {
			t.Errorf("cycle %d should requeue, got %s", i, body)
		}
		if i == 3 {
			if !strings.Contains(body, `"failed_permanent":true`) {
				t.Errorf("cycle 3 should hit FailedPermanent, got %s", body)
			}
		}
	}
}

// TestReject_RequiresTaskID — empty task_id rejected at entry.
func TestReject_RequiresTaskID(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())
	if _, err := tool.handleReject(context.Background(), map[string]any{}); err == nil {
		t.Error("reject without task_id should error")
	}
}

// TestReject_InvalidReasonKindErrors — reason_kind outside the enum
// is rejected before any side effects.
func TestReject_InvalidReasonKindErrors(t *testing.T) {
	tool := rejectFlowSetup(t)
	_, err := tool.handleReject(context.Background(), map[string]any{
		"task_id":     "TASK-001",
		"reason_kind": "made-up-kind",
	})
	if err == nil {
		t.Error("invalid reason_kind should error")
	}
}

// TestReject_DefaultsToQuality — omitting reason_kind defaults to
// quality (β += 5). Backward compat with the original 138.C.3 schema.
func TestReject_DefaultsToQuality(t *testing.T) {
	tool := rejectFlowSetup(t)
	pre, _ := state.GetDaemonResult("TASK-001")
	preScore, _ := state.TrustGet(pre.Pattern, pre.Scope)

	if _, err := tool.handleReject(context.Background(), map[string]any{
		"task_id": "TASK-001",
		"reason":  "default reason_kind path",
	}); err != nil {
		t.Fatalf("handleReject: %v", err)
	}

	postScore, _ := state.TrustGet(pre.Pattern, pre.Scope)
	if postScore.Beta != preScore.Beta+5 {
		t.Errorf("default kind should be quality (β+=5), got β=%v (pre=%v)", postScore.Beta, preScore.Beta)
	}
}

// TestReject_QualityNoRequeueMarksFailedPermanent — quality reject with
// requeue=false explicitly marks the task FailedPermanent so the orphan
// scanner won't re-claim it and create a reject-loop where each cycle
// re-applies the β=5 trust penalty. [DeepSeek QUALITY-DEAD-LETTER fix]
func TestReject_QualityNoRequeueMarksFailedPermanent(t *testing.T) {
	tool := rejectFlowSetup(t)

	resp, err := tool.handleReject(context.Background(), map[string]any{
		"task_id":     "TASK-001",
		"reason":      "permanent quality issue, kill it",
		"reason_kind": "quality",
		"requeue":     false,
	})
	if err != nil {
		t.Fatalf("handleReject: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"failed_permanent":true`) {
		t.Errorf("expected failed_permanent:true, got %s", body)
	}

	// Task should be in FailedPermanent lifecycle.
	for _, task := range state.GetAllTasks() {
		if task.ID == "TASK-001" && task.LifecycleState != state.TaskLifecycleFailedPermanent {
			t.Errorf("LifecycleState=%q, want %q", task.LifecycleState, state.TaskLifecycleFailedPermanent)
		}
	}
}

// TestTrustStatus_EmptyReturnsZero — no trust data yet, response shows
// total=0, no entries. Operator sees "nothing to report" cleanly.
// [138.C.6]
func TestTrustStatus_EmptyReturnsZero(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())

	resp, err := tool.handleTrustStatus(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("handleTrustStatus: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"total_scores":0`) {
		t.Errorf("expected total_scores:0, got %s", body)
	}
}

// TestTrustStatus_SortsByLowerBoundDesc — patterns with strong evidence
// rank above patterns with weak evidence. Operator scanning the report
// sees most-trusted patterns at the top.
func TestTrustStatus_SortsByLowerBoundDesc(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())

	// Seed three scores with very different α/β profiles:
	//   high-trust: α=200, β=10  → LB ≈ 0.91
	//   med-trust:  α=20,  β=10  → LB ≈ 0.49
	//   low-trust:  α=2,   β=2   → LB ≈ 0.16 (lots of variance)
	if err := state.TrustWarmup("refactor", ".go:pkg/state", 200, 10); err != nil {
		t.Fatalf("warmup high: %v", err)
	}
	if err := state.TrustWarmup("audit", ".go:pkg/sre", 20, 10); err != nil {
		t.Fatalf("warmup med: %v", err)
	}
	if err := state.TrustUpdate("distill", ".md:docs", func(s *state.TrustScore) {
		s.Alpha = 2
		s.Beta = 2
		s.LastUpdate = time.Now()
	}); err != nil {
		t.Fatalf("seed low: %v", err)
	}

	resp, err := tool.handleTrustStatus(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("handleTrustStatus: %v", err)
	}
	body := extractMcpText(t, resp)

	// First entry should be refactor (highest LB), last should be distill.
	highIdx := strings.Index(body, `"pattern":"refactor"`)
	medIdx := strings.Index(body, `"pattern":"audit"`)
	lowIdx := strings.Index(body, `"pattern":"distill"`)
	if highIdx < 0 || medIdx < 0 || lowIdx < 0 {
		t.Fatalf("missing entries in response: %s", body)
	}
	if !(highIdx < medIdx && medIdx < lowIdx) {
		t.Errorf("expected order refactor < audit < distill (by LB DESC), got positions %d, %d, %d",
			highIdx, medIdx, lowIdx)
	}
}

// TestTrustStatus_FilterPattern — substring filter on pattern name.
// Case-insensitive. Used to narrow the report to one keyword family.
func TestTrustStatus_FilterPattern(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())

	if err := state.TrustWarmup("refactor", ".go:pkg/state", 100, 10); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	if err := state.TrustWarmup("audit", ".go:pkg/sre", 100, 10); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	resp, err := tool.handleTrustStatus(context.Background(), map[string]any{
		"filter_pattern": "REFAC", // case-insensitive
	})
	if err != nil {
		t.Fatalf("handleTrustStatus: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"pattern":"refactor"`) {
		t.Errorf("filter should include refactor, body=%s", body)
	}
	if strings.Contains(body, `"pattern":"audit"`) {
		t.Errorf("filter should exclude audit, body=%s", body)
	}
	if !strings.Contains(body, `"returned":1`) {
		t.Errorf("expected returned:1, got %s", body)
	}
	// total_scores still reports the whole bucket size.
	if !strings.Contains(body, `"total_scores":2`) {
		t.Errorf("expected total_scores:2 (filter doesn't shrink total), got %s", body)
	}
}

// TestTrustStatus_TopZeroMeansNoLimit — explicit top:0 returns ALL
// scores, not the default 10. Operator must be able to export the full
// trust state for audits. [DeepSeek TRUST-STATUS-002]
func TestTrustStatus_TopZeroMeansNoLimit(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())

	// Seed 12 scores — more than the default top of 10.
	for _, p := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		if err := state.TrustWarmup(p, ".go:pkg/state", 100, 10); err != nil {
			t.Fatalf("warmup %s: %v", p, err)
		}
	}

	resp, err := tool.handleTrustStatus(context.Background(), map[string]any{
		"top": float64(0), // explicit "no limit"
	})
	if err != nil {
		t.Fatalf("handleTrustStatus: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"returned":12`) {
		t.Errorf("top:0 should return all 12, got %s", body)
	}
}

// TestCompactTrustSegment_EmptyWhenNoHighlights — fresh workspace
// has no trust highlights, segment is empty (keeps the briefing
// line clean). [138.E.4]
func TestCompactTrustSegment_EmptyWhenNoHighlights(t *testing.T) {
	d := &briefingData{}
	if got := compactTrustSegment(d); got != "" {
		t.Errorf("empty highlights: got %q, want empty", got)
	}
}

// TestCompactTrustSegment_RendersHighlights — three highlights produce
// the canonical " | trust: pattern:scope=tier(α=N β=M) | ..." string.
func TestCompactTrustSegment_RendersHighlights(t *testing.T) {
	d := &briefingData{
		trustHighlights: []TrustStatusEntry{
			{Pattern: "refactor", Scope: ".go:pkg/state", Tier: "L1", Alpha: 12, Beta: 4},
			{Pattern: "distill", Scope: ".md:docs", Tier: "L2", Alpha: 45, Beta: 3},
		},
	}
	got := compactTrustSegment(d)
	if !strings.Contains(got, "trust:") {
		t.Errorf("missing 'trust:' header, got %q", got)
	}
	if !strings.Contains(got, "refactor:.go:pkg/state=L1(α=12 β=4)") {
		t.Errorf("missing refactor entry, got %q", got)
	}
	if !strings.Contains(got, "distill:.md:docs=L2(α=45 β=3)") {
		t.Errorf("missing distill entry, got %q", got)
	}
}

// TestPopulateTrustHighlights_FiltersFreshPriors — entries with
// total_executions=0 (the unknown:unknown migration seed, freshly-
// created scopes) are filtered out so they don't clutter the compact
// line. Only patterns with real evidence appear. [138.E.4]
func TestPopulateTrustHighlights_FiltersFreshPriors(t *testing.T) {
	setupExecuteNextPlanner(t)

	// Seed three trust scores: two with evidence, one fresh prior.
	if err := state.TrustWarmup("refactor", ".go:pkg/state", 50, 5); err != nil {
		t.Fatalf("warmup 1: %v", err)
	}
	if err := state.TrustWarmup("distill", ".md:docs", 100, 2); err != nil {
		t.Fatalf("warmup 2: %v", err)
	}
	// Fresh-prior entry — TrustGet auto-creates with (1,1) and
	// TotalExecutions=0. We get it by simply asking.
	if _, err := state.TrustGet("audit", ".go:pkg/sre"); err != nil {
		t.Fatalf("trust get: %v", err)
	}

	highlights := populateTrustHighlights()
	// audit:.go:pkg/sre never got persisted (TrustGet without write),
	// so it shouldn't show up in ListTrustScores. Just refactor and
	// distill should appear.
	if len(highlights) > 3 {
		t.Errorf("got %d highlights, max should be 3", len(highlights))
	}
	for _, h := range highlights {
		if h.TotalExecutions == 0 {
			t.Errorf("highlight has 0 executions (should be filtered): %+v", h)
		}
	}
}

// TestPairAuditEmit_PersistsEvent — agent emits one finding; the
// event lands in pair_audit_events with severity, scope, files
// preserved. Verifies the wire from the action down to BoltDB.
// [138.E.1]
func TestPairAuditEmit_PersistsEvent(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())

	resp, err := tool.handlePairAuditEmit(context.Background(), map[string]any{
		"scope":       "refactor:.go:pkg/state",
		"finding_id":  "TRUST-LOGIC-001",
		"claim_text":  "empty severity should route to prompt-operator",
		"severity":    float64(8),
		"files":       []any{"pkg/state/daemon_trust.go"},
	})
	if err != nil {
		t.Fatalf("handlePairAuditEmit: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"event_id":"evt-`) {
		t.Errorf("response missing event_id: %s", body)
	}
	if !strings.Contains(body, `"severity":8`) {
		t.Errorf("response missing severity, got %s", body)
	}

	// Verify it's in the bucket and unresolved.
	events, _, err := state.ListUnresolvedPairEvents()
	if err != nil {
		t.Fatalf("ListUnresolvedPairEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Scope != "refactor:.go:pkg/state" {
		t.Errorf("Scope=%q", events[0].Scope)
	}
	if events[0].Severity != 8 {
		t.Errorf("Severity=%d", events[0].Severity)
	}
}

// TestPairAuditEmit_RequiresScope — empty scope is rejected at the
// handler entry point.
func TestPairAuditEmit_RequiresScope(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())
	if _, err := tool.handlePairAuditEmit(context.Background(), map[string]any{
		"finding_id": "X",
	}); err == nil {
		t.Error("emit without scope should error")
	}
}

// TestPairAuditEmit_RejectsExplicitZeroSeverity — explicit severity:0
// surfaces a clear handler-level error instead of slipping through to
// the storage-layer "out of [1,10]" message. [DeepSeek VULN-004]
func TestPairAuditEmit_RejectsExplicitZeroSeverity(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())
	_, err := tool.handlePairAuditEmit(context.Background(), map[string]any{
		"scope":      "scope",
		"finding_id": "X",
		"severity":   float64(0),
	})
	if err == nil {
		t.Error("explicit severity:0 should error at handler level")
	}
	if err != nil && !strings.Contains(err.Error(), "out of [1,10]") {
		t.Errorf("error message should mention [1,10] range, got: %v", err)
	}
}

// TestPairAuditEmit_RejectsNonStringFiles — non-string array element
// surfaces a clear error rather than being silently dropped (which
// would bias the certify-time intersect check). [DeepSeek VULN-005]
func TestPairAuditEmit_RejectsNonStringFiles(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())
	_, err := tool.handlePairAuditEmit(context.Background(), map[string]any{
		"scope":      "scope",
		"finding_id": "X",
		"files":      []any{"good.go", float64(42), "alsogood.go"},
	})
	if err == nil {
		t.Error("non-string file element should error")
	}
}

// TestPairAuditEmit_DefaultsSeverity — when severity is missing from
// args, the handler defaults to 5. Avoids forcing the agent to compute
// a number when DeepSeek didn't return one.
func TestPairAuditEmit_DefaultsSeverity(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())
	resp, err := tool.handlePairAuditEmit(context.Background(), map[string]any{
		"scope":      "audit:.go:pkg/sre",
		"finding_id": "Y",
	})
	if err != nil {
		t.Fatalf("handlePairAuditEmit: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"severity":5`) {
		t.Errorf("expected default severity:5, got %s", body)
	}
}

// TestTrustStatus_TopLimit — top:N caps the entries returned.
func TestTrustStatus_TopLimit(t *testing.T) {
	setupExecuteNextPlanner(t)
	tool := NewDaemonTool(nil, t.TempDir())

	for _, p := range []string{"a", "b", "c", "d", "e"} {
		if err := state.TrustWarmup(p, ".go:pkg/state", 100, 10); err != nil {
			t.Fatalf("warmup %s: %v", p, err)
		}
	}

	resp, err := tool.handleTrustStatus(context.Background(), map[string]any{
		"top": float64(2), // JSON numbers come in as float64
	})
	if err != nil {
		t.Fatalf("handleTrustStatus: %v", err)
	}
	body := extractMcpText(t, resp)
	if !strings.Contains(body, `"returned":2`) {
		t.Errorf("expected returned:2 with top:2, got %s", body)
	}
	if !strings.Contains(body, `"total_scores":5`) {
		t.Errorf("expected total_scores:5, got %s", body)
	}
}

// TestReject_DoubleRejectErrors — once rejected, the daemon_result is
// in status=rejected; a second reject errors instead of double-billing
// trust.
func TestReject_DoubleRejectErrors(t *testing.T) {
	tool := rejectFlowSetup(t)
	if _, err := tool.handleReject(context.Background(), map[string]any{
		"task_id":     "TASK-001",
		"reason_kind": "quality",
	}); err != nil {
		t.Fatalf("first reject: %v", err)
	}
	if _, err := tool.handleReject(context.Background(), map[string]any{
		"task_id":     "TASK-001",
		"reason_kind": "quality",
	}); err == nil {
		t.Error("second reject on same task should error")
	}
}
