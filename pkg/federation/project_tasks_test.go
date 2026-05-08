package federation

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newProjectTaskDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	projDir := filepath.Join(d, ".neo-project")
	if err := os.MkdirAll(projDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return projDir
}

// TestAppendProjectTask_DefaultsAndPersistence verifies defaults + round-trip. [349.A]
func TestAppendProjectTask_DefaultsAndPersistence(t *testing.T) {
	dir := newProjectTaskDir(t)
	task, err := AppendProjectTask(dir, ProjectTask{Description: "refactor auth handler"})
	if err != nil {
		t.Fatalf("AppendProjectTask: %v", err)
	}
	if task.ID == "" {
		t.Error("expected generated ID")
	}
	if task.Status != "pending" {
		t.Errorf("Status = %q, want pending", task.Status)
	}
	if task.TargetWorkspace != "*" {
		t.Errorf("TargetWorkspace default = %q, want *", task.TargetWorkspace)
	}
	if task.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}

	all, err := ListProjectTasks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("ListProjectTasks = %d, want 1", len(all))
	}
}

// TestClaimProjectTask_WildcardTarget verifies "*" target is claimable by any
// workspace. [349.A]
func TestClaimProjectTask_WildcardTarget(t *testing.T) {
	dir := newProjectTaskDir(t)
	_, _ = AppendProjectTask(dir, ProjectTask{
		Description: "run big migration",
		TargetWorkspace: "*",
	})
	claimed, err := ClaimProjectTask(dir, "ws-alpha")
	if err != nil {
		t.Fatalf("claim by ws-alpha: %v", err)
	}
	if claimed.Status != "in_progress" || claimed.ClaimedBy != "ws-alpha" {
		t.Errorf("claim did not update state: %+v", claimed)
	}
}

// TestClaimProjectTask_SpecificTarget verifies only matching workspace can
// claim a task with explicit TargetWorkspace. [349.A]
func TestClaimProjectTask_SpecificTarget(t *testing.T) {
	dir := newProjectTaskDir(t)
	_, _ = AppendProjectTask(dir, ProjectTask{
		Description:     "frontend form fix",
		TargetWorkspace: "ws-frontend",
	})
	// Wrong workspace — should not claim.
	if _, err := ClaimProjectTask(dir, "ws-backend"); err != ErrProjectTaskNotFound {
		t.Errorf("wrong target claimed task: %v", err)
	}
	// Right workspace — claims.
	if _, err := ClaimProjectTask(dir, "ws-frontend"); err != nil {
		t.Errorf("correct target could not claim: %v", err)
	}
}

// TestClaimProjectTask_NoneAvailable returns ErrProjectTaskNotFound when all
// tasks are already claimed or empty. [349.A]
func TestClaimProjectTask_NoneAvailable(t *testing.T) {
	dir := newProjectTaskDir(t)
	// Empty queue.
	if _, err := ClaimProjectTask(dir, "ws-a"); err != ErrProjectTaskNotFound {
		t.Errorf("empty queue: %v", err)
	}
	// Queue with only in_progress tasks.
	tsk, _ := AppendProjectTask(dir, ProjectTask{Description: "x"})
	_, _ = ClaimProjectTask(dir, "ws-a")
	if _, err := ClaimProjectTask(dir, "ws-a"); err != ErrProjectTaskNotFound {
		t.Errorf("no-pending queue: %v", err)
	}
	_ = tsk
}

// TestCompleteProjectTask_Transition verifies in_progress → completed. [349.A]
func TestCompleteProjectTask_Transition(t *testing.T) {
	dir := newProjectTaskDir(t)
	tsk, _ := AppendProjectTask(dir, ProjectTask{Description: "bar"})
	_, _ = ClaimProjectTask(dir, "ws-a")
	if err := CompleteProjectTask(dir, tsk.ID, "done — 3 LOC fix"); err != nil {
		t.Fatalf("CompleteProjectTask: %v", err)
	}
	all, _ := ListProjectTasks(dir)
	if all[0].Status != "completed" || all[0].Result == "" {
		t.Errorf("expected completed status + result, got %+v", all[0])
	}
	if err := CompleteProjectTask(dir, "no-such", "x"); err != ErrProjectTaskNotFound {
		t.Errorf("unknown id: %v", err)
	}
}

// TestAppendProjectTask_ConcurrentClaim verifies only one of N concurrent
// claimers acquires a single task — LWW by mutex + file serialization. [349.A]
func TestAppendProjectTask_ConcurrentClaim(t *testing.T) {
	dir := newProjectTaskDir(t)
	_, _ = AppendProjectTask(dir, ProjectTask{Description: "race", TargetWorkspace: "*"})

	const claimers = 10
	var wg sync.WaitGroup
	winners := make(chan string, claimers)
	for i := range claimers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := ClaimProjectTask(dir, "claimer-"+string(rune('a'+i))); err == nil {
				winners <- "win"
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	count := 0
	for range winners {
		count++
	}
	if count != 1 {
		t.Errorf("expected exactly 1 winner, got %d", count)
	}
}

// TestAppendProjectTask_RendersTable verifies the markdown output contains the
// JSON block + rendered table. [349.A]
func TestAppendProjectTask_RendersTable(t *testing.T) {
	dir := newProjectTaskDir(t)
	_, _ = AppendProjectTask(dir, ProjectTask{
		Description:     "migrate DB schema V3",
		TargetWorkspace: "ws-backend",
		Role:            "backend",
	})
	raw, _ := os.ReadFile(filepath.Join(dir, projectTasksFile))
	s := string(raw)
	for _, needle := range []string{
		"# Project Task Queue",
		"neo-project-tasks-v1",
		"## Pending (1)",
		"migrate DB schema V3",
		"ws-backend",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("missing %q:\n%s", needle, s)
		}
	}
}
