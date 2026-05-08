// pkg/state/planner_orphan_recovery_test.go — tests for RecoverOrphanedTasks. [132.A]
package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// TestRecoverOrphanedTasks_RetriableReset verifies that an orphaned task below maxRetries
// is reset to pending with Retries incremented and LastError populated. [132.A]
func TestRecoverOrphanedTasks_RetriableReset(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{{Description: "retriable", TargetFile: "r.go"}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	all := GetAllTasks()
	taskID := all[0].ID

	// Claim and stall the task so it becomes orphaned.
	if err := MarkTaskInProgress(taskID); err != nil {
		t.Fatalf("MarkTaskInProgress: %v", err)
	}
	backdateTaskUpdatedAt(t, taskID, time.Now().Add(-2*time.Hour).Unix())

	// Recover with maxRetries=3 and 60-minute timeout.
	recovered, err := RecoverOrphanedTasks(3, 60)
	if err != nil {
		t.Fatalf("RecoverOrphanedTasks: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("want 1 recovered task, got %d", len(recovered))
	}

	task := recovered[0]
	if task.LifecycleState != TaskLifecyclePending {
		t.Errorf("lifecycle_state=%q, want %q", task.LifecycleState, TaskLifecyclePending)
	}
	if task.Status != "TODO" {
		t.Errorf("status=%q, want TODO", task.Status)
	}
	if task.Retries != 1 {
		t.Errorf("retries=%d, want 1", task.Retries)
	}
	if task.LastError == "" {
		t.Error("LastError should be set after orphan recovery")
	}
	if !strings.Contains(task.LastError, "orphaned") {
		t.Errorf("LastError should mention 'orphaned', got: %s", task.LastError)
	}

	// Verify the task is returned by GetNextTask again.
	next, err := GetNextTask()
	if err != nil || next == nil {
		t.Fatalf("GetNextTask after recovery: err=%v task=%v", err, next)
	}
	if next.ID != taskID {
		t.Errorf("GetNextTask returned wrong task %s, want %s", next.ID, taskID)
	}
}

// TestRecoverOrphanedTasks_FailPermanent verifies that a task at maxRetries becomes
// failed_permanent and is excluded from future PullTasks. [132.A]
func TestRecoverOrphanedTasks_FailPermanent(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{{Description: "exhausted", TargetFile: "e.go"}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	all := GetAllTasks()
	taskID := all[0].ID

	// Simulate task having already used all retries.
	setTaskRetries(t, taskID, 3)

	// Claim and stall so timeout fires.
	if err := MarkTaskInProgress(taskID); err != nil {
		t.Fatalf("MarkTaskInProgress: %v", err)
	}
	backdateTaskUpdatedAt(t, taskID, time.Now().Add(-2*time.Hour).Unix())

	recovered, err := RecoverOrphanedTasks(3, 60)
	if err != nil {
		t.Fatalf("RecoverOrphanedTasks: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("want 1 recovered task, got %d", len(recovered))
	}
	if recovered[0].LifecycleState != TaskLifecycleFailedPermanent {
		t.Errorf("lifecycle_state=%q, want %q", recovered[0].LifecycleState, TaskLifecycleFailedPermanent)
	}
	if !strings.Contains(recovered[0].LastError, "max retries") {
		t.Errorf("LastError should mention 'max retries', got: %s", recovered[0].LastError)
	}

	// Verify PullTasks skips it.
	next, err := GetNextTask()
	if err != nil {
		t.Fatalf("GetNextTask: %v", err)
	}
	if next != nil {
		t.Errorf("GetNextTask should return nil after permanent failure, got %s", next.ID)
	}
}

// TestRecoverOrphanedTasks_SkipsNonOrphaned verifies that fresh in_progress tasks
// (within timeout) are not touched by recovery. [132.A]
func TestRecoverOrphanedTasks_SkipsNonOrphaned(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{
		{Description: "fresh-active", TargetFile: "f.go"},
		{Description: "pending-only", TargetFile: "p.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	all := GetAllTasks()
	// Claim the first task (recent UpdatedAt — within timeout).
	if err := MarkTaskInProgress(all[0].ID); err != nil {
		t.Fatalf("MarkTaskInProgress: %v", err)
	}

	recovered, err := RecoverOrphanedTasks(3, 60)
	if err != nil {
		t.Fatalf("RecoverOrphanedTasks: %v", err)
	}
	if len(recovered) != 0 {
		t.Errorf("want 0 recovered (fresh tasks should not be touched), got %d", len(recovered))
	}
}

// TestGetNextTaskByRole_SkipsFailedPermanent verifies that GetNextTaskByRole never returns
// a task with lifecycle_state=failed_permanent. [132.A]
func TestGetNextTaskByRole_SkipsFailedPermanent(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{
		{Description: "dead", TargetFile: "d.go"},
		{Description: "alive", TargetFile: "a.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	all := GetAllTasks()

	// Mark first task as failed_permanent directly.
	setTaskLifecycle(t, all[0].ID, TaskLifecycleFailedPermanent)

	// GetNextTaskByRole should skip the dead task and return the alive one.
	next, err := GetNextTaskByRole("")
	if err != nil {
		t.Fatalf("GetNextTaskByRole: %v", err)
	}
	if next == nil {
		t.Fatal("GetNextTaskByRole returned nil, want alive task")
	}
	if next.ID == all[0].ID {
		t.Errorf("returned failed_permanent task %s, should have been skipped", next.ID)
	}
	if next.ID != all[1].ID {
		t.Errorf("want task %s, got %s", all[1].ID, next.ID)
	}
}

// setTaskRetries directly sets the Retries field of a task in BoltDB. [132.A test helper]
func setTaskRetries(t *testing.T, taskID string, retries int) {
	t.Helper()
	err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(taskID))
		if v == nil {
			return nil
		}
		var task SRETask
		if jerr := json.Unmarshal(v, &task); jerr != nil {
			return jerr
		}
		task.Retries = retries
		val, merr := json.Marshal(task)
		if merr != nil {
			return merr
		}
		return b.Put([]byte(taskID), val)
	})
	if err != nil {
		t.Fatalf("setTaskRetries: %v", err)
	}
}

// setTaskLifecycle directly sets the LifecycleState of a task in BoltDB. [132.A test helper]
func setTaskLifecycle(t *testing.T, taskID, state string) {
	t.Helper()
	err := plannerDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(taskBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(taskID))
		if v == nil {
			return nil
		}
		var task SRETask
		if jerr := json.Unmarshal(v, &task); jerr != nil {
			return jerr
		}
		task.LifecycleState = state
		val, merr := json.Marshal(task)
		if merr != nil {
			return merr
		}
		return b.Put([]byte(taskID), val)
	})
	if err != nil {
		t.Fatalf("setTaskLifecycle: %v", err)
	}
}
