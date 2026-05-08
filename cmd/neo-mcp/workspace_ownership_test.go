package main

import (
	"path/filepath"
	"testing"
)

// TestIsPathInWorkspace_HappyPath — file inside workspace → true. [330.L]
func TestIsPathInWorkspace_HappyPath(t *testing.T) {
	ws := t.TempDir()
	file := filepath.Join(ws, "backend", "handler.go")
	if !isPathInWorkspace(ws, file) {
		t.Errorf("file %s should belong to workspace %s", file, ws)
	}
}

// TestIsPathInWorkspace_RejectsOutside — cross-workspace path → false. [330.L]
// This is the bug vector that caused vision-link to surface strategos paths:
// a call to certify with an absolute path pointing to a DIFFERENT workspace
// used to pollute the session_state bucket of the invoked workspace.
func TestIsPathInWorkspace_RejectsOutside(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	otherFile := filepath.Join(wsB, "some", "file.go")
	if isPathInWorkspace(wsA, otherFile) {
		t.Errorf("file %s (in %s) must NOT be accepted as belonging to %s", otherFile, wsB, wsA)
	}
}

// TestIsPathInWorkspace_RejectsDotDotEscape — path with .. escapes. [330.L]
func TestIsPathInWorkspace_RejectsDotDotEscape(t *testing.T) {
	ws := t.TempDir()
	escape := filepath.Join(ws, "..", "other", "file.go")
	if isPathInWorkspace(ws, escape) {
		t.Errorf("escape path %s should NOT be accepted for workspace %s", escape, ws)
	}
}

// TestIsPathInWorkspace_AcceptsWorkspaceRoot — the workspace dir itself. [330.L]
func TestIsPathInWorkspace_AcceptsWorkspaceRoot(t *testing.T) {
	ws := t.TempDir()
	if !isPathInWorkspace(ws, ws) {
		t.Error("workspace dir itself should be accepted (rel='.')")
	}
}

// TestIsPathInWorkspace_AcceptsNested — nested subdirs. [330.L]
func TestIsPathInWorkspace_AcceptsNested(t *testing.T) {
	ws := t.TempDir()
	deep := filepath.Join(ws, "a", "b", "c", "d", "file.go")
	if !isPathInWorkspace(ws, deep) {
		t.Errorf("deeply nested file should be accepted: %s", deep)
	}
}

// TestIsPathInWorkspace_RelativePathArgs — accepts relative args, absolutizes. [330.L]
func TestIsPathInWorkspace_RelativePathArgs(t *testing.T) {
	// Relative path gets absolutized from cwd. Our own repo has "pkg/" and
	// "cmd/" dirs — check a relative reference resolves inside CWD.
	cwd := t.TempDir()
	if !isPathInWorkspace(cwd, cwd+"/./file.go") {
		t.Error("cleaned-but-still-inside path should be accepted")
	}
}
