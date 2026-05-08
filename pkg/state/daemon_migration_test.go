// pkg/state/daemon_migration_test.go — tests for the boot-time trust
// migration shim. [138.C.7]
package state

import (
	"testing"
	"time"
)

// TestMigrateLegacyTasks_StampsActiveTasks — pending and in_progress
// tasks without migrated_at receive the timestamp; terminal-state tasks
// (completed, failed_permanent) are skipped.
func TestMigrateLegacyTasks_StampsActiveTasks(t *testing.T) {
	setupTestPlanner(t)

	// 4 tasks: 1 pending, 1 in_progress, 1 completed, 1 failed_permanent.
	if err := EnqueueTasks([]SRETask{
		{Description: "active 1", TargetFile: "a.go"},
		{Description: "active 2", TargetFile: "b.go"},
		{Description: "done one", TargetFile: "c.go"},
		{Description: "permafail", TargetFile: "d.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	// Manually transition the lifecycle of the last two tasks via the
	// existing helpers + a direct mark.
	if err := MarkTaskInProgress("TASK-002"); err != nil {
		t.Fatalf("MarkTaskInProgress: %v", err)
	}
	if err := MarkTaskCompleted("TASK-003"); err != nil {
		t.Fatalf("MarkTaskCompleted: %v", err)
	}
	if err := MarkTaskFailedPermanent("TASK-004", "test setup"); err != nil {
		t.Fatalf("MarkTaskFailedPermanent: %v", err)
	}

	migrated, err := MigrateLegacyTasks()
	if err != nil {
		t.Fatalf("MigrateLegacyTasks: %v", err)
	}
	// TASK-001 (pending) + TASK-002 (in_progress) should be stamped.
	// TASK-003 (completed) + TASK-004 (failed_permanent) skipped.
	if migrated != 2 {
		t.Errorf("migrated=%d, want 2 (pending+in_progress only)", migrated)
	}

	all := GetAllTasks()
	for _, task := range all {
		switch task.ID {
		case "TASK-001", "TASK-002":
			if task.MigratedAt == 0 {
				t.Errorf("%s should be stamped, got MigratedAt=0", task.ID)
			}
		case "TASK-003", "TASK-004":
			if task.MigratedAt != 0 {
				t.Errorf("%s should NOT be stamped (terminal lifecycle), got MigratedAt=%d", task.ID, task.MigratedAt)
			}
		}
	}
}

// TestMigrateLegacyTasks_Idempotent — running the migration twice
// produces zero new stamps on the second run. Boot can call this on
// every startup without re-stamping or accumulating wall-clock drift.
func TestMigrateLegacyTasks_Idempotent(t *testing.T) {
	setupTestPlanner(t)

	if err := EnqueueTasks([]SRETask{
		{Description: "to migrate", TargetFile: "x.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	first, err := MigrateLegacyTasks()
	if err != nil {
		t.Fatalf("first MigrateLegacyTasks: %v", err)
	}
	if first != 1 {
		t.Errorf("first run: migrated=%d, want 1", first)
	}

	// Capture the original timestamp; second run must NOT mutate it.
	all := GetAllTasks()
	var firstStamp int64
	for _, task := range all {
		if task.ID == "TASK-001" {
			firstStamp = task.MigratedAt
		}
	}
	if firstStamp == 0 {
		t.Fatal("first run did not stamp TASK-001")
	}

	// Sleep a second so a buggy non-idempotent re-stamp would change
	// the timestamp visibly.
	time.Sleep(time.Second + 100*time.Millisecond)

	second, err := MigrateLegacyTasks()
	if err != nil {
		t.Fatalf("second MigrateLegacyTasks: %v", err)
	}
	if second != 0 {
		t.Errorf("second run: migrated=%d, want 0 (idempotent)", second)
	}

	all2 := GetAllTasks()
	for _, task := range all2 {
		if task.ID == "TASK-001" && task.MigratedAt != firstStamp {
			t.Errorf("MigratedAt mutated by second run: was=%d, now=%d", firstStamp, task.MigratedAt)
		}
	}
}

// TestMigrateLegacyTasks_SeedsTrustBucket — after migration runs, the
// unknown:unknown TrustScore is present in the daemon_trust bucket so
// trust_status surfaces it with the (1, 1) prior even before any
// execute_next call.
func TestMigrateLegacyTasks_SeedsTrustBucket(t *testing.T) {
	setupTestPlanner(t)

	if err := EnqueueTasks([]SRETask{
		{Description: "test seed", TargetFile: "y.go"},
	}); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	if _, err := MigrateLegacyTasks(); err != nil {
		t.Fatalf("MigrateLegacyTasks: %v", err)
	}

	// The unknown:unknown bucket entry must exist after migration. We
	// use ListTrustScores so we hit the actual stored data, not the
	// auto-prior fallback inside TrustGet.
	scores, _, err := ListTrustScores()
	if err != nil {
		t.Fatalf("ListTrustScores: %v", err)
	}
	found := false
	for _, s := range scores {
		if s.Pattern == legacyMigrationPattern && s.Scope == legacyMigrationScope {
			found = true
			if s.Alpha != 1 || s.Beta != 1 {
				t.Errorf("seed prior wrong: α=%v β=%v, want 1/1", s.Alpha, s.Beta)
			}
		}
	}
	if !found {
		t.Errorf("unknown:unknown trust score not found in bucket after migration")
	}
}

// TestMigrateLegacyTasks_EmptyDB — no tasks → migrated=0, no error.
// First boot of a fresh installation lands here.
func TestMigrateLegacyTasks_EmptyDB(t *testing.T) {
	setupTestPlanner(t)
	migrated, err := MigrateLegacyTasks()
	if err != nil {
		t.Fatalf("MigrateLegacyTasks on empty: %v", err)
	}
	if migrated != 0 {
		t.Errorf("empty DB: migrated=%d, want 0", migrated)
	}
}
