package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunGoBouncer_BatchPkgDedupSkip covers the [Phase 2 MV+ / Speed-First]
// short-circuit. When testedPkgs already contains the (goModRoot|pkgDir)
// key for this file, runGoBouncer MUST return ("", nil) before touching
// `go list` / `go test`. We prove the short-circuit by pointing at a
// pkgPath that has no Go files: if dedup didn't fire, `go list` would
// reach the cmd subprocess and return a non-empty diagnostic. Empty
// result == dedup gate triggered.
func TestRunGoBouncer_BatchPkgDedupSkip(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"),
		[]byte("module example.com/test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	pkgDir := filepath.Join(workspace, "pkg", "foo")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	file := filepath.Join(pkgDir, "x.go")

	pkgKey := goModRootOf(file) + "|" + filepath.Dir(file)
	testedPkgs := map[string]struct{}{pkgKey: {}}

	rollbackCalls := 0
	rollback := func() { rollbackCalls++ }

	tool := &CertifyMutationTool{}
	msg, bounce := tool.runGoBouncer(
		context.Background(),
		file,
		[]byte("package foo\n"),
		"FEATURE_ADD",
		rollback,
		testedPkgs,
	)
	if msg != "" {
		t.Errorf("dedup hit must short-circuit with empty msg, got: %q", msg)
	}
	if bounce != nil {
		t.Errorf("dedup hit must return nil bounce, got: %+v", bounce)
	}
	if rollbackCalls != 0 {
		t.Errorf("dedup short-circuit must not invoke rollback, got %d calls", rollbackCalls)
	}
}

// TestRunGoBouncer_PkgKeyShape locks the (goModRoot|pkgDir) key contract.
// Both halves must be non-empty and separated by `|` so two pkgs with the
// same dir name in different modules cannot alias. If the key shape ever
// drifts, the dedup map silently collides — this test makes that fail
// loud.
func TestRunGoBouncer_PkgKeyShape(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"),
		[]byte("module example.com/test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	file := filepath.Join(workspace, "pkg", "foo", "x.go")

	key := goModRootOf(file) + "|" + filepath.Dir(file)
	if !strings.Contains(key, "|") {
		t.Errorf("pkgKey must use | separator, got: %q", key)
	}
	left, right, _ := strings.Cut(key, "|")
	if left == "" || right == "" {
		t.Errorf("both halves must be non-empty, got left=%q right=%q", left, right)
	}
	if left == right {
		t.Errorf("goModRoot and pkgDir must differ when file is inside a subpkg, got both=%q", left)
	}
}

// TestRunGoBouncer_PkgKeyDifferentModules covers the cross-module
// disambiguation: two files in pkg dirs that happen to share the same
// LEAF name but live under different go.mod roots must hash to distinct
// keys, otherwise a tested pkg in module A would silently mark module B
// as also-tested. Different modules → different keys = safety.
func TestRunGoBouncer_PkgKeyDifferentModules(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	for _, ws := range []string{wsA, wsB} {
		if err := os.WriteFile(filepath.Join(ws, "go.mod"),
			[]byte("module example.com/test\n\ngo 1.22\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(ws, "pkg", "foo"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fileA := filepath.Join(wsA, "pkg", "foo", "x.go")
	fileB := filepath.Join(wsB, "pkg", "foo", "x.go")

	keyA := goModRootOf(fileA) + "|" + filepath.Dir(fileA)
	keyB := goModRootOf(fileB) + "|" + filepath.Dir(fileB)
	if keyA == keyB {
		t.Errorf("cross-module pkgs with same leaf must have distinct keys, both=%q", keyA)
	}
}
