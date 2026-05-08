package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadActivePhase_FirstPhaseWithOpenTask(t *testing.T) {
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	const plan = `## PILAR A — Activo
### Épica 1
- [x] **1.A — done**
- [ ] **1.B — open**
## PILAR B — Futuro
### Épica 2
- [ ] **2.A — pending**
`
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadActivePhase(dir)
	if err != nil {
		t.Fatalf("ReadActivePhase: %v", err)
	}
	if !strings.Contains(got, "PILAR A") {
		t.Errorf("should return PILAR A as active phase, got: %q", got)
	}
	if strings.Contains(got, "PILAR B") {
		t.Errorf("should not include PILAR B (second phase), got: %q", got)
	}
}

func TestReadActivePhase_AllDone(t *testing.T) {
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	const plan = `## PILAR A — Completo
- [x] **1.A — done**
- [x] **1.B — done**
`
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadActivePhase(dir)
	if err != nil {
		t.Fatalf("ReadActivePhase: %v", err)
	}
	if !strings.Contains(got, "NO HAY FASES PENDIENTES") {
		t.Errorf("expected completion message, got: %q", got)
	}
}

func TestReadActivePhase_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadActivePhase(dir)
	if err == nil {
		t.Error("expected error for missing master_plan.md")
	}
}

func TestSetActiveTenant_NamespacesMemexBucket(t *testing.T) {
	// Reset to empty first
	SetActiveTenant("")
	defaultBucket := memexBucketName()
	if defaultBucket != memexBucket {
		t.Errorf("default bucket should be %q, got %q", memexBucket, defaultBucket)
	}

	SetActiveTenant("tenant-42")
	namespaced := memexBucketName()
	if !strings.Contains(namespaced, "tenant-42") {
		t.Errorf("namespaced bucket should contain tenant ID, got %q", namespaced)
	}
	if !strings.HasPrefix(namespaced, memexBucket) {
		t.Errorf("namespaced bucket should start with %q, got %q", memexBucket, namespaced)
	}

	// Cleanup
	SetActiveTenant("")
}

func TestGetPlannerState_EmptyQueue(t *testing.T) {
	dir := t.TempDir()
	if err := InitPlanner(dir); err != nil {
		t.Fatalf("InitPlanner: %v", err)
	}
	pending, completed := GetPlannerState()
	if pending != 0 {
		t.Errorf("expected 0 pending tasks on empty queue, got %d", pending)
	}
	if completed != 0 {
		t.Errorf("expected 0 completed tasks on empty queue, got %d", completed)
	}
}

func TestGetPlannerState_AfterEnqueue(t *testing.T) {
	dir := t.TempDir()
	if err := InitPlanner(dir); err != nil {
		t.Fatalf("InitPlanner: %v", err)
	}

	tasks := []SRETask{
		{Description: "Task 1", TargetFile: "file1.go"},
		{Description: "Task 2", TargetFile: "file2.go"},
	}
	if err := EnqueueTasks(tasks); err != nil {
		t.Fatalf("EnqueueTasks: %v", err)
	}

	pending, _ := GetPlannerState()
	if pending != 2 {
		t.Errorf("expected 2 pending tasks, got %d", pending)
	}
}

func TestReadActivePhase_EmptyPlan(t *testing.T) {
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Plan with no tasks at all
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte("# Master Plan\n\nEmpty.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadActivePhase(dir)
	if err != nil {
		t.Fatalf("ReadActivePhase: %v", err)
	}
	if !strings.Contains(got, "NO HAY FASES PENDIENTES") {
		t.Errorf("expected no-pending message for empty plan, got: %q", got)
	}
}
