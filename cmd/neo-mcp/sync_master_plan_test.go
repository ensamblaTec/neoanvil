package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncMasterPlanMarksReferencedTask verifies that scripts/git-hooks/sync-master-plan.sh
// flips "- [ ] **134.C.1** ..." to "- [x] **134.C.1** ..." when the commit
// message references the task ID. [134.A.4]
func TestSyncMasterPlanMarksReferencedTask(t *testing.T) {
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0755); err != nil {
		t.Fatal(err)
	}
	plan := `# Plan
- [ ] **999.A.1** dummy task one
- [ ] **999.A.2** dummy task two
- [ ] **999.B.1** unrelated task
`
	planPath := filepath.Join(neoDir, "master_plan.md")
	if err := os.WriteFile(planPath, []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}

	script := repoRootForTest(t) + "/scripts/git-hooks/sync-master-plan.sh"
	commitMsg := "feat(test): 999.A.1 dummy task — wraps up phase A.1"
	out, err := runHookScript(t, script, commitMsg, dir)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, "- [x] **999.A.1** dummy task one") {
		t.Errorf("expected task 999.A.1 marked done, got plan:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "- [ ] **999.A.2** dummy task two") {
		t.Errorf("999.A.2 should remain unchecked (not in commit msg), got:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "- [ ] **999.B.1** unrelated task") {
		t.Errorf("999.B.1 should remain unchecked, got:\n%s", gotStr)
	}
	if !strings.Contains(out, "marked 1 task(s) as done") {
		t.Errorf("expected stderr log 'marked 1 task(s) as done', got %q", out)
	}
}

// TestSyncMasterPlanSilentWhenNoTokens verifies the script is silent on a
// commit message without recognisable task IDs. [134.A.4]
func TestSyncMasterPlanSilentWhenNoTokens(t *testing.T) {
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0755); err != nil {
		t.Fatal(err)
	}
	plan := "# Plan\n- [ ] **999.A.1** task\n"
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}

	script := repoRootForTest(t) + "/scripts/git-hooks/sync-master-plan.sh"
	out, err := runHookScript(t, script, "fix(jira): MCPI-42 unrelated bug", dir)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "marked") {
		t.Errorf("expected no log output, got %q", out)
	}
}

// TestSyncMasterPlanBodyReferenceIgnored verifies that task IDs mentioned only
// in the commit body (e.g. inside regex examples or paths) do NOT trigger a
// mark. Real-world bug found 2026-04-30: a body containing "(catches 134.C.1,
// 130.4.2, 132.A, 128.1)" auto-checked 130.4.2 even though the commit closed
// 134.A. Subject-only parsing is the fix. [134.A]
func TestSyncMasterPlanBodyReferenceIgnored(t *testing.T) {
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0755); err != nil {
		t.Fatal(err)
	}
	plan := "# Plan\n- [ ] **999.A.1** target\n- [ ] **888.B.2** trap\n"
	planPath := filepath.Join(neoDir, "master_plan.md")
	if err := os.WriteFile(planPath, []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}

	script := repoRootForTest(t) + "/scripts/git-hooks/sync-master-plan.sh"
	// Subject closes 999.A.1, body mentions 888.B.2 incidentally.
	commitMsg := "feat(test): 999.A.1 closes phase A\n\nNote: regex catches 888.B.2 as example."
	out, err := runHookScript(t, script, commitMsg, dir)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, "- [x] **999.A.1** target") {
		t.Errorf("999.A.1 (in subject) should be marked done, got:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "- [ ] **888.B.2** trap") {
		t.Errorf("888.B.2 (only in body) MUST remain unchecked, got:\n%s", gotStr)
	}
}

// TestSyncMasterPlanIdempotent verifies running the script twice on the same
// message produces the same final state and only logs once. [134.A.4]
func TestSyncMasterPlanIdempotent(t *testing.T) {
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0755); err != nil {
		t.Fatal(err)
	}
	plan := "# Plan\n- [ ] **999.A.1** task\n"
	planPath := filepath.Join(neoDir, "master_plan.md")
	if err := os.WriteFile(planPath, []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}

	script := repoRootForTest(t) + "/scripts/git-hooks/sync-master-plan.sh"
	commitMsg := "feat(test): 999.A.1 done"
	if _, err := runHookScript(t, script, commitMsg, dir); err != nil {
		t.Fatal(err)
	}
	out2, err := runHookScript(t, script, commitMsg, dir)
	if err != nil {
		t.Fatalf("second run failed: %v\n%s", err, out2)
	}
	got, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "- [x] **999.A.1**") {
		t.Errorf("expected task still marked done, got:\n%s", got)
	}
	if strings.Contains(out2, "marked") {
		t.Errorf("second run should be silent (already done), got %q", out2)
	}
}

// runHookScript invokes the bash script with the given commit message and
// repo root, returning stderr+stdout combined.
func runHookScript(t *testing.T, scriptPath, commitMsg, repoRoot string) (string, error) {
	t.Helper()
	//nolint:gosec // G204-LITERAL-BIN: bash binary literal, args validated test-only.
	cmd := exec.Command("bash", scriptPath, commitMsg, repoRoot)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// repoRootForTest walks up from the current test directory until it finds
// the repo root (marker: scripts/git-hooks/sync-master-plan.sh exists).
func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for dir := wd; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "scripts/git-hooks/sync-master-plan.sh")); err == nil {
			return dir
		}
	}
	t.Fatalf("could not locate repo root from %s", wd)
	return ""
}
