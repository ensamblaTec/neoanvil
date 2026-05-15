package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// fakeTestWorkspace materialises a small workspace tree on disk so the
// same-package leg of testsImpactedBy (os.ReadDir-based) has real files
// to find. The dep-graph leg is exercised via an in-process WAL +
// SaveGraphEdges. [Phase 2 MV / Speed-First]
func fakeTestWorkspace(t *testing.T) (workspace string, wal *rag.WAL) {
	t.Helper()
	workspace = t.TempDir()

	// pkg/foo: prod x.go + same-package x_test.go
	mkdir := func(rel string) string {
		dir := filepath.Join(workspace, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		return dir
	}
	touch := func(rel, body string) {
		path := filepath.Join(workspace, rel)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	mkdir("pkg/foo")
	touch("pkg/foo/x.go", "package foo\n")
	touch("pkg/foo/x_test.go", "package foo\nimport \"testing\"\n")
	touch("pkg/foo/y.go", "package foo\n")

	// pkg/consumer: imports pkg/foo — both prod and test.
	mkdir("pkg/consumer")
	touch("pkg/consumer/c.go", "package consumer\n")
	touch("pkg/consumer/c_test.go", "package consumer\n")

	// WAL with a few hand-rolled edges. Real-world the resolver writes
	// these; for unit-testing we shape them directly.
	wal, err := rag.OpenWAL(filepath.Join(workspace, "wal.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	if err := rag.InitGraphRAG(wal); err != nil {
		t.Fatalf("InitGraphRAG: %v", err)
	}

	// cross-package: pkg/consumer/c_test.go depends on pkg/foo/x.go
	// (i.e. there's an edge c_test.go → x.go from the resolver).
	edges := []rag.GraphEdge{
		{SourceNode: "pkg/consumer/c.go", TargetNode: "pkg/foo/x.go"},
		{SourceNode: "pkg/consumer/c_test.go", TargetNode: "pkg/foo/x.go"},
	}
	if err := rag.SaveGraphEdges(wal, edges); err != nil {
		t.Fatalf("SaveGraphEdges: %v", err)
	}
	return workspace, wal
}

// TestTestsImpactedBy_SamePackage covers the dir-glob leg: editing
// pkg/foo/x.go must select pkg/foo/x_test.go even though there's no
// Go-import edge between same-package files.
func TestTestsImpactedBy_SamePackage(t *testing.T) {
	workspace, wal := fakeTestWorkspace(t)
	got := testsImpactedBy(wal, workspace, []string{"pkg/foo/x.go"})

	// Must include same-package test. May also include cross-package
	// test (consumer/c_test.go) via the dep-graph leg — that's fine.
	mustContain(t, got, "pkg/foo/x_test.go")
}

// TestTestsImpactedBy_CrossPackageDepGraph covers the dep-graph leg:
// editing pkg/foo/x.go must surface pkg/consumer/c_test.go because the
// resolver registered c_test.go → x.go as an edge.
func TestTestsImpactedBy_CrossPackageDepGraph(t *testing.T) {
	workspace, wal := fakeTestWorkspace(t)
	got := testsImpactedBy(wal, workspace, []string{"pkg/foo/x.go"})
	mustContain(t, got, "pkg/consumer/c_test.go")
}

// TestTestsImpactedBy_OnlyTestsReturned covers the filter: the dep-graph
// reverse-walk surfaces ALL importers (including prod c.go), but only
// _test.go files end up in the result.
func TestTestsImpactedBy_OnlyTestsReturned(t *testing.T) {
	workspace, wal := fakeTestWorkspace(t)
	got := testsImpactedBy(wal, workspace, []string{"pkg/foo/x.go"})

	for _, p := range got {
		if !endsWith(p, "_test.go") {
			t.Errorf("non-test file leaked into impacted set: %q", p)
		}
	}
	// And the prod consumer must NOT appear.
	for _, p := range got {
		if p == "pkg/consumer/c.go" {
			t.Errorf("prod file pkg/consumer/c.go must not be in impacted set")
		}
	}
}

// TestTestsImpactedBy_DedupAcrossSources covers batch mode: mutating two
// files in the same package must not double-count their shared test files.
func TestTestsImpactedBy_DedupAcrossSources(t *testing.T) {
	workspace, wal := fakeTestWorkspace(t)
	got := testsImpactedBy(wal, workspace, []string{"pkg/foo/x.go", "pkg/foo/y.go"})

	// x_test.go should appear exactly once.
	count := 0
	for _, p := range got {
		if p == "pkg/foo/x_test.go" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("pkg/foo/x_test.go appears %d times, want 1 (dedup broken)", count)
	}
}

// TestTestsImpactedBy_MutatedFileIsTest covers the self-skip: when the
// mutated file itself is *_test.go, it must not include itself in the
// impacted set (touching x_test.go doesn't impact x_test.go).
func TestTestsImpactedBy_MutatedFileIsTest(t *testing.T) {
	workspace, wal := fakeTestWorkspace(t)
	got := testsImpactedBy(wal, workspace, []string{"pkg/foo/x_test.go"})
	for _, p := range got {
		if p == "pkg/foo/x_test.go" {
			t.Errorf("mutated test file should not appear in its own impacted set")
		}
	}
}

// TestTestsImpactedBy_NilWAL covers a fail-soft path: if the dep-graph
// WAL is unavailable the helper should still return same-package tests
// from disk, not panic.
func TestTestsImpactedBy_NilWAL(t *testing.T) {
	workspace, _ := fakeTestWorkspace(t)
	got := testsImpactedBy(nil, workspace, []string{"pkg/foo/x.go"})
	mustContain(t, got, "pkg/foo/x_test.go")
}

// TestTestsImpactedBy_Empty covers the trivial input — no mutated files,
// no result, no panic.
func TestTestsImpactedBy_Empty(t *testing.T) {
	workspace, wal := fakeTestWorkspace(t)
	if got := testsImpactedBy(wal, workspace, nil); len(got) != 0 {
		t.Errorf("empty input should return empty result, got %v", got)
	}
}

// --- tiny helpers (avoid pulling in strings just for this file) ----------

func mustContain(t *testing.T, haystack []string, needle string) {
	t.Helper()
	for _, h := range haystack {
		if h == needle {
			return
		}
	}
	t.Errorf("expected %q in %v", needle, haystack)
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
