package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func newTestAsyncStore(t *testing.T) *AsyncTaskStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "async_test.db")
	store, err := NewAsyncTaskStore(dbPath)
	if err != nil {
		t.Fatalf("NewAsyncTaskStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAsyncStore_SubmitAndGet(t *testing.T) {
	store := newTestAsyncStore(t)
	id, err := store.Submit("deepseek", "red_team_audit")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	task, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.Status != AsyncPending {
		t.Errorf("expected pending, got %s", task.Status)
	}
	if task.Plugin != "deepseek" {
		t.Errorf("expected deepseek, got %s", task.Plugin)
	}
}

func TestAsyncStore_CompleteAndPoll(t *testing.T) {
	store := newTestAsyncStore(t)
	id, _ := store.Submit("deepseek", "red_team_audit")
	result := json.RawMessage(`{"findings": 3}`)
	if err := store.Complete(id, result, 5*time.Second); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	task, _ := store.Get(id)
	if task.Status != AsyncDone {
		t.Errorf("expected done, got %s", task.Status)
	}
	if task.ElapsedMs != 5000 {
		t.Errorf("expected 5000ms, got %d", task.ElapsedMs)
	}
	if task.CompletedAt == nil {
		t.Error("CompletedAt should be set on done")
	}
}

func TestAsyncStore_RunningNoCompletedAt(t *testing.T) {
	store := newTestAsyncStore(t)
	id, _ := store.Submit("deepseek", "audit")
	_ = store.setStatus(id, AsyncRunning, nil, "", 0)
	task, _ := store.Get(id)
	if task.Status != AsyncRunning {
		t.Errorf("expected running, got %s", task.Status)
	}
	if task.CompletedAt != nil {
		t.Error("CompletedAt should be nil for running status (ASYNC-001 fix)")
	}
}

func TestAsyncStore_FailWithError(t *testing.T) {
	store := newTestAsyncStore(t)
	id, _ := store.Submit("deepseek", "audit")
	if err := store.Fail(id, "timeout", 30*time.Second); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	task, _ := store.Get(id)
	if task.Status != AsyncError {
		t.Errorf("expected error, got %s", task.Status)
	}
	if task.ErrorMsg != "timeout" {
		t.Errorf("expected 'timeout', got %q", task.ErrorMsg)
	}
}

func TestAsyncStore_MaxPendingReject(t *testing.T) {
	store := newTestAsyncStore(t)
	for i := range asyncMaxPending {
		_, err := store.Submit("test", "action")
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	_, err := store.Submit("test", "overflow")
	if err == nil {
		t.Fatal("expected error when queue full")
	}
}

func TestAsyncStore_Cleanup(t *testing.T) {
	store := newTestAsyncStore(t)
	id, _ := store.Submit("test", "action")
	_ = store.Complete(id, json.RawMessage(`{}`), time.Second)

	removed, err := store.Cleanup(0)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
	_, err = store.Get(id)
	if err == nil {
		t.Error("task should be gone after cleanup")
	}
}

func TestAsyncStore_List(t *testing.T) {
	store := newTestAsyncStore(t)
	store.Submit("deepseek", "audit")
	store.Submit("deepseek", "distill")
	store.Submit("jira", "get_context")

	all, _ := store.List("", "")
	if len(all) != 3 {
		t.Errorf("expected 3, got %d", len(all))
	}
	ds, _ := store.List("deepseek", "")
	if len(ds) != 2 {
		t.Errorf("expected 2 deepseek, got %d", len(ds))
	}
}

// TestAsyncStore_BatchMapping_SaveGet covers the basic round-trip in a single
// store instance — [Phase 4.B / Speed-First] precursor to the cross-restart
// case below.
func TestAsyncStore_BatchMapping_SaveGet(t *testing.T) {
	store := newTestAsyncStore(t)

	if got, ok := store.GetBatchMapping("batch_unknown"); ok || got != nil {
		t.Errorf("missing batch should yield (nil, false), got (%v, %v)", got, ok)
	}

	taskIDs := []string{"async_aaa", "async_bbb", "async_ccc"}
	if err := store.SaveBatchMapping("batch_xyz", taskIDs); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok := store.GetBatchMapping("batch_xyz")
	if !ok {
		t.Fatal("expected batch found after save")
	}
	if len(got) != 3 || got[0] != "async_aaa" || got[2] != "async_ccc" {
		t.Errorf("got %v, want %v", got, taskIDs)
	}

	// Idempotent overwrite.
	if err := store.SaveBatchMapping("batch_xyz", []string{"async_only"}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = store.GetBatchMapping("batch_xyz")
	if len(got) != 1 || got[0] != "async_only" {
		t.Errorf("overwrite lost: got %v", got)
	}
}

// TestAsyncStore_BatchMapping_SurvivesRestart is the headline Phase 4.B
// regression. Before this commit, batchMap was a package-level in-memory
// map: a Nexus restart wiped it while the per-task AsyncTask rows in BoltDB
// survived, so handleBatchPoll returned "batch not found" for valid task
// IDs. Now the mapping is in BoltDB next to the tasks → close the store,
// re-open the SAME file, the mapping is still there.
func TestAsyncStore_BatchMapping_SurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "async.db")

	// First "lifetime" — write the mapping.
	store1, err := NewAsyncTaskStore(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	want := []string{"async_111", "async_222", "async_333", "async_444"}
	if err := store1.SaveBatchMapping("batch_persistent", want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	// Simulated restart — fresh store, same disk path.
	store2, err := NewAsyncTaskStore(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer store2.Close()

	got, ok := store2.GetBatchMapping("batch_persistent")
	if !ok {
		t.Fatal("mapping lost across restart — Phase 4.B regression")
	}
	if len(got) != len(want) {
		t.Fatalf("post-restart len = %d, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("post-restart got[%d] = %q, want %q", i, got[i], id)
		}
	}
}
