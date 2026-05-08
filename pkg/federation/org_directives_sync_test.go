package federation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeOrgDirective(t *testing.T, orgDir, name, content string) {
	t.Helper()
	dir := filepath.Join(orgDir, "knowledge", "directives")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

// TestSyncOrgDirectives_CopiesAndSkipsIdentical verifies the mirror writes
// new files and skips identical ones on re-run (idempotency). [355.B]
func TestSyncOrgDirectives_CopiesAndSkipsIdentical(t *testing.T) {
	orgDir := t.TempDir()
	ws := t.TempDir()
	writeOrgDirective(t, orgDir, "soc2-compliance.md", "Do X when Y.")
	writeOrgDirective(t, orgDir, "commit-conventions.md", "Feat(scope): <msg>")

	r, err := SyncOrgDirectivesToWorkspace(orgDir, ws)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if r.Copied != 2 {
		t.Errorf("first sync Copied = %d, want 2", r.Copied)
	}
	if r.Skipped != 0 {
		t.Errorf("first sync Skipped = %d, want 0", r.Skipped)
	}

	// Destination files exist with `org-` prefix.
	for _, basename := range []string{"org-soc2-compliance.md", "org-commit-conventions.md"} {
		full := filepath.Join(ws, ".claude", "rules", basename)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected %s to exist: %v", full, err)
		}
	}

	// Second sync is idempotent — all Skipped.
	r2, err := SyncOrgDirectivesToWorkspace(orgDir, ws)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if r2.Copied != 0 {
		t.Errorf("second sync Copied = %d, want 0 (idempotent)", r2.Copied)
	}
	if r2.Skipped != 2 {
		t.Errorf("second sync Skipped = %d, want 2", r2.Skipped)
	}
}

// TestSyncOrgDirectives_DriftIsOverwrittenBySource verifies that when the org
// source changes, the workspace copy is updated. Source wins conflicts. [355.B]
func TestSyncOrgDirectives_DriftIsOverwrittenBySource(t *testing.T) {
	orgDir := t.TempDir()
	ws := t.TempDir()
	writeOrgDirective(t, orgDir, "auth-policy.md", "v1: always TLS")

	if _, err := SyncOrgDirectivesToWorkspace(orgDir, ws); err != nil {
		t.Fatal(err)
	}

	// Update source.
	writeOrgDirective(t, orgDir, "auth-policy.md", "v2: always TLS + audit log")

	r, err := SyncOrgDirectivesToWorkspace(orgDir, ws)
	if err != nil {
		t.Fatal(err)
	}
	if r.Copied != 1 {
		t.Errorf("drift re-sync Copied = %d, want 1", r.Copied)
	}

	got, _ := os.ReadFile(filepath.Join(ws, ".claude", "rules", "org-auth-policy.md"))
	if string(got) != "v2: always TLS + audit log" {
		t.Errorf("destination not updated: %q", got)
	}
}

// TestSyncOrgDirectives_MissingDirReturnsSentinel verifies the graceful no-op
// when the org has no directives/ folder yet. [355.B]
func TestSyncOrgDirectives_MissingDirReturnsSentinel(t *testing.T) {
	orgDir := t.TempDir()
	ws := t.TempDir()
	// No directives subdir created.

	_, err := SyncOrgDirectivesToWorkspace(orgDir, ws)
	if !errors.Is(err, ErrOrgDirectivesDirMissing) {
		t.Errorf("expected ErrOrgDirectivesDirMissing, got: %v", err)
	}
}

// TestSyncOrgDirectives_DetectOrphans verifies that workspace `.claude/rules/`
// entries with `org-` prefix but no matching source are surfaced in
// OrphansDetected. [355.B]
func TestSyncOrgDirectives_DetectOrphans(t *testing.T) {
	orgDir := t.TempDir()
	ws := t.TempDir()
	writeOrgDirective(t, orgDir, "current.md", "live rule")

	// Pre-seed a stale org-*.md in the workspace with no source.
	rulesDir := filepath.Join(ws, ".claude", "rules")
	if err := os.MkdirAll(rulesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rulesDir, "org-removed.md"), []byte("stale"), 0o640); err != nil {
		t.Fatal(err)
	}

	r, err := SyncOrgDirectivesToWorkspace(orgDir, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.OrphansDetected) != 1 || r.OrphansDetected[0] != "org-removed.md" {
		t.Errorf("OrphansDetected = %v, want [org-removed.md]", r.OrphansDetected)
	}
}

// TestSyncOrgDirectives_IgnoresNonMarkdown verifies non-.md files in the
// directives folder are skipped. [355.B]
func TestSyncOrgDirectives_IgnoresNonMarkdown(t *testing.T) {
	orgDir := t.TempDir()
	ws := t.TempDir()
	dir := filepath.Join(orgDir, "knowledge", "directives")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "real.md"), []byte("ok"), 0o640)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("should skip"), 0o640)
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("skip too"), 0o640)

	r, err := SyncOrgDirectivesToWorkspace(orgDir, ws)
	if err != nil {
		t.Fatal(err)
	}
	if r.Copied != 1 {
		t.Errorf("Copied = %d, want 1 (only .md)", r.Copied)
	}

	for _, name := range []string{"org-notes.txt", "org-config.yaml"} {
		if _, err := os.Stat(filepath.Join(ws, ".claude", "rules", name)); err == nil {
			t.Errorf("non-.md file should not have been copied: %s", name)
		}
	}
}

// TestSyncOrgDirectives_EmptyArgsError verifies input validation. [355.B]
func TestSyncOrgDirectives_EmptyArgsError(t *testing.T) {
	if _, err := SyncOrgDirectivesToWorkspace("", "/tmp"); err == nil {
		t.Error("expected error on empty orgDir")
	}
	if _, err := SyncOrgDirectivesToWorkspace("/tmp", ""); err == nil {
		t.Error("expected error on empty workspaceDir")
	}
}
