// cmd/neo-mcp/daemon_e2e_test.go — end-to-end test of the iterative
// daemon cycle: PushTasks → execute_next → approve loop. Validates
// that trust scores ascend correctly with repeated successes and that
// the bucket transitions match the contract. [138.D.1]
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/state"
)

// TestE2E_TrustAscendsWithRepeatedSuccess — the canonical end-to-end
// scenario from the master_plan: enqueue 10 refactor tasks all hitting
// the same (pattern, scope) bucket, run execute_next + approve in a
// loop, verify trust grows monotonically and tier eventually reflects
// the accumulated evidence.
//
// EnqueueTasks renames IDs to TASK-001..TASK-010 deterministically, so
// the test references those IDs. The trust ascent is the critical
// observable: 10 successes against the same bucket → α=11 (10 + prior),
// β=1 (prior), point estimate ≈ 0.917.
func TestE2E_TrustAscendsWithRepeatedSuccess(t *testing.T) {
	setupExecuteNextPlanner(t)

	// 10 refactor tasks all targeting pkg/state — they collapse to
	// the same trust bucket (refactor:.go:pkg/state).
	tasks := make([]state.SRETask, 10)
	for i := 0; i < 10; i++ {
		tasks[i] = state.SRETask{
			Description: "refactor logger split",
			TargetFile:  "pkg/state/planner.go",
		}
	}
	if err := state.EnqueueTasks(tasks); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	tool := NewDaemonTool(nil, t.TempDir())
	ctx := context.Background()
	const pattern = "refactor"
	const scope = ".go:pkg/state"

	// Loop: 10 cycles of execute_next → approve. Capture α at each step
	// to verify monotonic growth.
	alphas := make([]float64, 10)
	for i := 0; i < 10; i++ {
		runOneApproveCycle(t, tool, ctx, i, pattern, scope)
		s, _ := state.TrustGet(pattern, scope)
		alphas[i] = s.Alpha
	}

	// Verify monotonic ascent: α[i] = α[i-1] + 1.
	for i := 1; i < 10; i++ {
		if alphas[i] != alphas[i-1]+1 {
			t.Errorf("α not monotonic: α[%d]=%v, α[%d]=%v", i-1, alphas[i-1], i, alphas[i])
		}
	}

	// Final state: α should be 11 (1 prior + 10 successes), β still 1.
	final, _ := state.TrustGet(pattern, scope)
	if final.Alpha != 11 || final.Beta != 1 {
		t.Errorf("final α/β = %v/%v, want 11/1", final.Alpha, final.Beta)
	}
	if final.TotalExecutions != 10 {
		t.Errorf("TotalExecutions=%d, want 10", final.TotalExecutions)
	}

	// Tier still L0 because the gate (50 execs) hasn't cleared yet —
	// even with 10/10 successes, the system requires more samples
	// before promoting. This is the conservative-by-design behavior.
	if final.CurrentTier != state.TierL0 {
		t.Errorf("CurrentTier=%q, want L0 (gate of 50 execs not cleared)",
			final.CurrentTier)
	}
}

// TestE2E_MixedSuccessFailureMovesTier — alternate approve and reject
// (quality) over many cycles to drive both α and β. After the gate
// clears, tier reflects the 50/50 LowerBound (well below 0.65 → L0).
//
// This is the "noisy pattern" scenario: a model that's 50% right
// shouldn't earn auto-approval, even with abundant evidence.
func TestE2E_MixedSuccessFailureMovesTier(t *testing.T) {
	setupExecuteNextPlanner(t)

	// Need 60 cycles to clear the 50-exec gate and have meaningful LB.
	tasks := make([]state.SRETask, 60)
	for i := 0; i < 60; i++ {
		tasks[i] = state.SRETask{
			Description: "audit handler",
			TargetFile:  "cmd/neo-mcp/main.go",
		}
	}
	if err := state.EnqueueTasks(tasks); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	tool := NewDaemonTool(nil, t.TempDir())
	ctx := context.Background()
	const pattern = "audit"
	const scope = ".go:cmd/neo-mcp"

	// Alternate: even index = approve, odd = reject(quality).
	// Reject(quality) re-queues with Retries++, so we may end up with
	// some FailedPermanent tasks after the third quality reject on the
	// SAME ID. Easier: enqueue twice as many tasks and approve/reject
	// each one once (no requeue cascade).
	for i := 0; i < 60; i++ {
		taskID := taskIDForIndex(i)
		if _, err := tool.handleExecuteNext(ctx, map[string]any{}); err != nil {
			// If the queue empties early due to FailedPermanent
			// cascading, stop the loop gracefully.
			break
		}
		if i%2 == 0 {
			_, err := tool.handleApprove(ctx, map[string]any{"task_id": taskID})
			if err != nil {
				t.Fatalf("approve cycle %d: %v", i, err)
			}
		} else {
			_, err := tool.handleReject(ctx, map[string]any{
				"task_id":     taskID,
				"reason_kind": "quality",
				"requeue":     false, // avoid retry cascade for this test
			})
			if err != nil {
				t.Fatalf("reject cycle %d: %v", i, err)
			}
		}
	}

	final, _ := state.TrustGet(pattern, scope)

	// 30 successes (α += 30), 30 quality rejects (β += 5*30 = 150).
	// LowerBound for α=31 β=151 is far below 0.65 → tier remains L0.
	if final.CurrentTier != state.TierL0 {
		t.Errorf("CurrentTier=%q, want L0 (50/50 with high β weight stays low-trust)",
			final.CurrentTier)
	}
	// β should be 1 (prior) + 30*5 = 151 (each quality reject adds 5).
	if final.Beta != 151 {
		t.Errorf("β=%v, want 151 (30 OperatorOverride at +5 each + prior)", final.Beta)
	}
}

