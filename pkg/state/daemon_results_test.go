// pkg/state/daemon_results_test.go — tests for the daemon_results bucket [138.C.4]
package state

import (
	"testing"
	"time"
)

// TestPersistDaemonResult_RoundTrip — write then read recovers identical state.
func TestPersistDaemonResult_RoundTrip(t *testing.T) {
	setupTestPlanner(t)

	src := DaemonResult{
		TaskID:           "task-001",
		TaskDescription:  "refactor logger",
		Backend:          "deepseek",
		Pattern:          "refactor",
		Scope:            ".go:pkg/state",
		TrustAlphaBefore: 5,
		TrustBetaBefore:  2,
		TrustTierBefore:  "L1",
		TokensUsed:       1234,
		SuggestedAction:  "auto-approve",
		Status:           ResultPendingReview,
		CreatedAt:        time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
	}
	if err := PersistDaemonResult(src); err != nil {
		t.Fatalf("PersistDaemonResult: %v", err)
	}

	got, err := GetDaemonResult("task-001")
	if err != nil {
		t.Fatalf("GetDaemonResult: %v", err)
	}
	if got == nil {
		t.Fatal("GetDaemonResult returned nil for present entry")
	}
	if got.TaskID != src.TaskID || got.Pattern != src.Pattern || got.TokensUsed != src.TokensUsed {
		t.Errorf("roundtrip diverged: %+v vs %+v", got, src)
	}
	if got.SchemaVersion != daemonResultSchemaV1 {
		t.Errorf("SchemaVersion=%d, want %d (auto-set)", got.SchemaVersion, daemonResultSchemaV1)
	}
}

// TestGetDaemonResult_Absent — reading a never-written task returns nil
// without error. Caller distinguishes "absent" from "I/O failure".
func TestGetDaemonResult_Absent(t *testing.T) {
	setupTestPlanner(t)
	got, err := GetDaemonResult("never-written")
	if err != nil {
		t.Fatalf("GetDaemonResult: %v", err)
	}
	if got != nil {
		t.Errorf("absent entry should return nil, got %+v", got)
	}
}

// TestPersistDaemonResult_RequiresTaskID — empty TaskID is a programmer
// error; reject up front.
func TestPersistDaemonResult_RequiresTaskID(t *testing.T) {
	setupTestPlanner(t)
	if err := PersistDaemonResult(DaemonResult{Pattern: "x", Scope: "y"}); err == nil {
		t.Error("empty TaskID should error")
	}
}

// TestUpdateDaemonResult_TransitionsStatus — typical approve flow:
// pending_review → approved + operator note + completed_at populated.
func TestUpdateDaemonResult_TransitionsStatus(t *testing.T) {
	setupTestPlanner(t)
	if err := PersistDaemonResult(DaemonResult{
		TaskID:  "task-002",
		Pattern: "audit",
		Scope:   ".go:pkg/state",
		Status:  ResultPendingReview,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	completedAt := time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)
	err := UpdateDaemonResult("task-002", func(r *DaemonResult) {
		r.Status = ResultApproved
		r.OperatorNote = "looks correct"
		r.CompletedAt = &completedAt
	})
	if err != nil {
		t.Fatalf("UpdateDaemonResult: %v", err)
	}

	got, _ := GetDaemonResult("task-002")
	if got.Status != ResultApproved {
		t.Errorf("Status=%q, want approved", got.Status)
	}
	if got.OperatorNote != "looks correct" {
		t.Errorf("OperatorNote=%q, want \"looks correct\"", got.OperatorNote)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt=%v, want %v", got.CompletedAt, completedAt)
	}
}

// TestUpdateDaemonResult_ErrorsWhenAbsent — caller must seed first
// via PersistDaemonResult; updating a missing entry surfaces the error
// instead of silently creating one (would mask seed-order bugs).
func TestUpdateDaemonResult_ErrorsWhenAbsent(t *testing.T) {
	setupTestPlanner(t)
	err := UpdateDaemonResult("never-seeded", func(r *DaemonResult) { r.Status = ResultApproved })
	if err == nil {
		t.Error("UpdateDaemonResult on absent entry should error")
	}
}

// TestMarkTaskCompleted_TransitionsStatus — by-ID variant of MarkTaskDone
// closes a specific task, leaving others alone. Companion to
// MarkTaskInProgress for the daemon approve flow. EnqueueTasks
// auto-assigns IDs as TASK-001, TASK-002, ... so the test uses those
// rather than the input field. [138.C.2]
func TestMarkTaskCompleted_TransitionsStatus(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{
		{Description: "first", TargetFile: "a.go"},
		{Description: "second", TargetFile: "b.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	if err := MarkTaskCompleted("TASK-001"); err != nil {
		t.Fatalf("MarkTaskCompleted: %v", err)
	}

	got := make(map[string]string)
	for _, task := range GetAllTasks() {
		got[task.ID] = task.Status
	}
	if got["TASK-001"] != "DONE" {
		t.Errorf("TASK-001 Status=%q, want DONE", got["TASK-001"])
	}
	if got["TASK-002"] == "DONE" {
		t.Error("TASK-002 should still be TODO — MarkTaskCompleted is by-ID, not cursor-walk")
	}
}

// TestMarkTaskCompleted_AbsentIsNoop — non-existent task ID is a no-op,
// not an error. Mirrors MarkTaskInProgress semantics so callers can be
// idempotent.
func TestMarkTaskCompleted_AbsentIsNoop(t *testing.T) {
	setupTestPlanner(t)
	if err := MarkTaskCompleted("never-existed"); err != nil {
		t.Errorf("MarkTaskCompleted on absent task: got %v, want nil", err)
	}
}
