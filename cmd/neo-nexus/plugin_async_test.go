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
