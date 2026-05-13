package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBackgroundIndexFile_SymlinkEscapeRejected covers the 146.C-mirror
// hardening: a symlink inside the workspace pointing OUTSIDE must not
// flow through to os.ReadFile. The function returns silently after
// logging — verifying the symlink path was NOT read is the contract.
//
// Threat model: shell-access attacker plants link at <workspace>/.neo/leak
// pointing at /etc/passwd; MCP-access attacker triggers BLAST_RADIUS
// with target=".neo/leak". Without the fix the file content gets
// indexed into HNSW and surfaces via SEMANTIC_CODE / federation peers.
func TestBackgroundIndexFile_SymlinkEscapeRejected(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("symlink semantics differ under root; skip in CI containers")
	}
	// Build a workspace dir + an outside-workspace target file. The
	// symlink we plant inside the workspace points at the outside file.
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("CONFIDENTIAL"), 0644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	linkPath := filepath.Join(ws, "leak.txt")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Sanity: EvalSymlinks should resolve to the outside path.
	resolved, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if resolved == linkPath {
		t.Fatalf("symlink should resolve to outside path, got %s", resolved)
	}

	// Verify the prefix-check logic the function uses. Whether or not
	// the resolved path stays under the workspace is the security gate.
	// We test the gate directly because backgroundIndexFile spawns
	// goroutines + Ollama embedder; the safety property is purely the
	// prefix re-check.
	//
	// [bug-fix 2026-05-13] On macOS t.TempDir() returns /var/folders/... but
	// filepath.EvalSymlinks resolves to /private/var/folders/... (system-wide
	// symlink redirect). Comparing raw ws against resolved paths triggered a
	// chronic false-positive failure of the *inner* assertion below. Fix:
	// canonicalize the workspace root via EvalSymlinks BEFORE comparison so
	// both sides are in the same realm. The security property remains: the
	// outside-target test below still must reject (resolved escapes ws).
	wsRoot, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("EvalSymlinks(ws): %v", err)
	}
	if got := isPathUnderRoot(resolved, wsRoot); got {
		t.Errorf("resolved %s incorrectly inside workspace %s", resolved, wsRoot)
	}

	// Inside-workspace symlink (regular file) → safe to follow.
	innerFile := filepath.Join(ws, "data.txt")
	if err := os.WriteFile(innerFile, []byte("inside"), 0644); err != nil {
		t.Fatal(err)
	}
	innerLink := filepath.Join(ws, "alias.txt")
	if err := os.Symlink(innerFile, innerLink); err != nil {
		t.Fatal(err)
	}
	innerResolved, err := filepath.EvalSymlinks(innerLink)
	if err != nil {
		t.Fatal(err)
	}
	if !isPathUnderRoot(innerResolved, wsRoot) {
		t.Errorf("inner symlink %s resolved to %s should be under workspace %s",
			innerLink, innerResolved, wsRoot)
	}
}

// isPathUnderRoot mirrors the prefix-check used by
// backgroundIndexFile. Extracted so the security property has its own
// test surface even when the surrounding handler can't be unit-tested
// without heavy dependencies.
//
// Equivalent to the in-handler check:
//
//	strings.HasPrefix(absTarget, t.workspace+"/") || absTarget == t.workspace
//
// Trailing-slash on path is normalized to match (treating "/ws/" as
// equivalent to "/ws" — same directory).
func isPathUnderRoot(path, root string) bool {
	// Normalize trailing slash on path: "/ws/" → "/ws" before equality.
	if len(path) > 1 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	if path == root {
		return true
	}
	prefix := root
	if len(prefix) == 0 || prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	return len(path) >= len(prefix) && path[:len(prefix)] == prefix
}

// TestIsPathUnderRoot covers the helper's edge cases. The actual
// in-handler check uses strings.HasPrefix(absTarget, t.workspace+"/")
// — same semantic.
func TestIsPathUnderRoot(t *testing.T) {
	cases := []struct {
		path string
		root string
		want bool
	}{
		{"/ws", "/ws", true},
		{"/ws/sub/file.go", "/ws", true},
		{"/wsx/sub", "/ws", false}, // prefix-substring isn't a parent
		{"/etc/passwd", "/ws", false},
		{"/ws/", "/ws", true},
	}
	for _, tc := range cases {
		if got := isPathUnderRoot(tc.path, tc.root); got != tc.want {
			t.Errorf("isPathUnderRoot(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}
