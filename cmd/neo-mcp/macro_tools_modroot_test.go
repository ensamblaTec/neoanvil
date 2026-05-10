package main

// Regression test for Nexus debt T001 (CERTIFY-CWD-BUG, P0).
//
// Operator-reported symptom: in strategos (Go module rooted at
// `backend/go.mod`, but workspace marker `neo.yaml` at workspace
// root), neo_sre_certify_mutation ran `go test` with cmd.Dir set
// to the projectRootOf result (workspace root containing neo.yaml).
// Go could not find go.mod there → "go.mod file not found in
// current directory or any parent directory" → 100% bypass rate
// over ~30 sessions.
//
// Fix: introduce goModRootOf() — walks up looking ONLY for go.mod,
// regardless of neo.yaml. Used at the cmd.Dir of go test/build/list.
// projectRootOf is still used for non-Go contexts (python, polyglot
// module builds) where neo.yaml is the right anchor.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGoModRootOf_PrefersGoModOverNeoYaml replicates the strategos
// monorepo layout exactly: workspace root has neo.yaml, but the Go
// module lives one level deeper at backend/go.mod. The fix must
// return the backend/ dir, not the workspace root.
func TestGoModRootOf_PrefersGoModOverNeoYaml(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(path, body string) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// Strategos-style layout:
	//   <root>/neo.yaml         ← workspace marker (NOT a Go module)
	//   <root>/go.work          ← multi-module workspace declaration
	//   <root>/backend/go.mod   ← actual Go module root
	//   <root>/backend/internal/services/foo.go  ← mutated file
	mustWrite("neo.yaml", "mode: pair\n")
	mustWrite("go.work", "go 1.26\nuse ./backend\n")
	mustWrite("backend/go.mod", "module example/strategos/backend\n\ngo 1.26\n")
	mustWrite("backend/internal/services/foo.go", "package services\n")

	target := filepath.Join(root, "backend/internal/services/foo.go")
	got := goModRootOf(target)
	want := filepath.Join(root, "backend")
	if got != want {
		t.Errorf("goModRootOf(%q):\n  got  %q\n  want %q (the backend/ dir with go.mod, NOT workspace root with neo.yaml)",
			target, got, want)
	}
}

// TestGoModRootOf_FallsBackToFileDirWhenNoGoMod confirms graceful
// degradation when a file isn't inside any Go module — must not
// return the empty string or panic.
func TestGoModRootOf_FallsBackToFileDirWhenNoGoMod(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lonely.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := goModRootOf(target)
	if got != root {
		t.Errorf("goModRootOf(no-go-mod):\n  got  %q\n  want %q (file's dir as last resort)",
			got, root)
	}
}

// TestGoModRootOf_StopsAtNearestGoMod ensures we don't walk past the
// first go.mod we find. In nested-module layouts (rare but legal),
// the inner module's go.mod wins over an outer one.
func TestGoModRootOf_StopsAtNearestGoMod(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(path, body string) {
		full := filepath.Join(root, path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(body), 0o644)
	}
	// Outer go.mod at root, inner go.mod at sub/, file deeper still.
	// goModRootOf(file) MUST return sub/ — the nearest ancestor.
	mustWrite("go.mod", "module outer\n\ngo 1.26\n")
	mustWrite("sub/go.mod", "module inner\n\ngo 1.26\n")
	mustWrite("sub/pkg/x.go", "package pkg\n")
	target := filepath.Join(root, "sub/pkg/x.go")
	got := goModRootOf(target)
	want := filepath.Join(root, "sub")
	if got != want {
		t.Errorf("goModRootOf nested:\n  got  %q\n  want %q", got, want)
	}
}

// TestProjectRootOf_StillPrefersNeoYaml documents that projectRootOf
// retains its old semantics. Both helpers exist on purpose: pick
// projectRootOf for workspace-anchored contexts (reading neo.yaml,
// resolving polyglot module paths), pick goModRootOf for go-toolchain
// commands where the module root is the only correct cwd.
func TestProjectRootOf_StillPrefersNeoYaml(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(path, body string) {
		full := filepath.Join(root, path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(body), 0o644)
	}
	mustWrite("neo.yaml", "mode: pair\n")
	mustWrite("backend/go.mod", "module test\n\ngo 1.26\n")
	mustWrite("backend/foo.go", "package foo\n")

	target := filepath.Join(root, "backend/foo.go")
	got := projectRootOf(target)
	if got != root {
		t.Errorf("projectRootOf:\n  got  %q\n  want %q (workspace root with neo.yaml)",
			got, root)
	}
}
