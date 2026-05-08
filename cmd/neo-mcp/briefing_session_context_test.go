package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitClean verifies that a repo with no uncommitted changes returns "clean". [127.1]
func TestGitClean(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "test@test.com")
	mustRunGit(t, dir, "config", "user.name", "Test")
	f := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(f, []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", "readme.txt")
	mustRunGit(t, dir, "commit", "--no-gpg-sign", "-m", "init")

	got := populateGitState(dir)
	if !strings.Contains(got, "clean") {
		t.Errorf("expected 'clean' in git state, got %q", got)
	}
}

// TestGitDirty verifies that uncommitted changes report "N changes". [127.1]
func TestGitDirty(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "test@test.com")
	mustRunGit(t, dir, "config", "user.name", "Test")
	f := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(f, []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", "readme.txt")
	mustRunGit(t, dir, "commit", "--no-gpg-sign", "-m", "init")
	// Add an untracked dirty file.
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}

	got := populateGitState(dir)
	if !strings.Contains(got, "changes") {
		t.Errorf("expected 'changes' in git state for dirty repo, got %q", got)
	}
}

// TestGitUnreachable verifies fail-open when the directory is not a git repo. [127.1]
func TestGitUnreachable(t *testing.T) {
	dir := t.TempDir() // plain dir, no .git
	got := populateGitState(dir)
	if got != "" {
		t.Errorf("expected empty string for non-git dir, got %q", got)
	}
}

// TestHooksPresent verifies that an existing post-commit hook shows ✓. [127.2]
func TestHooksPresent(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".git/hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-commit"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	got := populateToolingState(dir)
	if !strings.Contains(got, "post-commit:✓") {
		t.Errorf("expected 'post-commit:✓' when hook present, got %q", got)
	}
}

// TestHooksAbsent verifies that a missing post-commit hook shows ✗. [127.2]
func TestHooksAbsent(t *testing.T) {
	dir := t.TempDir() // no .git/hooks directory at all
	got := populateToolingState(dir)
	if !strings.Contains(got, "post-commit:✗") {
		t.Errorf("expected 'post-commit:✗' when hook absent, got %q", got)
	}
}

// TestHooksSymlink verifies that a symlinked post-commit hook is detected. [134.C.2]
// Reproduces the bug where Stat would follow the symlink and fail when the
// target was momentarily unreachable post-rebuild.
func TestHooksSymlink(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".git/hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}
	scriptsDir := filepath.Join(dir, "scripts/git-hooks")
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(scriptsDir, "post-commit")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// Relative symlink, exactly like make install-git-hooks creates.
	link := filepath.Join(hooksDir, "post-commit")
	if err := os.Symlink("../../scripts/git-hooks/post-commit", link); err != nil {
		t.Fatal(err)
	}

	got := populateToolingState(dir)
	if !strings.Contains(got, "post-commit:✓") {
		t.Errorf("expected 'post-commit:✓' when hook is symlinked, got %q", got)
	}

	// Even with a broken symlink (target removed), the detector must still
	// report ✓ — the question is whether the hook is installed, not whether
	// the target is currently reachable.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	got = populateToolingState(dir)
	if !strings.Contains(got, "post-commit:✓") {
		t.Errorf("expected 'post-commit:✓' with broken symlink, got %q", got)
	}
}

// TestRecentEpics3 verifies that the last 3 closed épicas are returned. [127.3]
func TestRecentEpics3(t *testing.T) {
	dir := t.TempDir()
	neoDir := filepath.Join(dir, ".neo")
	if err := os.MkdirAll(neoDir, 0755); err != nil {
		t.Fatal(err)
	}
	plan := `
## ÉPICA 10 — First closed
- [x] **10.1** task one
- [x] **10.2** task two

## ÉPICA 20 — Second closed
- [x] **20.1** done

## ÉPICA 30 — Third closed
- [x] **30.1** done
- [x] **30.2** done

## ÉPICA 40 — Fourth closed
- [x] **40.1** done

## ÉPICA 50 — Open épica
- [ ] **50.1** still open
- [x] **50.2** done
`
	if err := os.WriteFile(filepath.Join(neoDir, "master_plan.md"), []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}

	got := populateRecentEpics(dir)
	// Should return last 3 closed: 20, 30, 40.
	for _, want := range []string{"20", "30", "40"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected epic %s in recent_epics, got %q", want, got)
		}
	}
	// 10 is the 4th oldest — should not appear.
	if strings.HasPrefix(got, "last_epics: 10") || strings.Contains(got, ", 10") {
		t.Errorf("epic 10 should not appear in last 3 closed, got %q", got)
	}
	// 50 is open — must not appear.
	if strings.Contains(got, "50") {
		t.Errorf("open epic 50 should not appear, got %q", got)
	}
}

// mustRunGit runs a git subcommand in dir, fataling the test on error.
func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	//nolint:gosec // G204-LITERAL-BIN: fixed "git" binary, test-only helper
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
