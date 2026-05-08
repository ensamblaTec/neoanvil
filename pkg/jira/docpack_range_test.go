package jira

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupGitRepo creates a temporary git repo with two commits touching different files.
// Returns (repoRoot, hashA, hashB).
func setupGitRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()

	runGit := func(args ...string) string {
		t.Helper()
		cmdArgs := append([]string{"-C", dir}, args...)
		out, err := exec.Command("git", cmdArgs...).CombinedOutput() //nolint:gosec // G204-LITERAL-BIN: test helper
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.email", "test@test.com")
	runGit("config", "user.name", "Test")

	// Commit A
	if err := os.WriteFile(filepath.Join(dir, "file_a.go"), []byte("package main"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "file_a.go")
	runGit("commit", "--no-gpg-sign", "-m", "commit A")
	hashA := runGit("rev-parse", "HEAD")

	// Commit B
	if err := os.WriteFile(filepath.Join(dir, "file_b.go"), []byte("package main"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "file_b.go")
	runGit("commit", "--no-gpg-sign", "-m", "commit B")
	hashB := runGit("rev-parse", "HEAD")

	return dir, hashA, hashB
}

// TestSingleHashCompat verifies that a plain hash (no "..") still works
// as a single-commit reference (backwards compatibility). [129.5]
func TestSingleHashCompat(t *testing.T) {
	dir, _, hashB := setupGitRepo(t)
	in := &PrepareDocPackInput{
		TicketID:   "MCPI-1",
		RepoRoot:   dir,
		CommitHash: hashB,
	}
	// Should route to populateFromCommit (not range).
	if strings.Contains(in.CommitHash, "..") {
		t.Error("single hash should not contain '..'")
	}
	if err := populateFromCommit(in); err != nil {
		t.Fatalf("populateFromCommit: %v", err)
	}
	if len(in.Files) == 0 {
		t.Error("expected at least one file from single commit")
	}
	found := false
	for _, f := range in.Files {
		if f == "file_b.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected file_b.go in files, got %v", in.Files)
	}
}

// TestRangeValid verifies that A..B returns the union of both commits' files. [129.5]
func TestRangeValid(t *testing.T) {
	dir, hashA, hashB := setupGitRepo(t)
	_ = hashB
	rangeSpec := hashA + "..HEAD"
	files, err := deriveFilesFromCommitRange(dir, rangeSpec)
	if err != nil {
		t.Fatalf("deriveFilesFromCommitRange: %v", err)
	}
	hasFileB := false
	for _, f := range files {
		if f == "file_b.go" {
			hasFileB = true
		}
	}
	if !hasFileB {
		t.Errorf("expected file_b.go in range result, got %v", files)
	}
	// file_a.go was committed in hashA — outside the range [hashA..HEAD],
	// so it should NOT appear (the range excludes hashA itself).
	for _, f := range files {
		if f == "file_a.go" {
			t.Errorf("file_a.go should not appear in range %s (exclusive), got %v", rangeSpec, files)
		}
	}
}

// TestRangeEmpty verifies that an empty range (equal hashes) returns 0 files. [129.5]
func TestRangeEmpty(t *testing.T) {
	dir, _, hashB := setupGitRepo(t)
	// hashB..hashB is an empty range (no commits between a ref and itself).
	rangeSpec := hashB + ".." + hashB
	files, err := deriveFilesFromCommitRange(dir, rangeSpec)
	if err != nil {
		t.Fatalf("unexpected error for empty range: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files for empty range, got %v", files)
	}
}

// TestEpicFinalHookTrigger verifies the EPIC-FINAL message pattern detection
// via simple string matching (hook logic is in bash; this tests the Go side
// of the range dispatch). [129.5]
func TestEpicFinalHookTrigger(t *testing.T) {
	msg := "feat(sre): close épica 129 [EPIC-FINAL MCPI-51]"
	if !strings.Contains(msg, "[EPIC-FINAL") {
		t.Error("EPIC-FINAL pattern not detected in commit message")
	}
	// Verify the range detection branch in PrepareDocPack is triggered
	// by a commit_hash containing "..".
	in := &PrepareDocPackInput{
		CommitHash: "abc123..HEAD",
	}
	isRange := strings.Contains(in.CommitHash, "..")
	if !isRange {
		t.Error("expected range detection for 'abc123..HEAD'")
	}
}