// TestE2E_TrustStatusSurfacesAccumulatedEvidence — after running a
// few approve cycles, trust_status reports the bucket with non-zero
// counters and decay-aware lower bound. Validates the reporting wire
// is intact end-to-end.
func TestE2E_TrustStatusSurfacesAccumulatedEvidence(t *testing.T) {
	setupExecuteNextPlanner(t)

	if err := state.EnqueueTasks([]state.SRETask{
		{Description: "distill logs", TargetFile: "pkg/sre/oracle.go"},
		{Description: "distill logs", TargetFile: "pkg/sre/oracle.go"},
		{Description: "distill logs", TargetFile: "pkg/sre/oracle.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	tool := NewDaemonTool(nil, t.TempDir())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := tool.handleExecuteNext(ctx, map[string]any{}); err != nil {
			t.Fatalf("execute_next %d: %v", i, err)
		}
		if _, err := tool.handleApprove(ctx, map[string]any{
			"task_id": taskIDForIndex(i),
		}); err != nil {
			t.Fatalf("approve %d: %v", i, err)
		}
	}

	resp, err := tool.handleTrustStatus(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("trust_status: %v", err)
	}
	body := extractMcpText(t, resp)

	// Should include the distill:.go:pkg/sre bucket with α=4 (3 + prior).
	if !strings.Contains(body, `"pattern":"distill"`) {
		t.Errorf("trust_status missing distill bucket, body=%s", body)
	}
	if !strings.Contains(body, `"alpha":4`) {
		t.Errorf("trust_status missing α=4, body=%s", body)
	}
	if !strings.Contains(body, `"total_executions":3`) {
		t.Errorf("trust_status missing total_executions=3, body=%s", body)
	}
}

// runOneApproveCycle exercises one execute_next → approve pair and
// asserts the resolved (pattern, scope) matches expectations. Extracted
// to keep TestE2E_TrustAscendsWithRepeatedSuccess below CC=15.
func runOneApproveCycle(t *testing.T, tool *DaemonTool, ctx context.Context, idx int, wantPattern, wantScope string) {
	t.Helper()
	taskID := taskIDForIndex(idx)

	execResp, err := tool.handleExecuteNext(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("execute_next cycle %d: %v", idx, err)
	}
	var exec ExecuteNextResponse
	if uerr := json.Unmarshal([]byte(extractMcpText(t, execResp)), &exec); uerr != nil {
		t.Fatalf("unmarshal cycle %d: %v", idx, uerr)
	}
	if exec.Pattern != wantPattern || exec.Scope != wantScope {
		t.Errorf("cycle %d: pattern/scope drift — got %s:%s want %s:%s",
			idx, exec.Pattern, exec.Scope, wantPattern, wantScope)
	}
	if exec.TaskID != taskID {
		t.Errorf("cycle %d: TaskID=%q want %q", idx, exec.TaskID, taskID)
	}
	if _, err := tool.handleApprove(ctx, map[string]any{"task_id": taskID}); err != nil {
		t.Fatalf("approve cycle %d: %v", idx, err)
	}
}

// taskIDForIndex returns the deterministic ID EnqueueTasks assigns to
// the i-th task (TASK-001, TASK-002, ...). Cleaner than inlining the
// fmt.Sprintf in every test.
func taskIDForIndex(i int) string {
	if i < 9 {
		return "TASK-00" + string(rune('1'+i))
	}
	if i < 99 {
		return taskIDForIndexTwoDigit(i)
	}
	return "TASK-" + threeDigit(i+1)
}

func taskIDForIndexTwoDigit(i int) string {
	n := i + 1
	return "TASK-0" + twoDigit(n)
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func threeDigit(n int) string {
	return string(rune('0'+n/100)) + twoDigit(n%100)
}
