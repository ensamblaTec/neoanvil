// pkg/state/daemon_progress_test.go — tests for daemon progress visibility. [132.C]
package state

import (
	"encoding/json"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// TestPullTasksIncludesActive verifies that GetDaemonActiveTask returns the
// in_progress task after MarkTaskInProgress is called. [132.C]
func TestPullTasksIncludesActive(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{
		{Description: "active-task", TargetFile: "active.go"},
		{Description: "queued-task", TargetFile: "queued.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	all := GetAllTasks()
	activeID := all[0].ID

	if err := MarkTaskInProgress(activeID); err != nil {
		t.Fatalf("MarkTaskInProgress: %v", err)
	}

	active, err := GetDaemonActiveTask()
	if err != nil {
		t.Fatalf("GetDaemonActiveTask: %v", err)
	}
	if active == nil {
		t.Fatal("GetDaemonActiveTask returned nil, want in_progress task")
	}
	if active.Description != "active-task" {
		t.Errorf("Description=%q, want %q", active.Description, "active-task")
	}
	if active.TargetFile != "active.go" {
		t.Errorf("TargetFile=%q, want %q", active.TargetFile, "active.go")
	}
	if active.StartedAt == 0 {
		t.Error("StartedAt should be set after MarkTaskInProgress")
	}

	// No active task when none are in_progress.
	setupTestPlanner(t) // reset
	if err := EnqueueTasks([]SRETask{{Description: "pending", TargetFile: "p.go"}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	noActive, err := GetDaemonActiveTask()
	if err != nil {
		t.Fatalf("GetDaemonActiveTask (no in_progress): %v", err)
	}
	if noActive != nil {
		t.Errorf("expected nil active task for pending-only queue, got: %+v", noActive)
	}
}

// TestProgressNotificationEmitted verifies that BuildDaemonProgressPayload includes
// the active task and correct counters. [132.C]
func TestProgressNotificationEmitted(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{
		{Description: "task-one", TargetFile: "one.go"},
		{Description: "task-two", TargetFile: "two.go"},
		{Description: "task-three", TargetFile: "three.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	all := GetAllTasks()

	// Mark first task completed, second in_progress.
	setTaskLifecycle(t, all[0].ID, TaskLifecycleCompleted)
	if err := MarkTaskInProgress(all[1].ID); err != nil {
		t.Fatalf("MarkTaskInProgress: %v", err)
	}
	// Update status field of first task to DONE so counter reads correctly.
	setTaskStatus(t, all[0].ID, "DONE")

	payload, err := BuildDaemonProgressPayload("sess-progress", 200000)
	if err != nil {
		t.Fatalf("BuildDaemonProgressPayload: %v", err)
	}

	if payload.TasksDone != 1 {
		t.Errorf("TasksDone=%d, want 1", payload.TasksDone)
	}
	if payload.TasksTotal != 3 {
		t.Errorf("TasksTotal=%d, want 3", payload.TasksTotal)
	}
	if payload.ActiveTask == nil {
		t.Fatal("ActiveTask should not be nil")
	}
	if payload.ActiveTask.Description != "task-two" {
		t.Errorf("ActiveTask.Description=%q, want %q", payload.ActiveTask.Description, "task-two")
	}
	if payload.Summary.InProgress != 1 {
		t.Errorf("Summary.InProgress=%d, want 1", payload.Summary.InProgress)
	}
	if payload.Summary.Pending != 1 {
		t.Errorf("Summary.Pending=%d, want 1", payload.Summary.Pending)
	}
}

// TestHudEventPayload verifies that DaemonProgressPayload marshals to JSON with
// all expected fields for the SSE EventDaemonProgress event. [132.C]
func TestHudEventPayload(t *testing.T) {
	payload := DaemonProgressPayload{
		TasksDone:  3,
		TasksTotal: 10,
		ActiveTask: &DaemonActiveTask{
			Description: "refactor handler",
			TargetFile:  "handler.go",
			StartedAt:   time.Now().Unix(),
			Retries:     1,
		},
		Summary: DaemonQueueSummary{
			Pending:         5,
			InProgress:      1,
			Done:            3,
			Failed:          1,
			BudgetRemaining: 150000,
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var roundtrip DaemonProgressPayload
	if err := json.Unmarshal(raw, &roundtrip); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if roundtrip.TasksDone != 3 {
		t.Errorf("TasksDone=%d, want 3", roundtrip.TasksDone)
	}
	if roundtrip.TasksTotal != 10 {
		t.Errorf("TasksTotal=%d, want 10", roundtrip.TasksTotal)
	}
	if roundtrip.ActiveTask == nil {
		t.Fatal("ActiveTask nil after roundtrip")
	}
	if roundtrip.ActiveTask.Retries != 1 {
		t.Errorf("ActiveTask.Retries=%d, want 1", roundtrip.ActiveTask.Retries)
	}
	if roundtrip.Summary.BudgetRemaining != 150000 {
		t.Errorf("Summary.BudgetRemaining=%d, want 150000", roundtrip.Summary.BudgetRemaining)
	}
	if roundtrip.Summary.Failed != 1 {
		t.Errorf("Summary.Failed=%d, want 1", roundtrip.Summary.Failed)
	}
}

// setTaskStatus directly sets the Status field of a task in BoltDB. [132.C test helper]
func setTaskStatus(t *testing.T, taskID, status string) {
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
		task.Status = status
		val, merr := json.Marshal(task)
		if merr != nil {
			return merr
		}
		return b.Put([]byte(taskID), val)
	})
	if err != nil {
		t.Fatalf("setTaskStatus: %v", err)
	}
}
