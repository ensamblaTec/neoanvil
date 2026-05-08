// pkg/state/planner_lifecycle_test.go — tests for task lifecycle and batch certify 2PC. [362.A]
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func setupTestPlanner(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := InitPlanner(dir); err != nil {
		t.Fatalf("InitPlanner: %v", err)
	}
	t.Cleanup(func() {
		if plannerDB != nil {
			_ = plannerDB.Close()
			plannerDB = nil
		}
	})
}

// backdateTaskUpdatedAt writes an earlier UpdatedAt for a task directly via BoltDB. [362.A test helper]
func backdateTaskUpdatedAt(t *testing.T, taskID string, unixSec int64) {
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
		task.UpdatedAt = unixSec
		val, merr := json.Marshal(task)
		if merr != nil {
			return merr
		}
		return b.Put([]byte(taskID), val)
	})
	if err != nil {
		t.Fatalf("backdateTaskUpdatedAt: %v", err)
	}
}

// TestTaskLifecycleInitialState verifies freshly enqueued tasks get lifecycle_state=pending. [362.A]
func TestTaskLifecycleInitialState(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{
		{Description: "alpha", TargetFile: "a.go"},
		{Description: "beta", TargetFile: "b.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	all := GetAllTasks()
	if len(all) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(all))
	}
	for _, task := range all {
		if task.LifecycleState != TaskLifecyclePending {
			t.Errorf("task %s: want lifecycle_state=%q, got %q", task.ID, TaskLifecyclePending, task.LifecycleState)
		}
		if task.CreatedAt == 0 {
			t.Errorf("task %s: CreatedAt not set", task.ID)
		}
	}
}

// TestMarkTaskInProgress verifies lifecycle transition pending → in_progress. [362.A]
func TestMarkTaskInProgress(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{{Description: "task1", TargetFile: "x.go"}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	task, err := GetNextTask()
	if err != nil || task == nil {
		t.Fatalf("GetNextTask: err=%v task=%v", err, task)
	}
	if err := MarkTaskInProgress(task.ID); err != nil {
		t.Fatalf("MarkTaskInProgress: %v", err)
	}
	all := GetAllTasks()
	if len(all) == 0 {
		t.Fatal("no tasks after MarkTaskInProgress")
	}
	if all[0].LifecycleState != TaskLifecycleInProgress {
		t.Errorf("want %q, got %q", TaskLifecycleInProgress, all[0].LifecycleState)
	}
}

// TestMarkTaskDoneLifecycle verifies MarkTaskDone transitions lifecycle to completed. [362.A]
func TestMarkTaskDoneLifecycle(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{{Description: "t1", TargetFile: "f.go"}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	if _, err := MarkTaskDone(); err != nil {
		t.Fatalf("MarkTaskDone: %v", err)
	}
	all := GetAllTasks()
	if len(all) == 0 {
		t.Fatal("no tasks")
	}
	if all[0].LifecycleState != TaskLifecycleCompleted {
		t.Errorf("want %q, got %q", TaskLifecycleCompleted, all[0].LifecycleState)
	}
}

// TestMarkTasksOrphaned verifies that in_progress tasks past the timeout become orphaned. [362.A]
func TestMarkTasksOrphaned(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{
		{Description: "stale", TargetFile: "s.go"},
		{Description: "fresh", TargetFile: "f.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	all := GetAllTasks()
	staleID := all[0].ID
	if err := MarkTaskInProgress(staleID); err != nil {
		t.Fatalf("MarkTaskInProgress: %v", err)
	}
	// Backdate the task to simulate a stalled agent (> 60 min ago).
	backdateTaskUpdatedAt(t, staleID, time.Now().Add(-2*time.Hour).Unix())

	orphans, err := MarkTasksOrphaned(60)
	if err != nil {
		t.Fatalf("MarkTasksOrphaned: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("want 1 orphan, got %d", len(orphans))
	}
	if orphans[0].ID != staleID {
		t.Errorf("unexpected orphaned task: %s", orphans[0].ID)
	}
	if orphans[0].LifecycleState != TaskLifecycleOrphaned {
		t.Errorf("lifecycle_state=%q, want %q", orphans[0].LifecycleState, TaskLifecycleOrphaned)
	}
	if n := CountOrphanedTasks(); n != 1 {
		t.Errorf("CountOrphanedTasks: want 1, got %d", n)
	}
}

// TestMarkTasksOrphaned_FreshNotOrphaned verifies recently-claimed tasks are not orphaned. [362.A]
func TestMarkTasksOrphaned_FreshNotOrphaned(t *testing.T) {
	setupTestPlanner(t)
	if err := EnqueueTasks([]SRETask{{Description: "active", TargetFile: "a.go"}}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}
	task, _ := GetNextTask()
	_ = MarkTaskInProgress(task.ID) // UpdatedAt = now

	orphans, err := MarkTasksOrphaned(60)
	if err != nil {
		t.Fatalf("MarkTasksOrphaned: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("want 0 orphans for fresh task, got %d", len(orphans))
	}
}

// TestBatchCertify2PC verifies begin/commit/abort/recover for the certify TX log. [362.A]
func TestBatchCertify2PC(t *testing.T) {
	setupTestPlanner(t)
	txID := "certify-test-001"
	files := []string{"/a.go", "/b.go"}

	if err := BeginBatchCertify(txID, files); err != nil {
		t.Fatalf("BeginBatchCertify: %v", err)
	}
	pending, err := RecoverPendingCertify()
	if err != nil {
		t.Fatalf("RecoverPendingCertify: %v", err)
	}
	if len(pending) != 1 || pending[0].TxID != txID {
		t.Fatalf("want 1 prepared tx %q, got %+v", txID, pending)
	}
	if pending[0].State != "prepared" {
		t.Errorf("want state=prepared, got %q", pending[0].State)
	}

	if err := CommitBatchCertify(txID); err != nil {
		t.Fatalf("CommitBatchCertify: %v", err)
	}
	pending, _ = RecoverPendingCertify()
	if len(pending) != 0 {
		t.Errorf("after commit: want 0 pending, got %d", len(pending))
	}

	// Abort path.
	tx2 := "certify-test-002"
	_ = BeginBatchCertify(tx2, files)
	_ = AbortBatchCertify(tx2)
	pending, _ = RecoverPendingCertify()
	if len(pending) != 0 {
		t.Errorf("after abort: want 0 pending, got %d", len(pending))
	}
}

// TestCountOrphanedTasksZero verifies no orphans on a fresh queue. [362.A]
func TestCountOrphanedTasksZero(t *testing.T) {
	setupTestPlanner(t)
	if n := CountOrphanedTasks(); n != 0 {
		t.Errorf("fresh queue: want 0 orphans, got %d", n)
	}
}

// TestInitPlannerCreatesDB verifies InitPlanner creates the database file. [362.A]
func TestInitPlannerCreatesDB(t *testing.T) {
	dir := t.TempDir()
	if err := InitPlanner(dir); err != nil {
		t.Fatalf("InitPlanner: %v", err)
	}
	defer func() {
		if plannerDB != nil {
			_ = plannerDB.Close()
			plannerDB = nil
		}
	}()
	dbPath := filepath.Join(dir, ".neo/db", "planner.db")
	if _, serr := os.Stat(dbPath); serr != nil {
		t.Errorf("planner.db not created at %s: %v", dbPath, serr)
	}
}
