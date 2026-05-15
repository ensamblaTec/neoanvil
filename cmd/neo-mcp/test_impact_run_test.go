package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// TestExtractTestFuncNames_FindsTopLevelTests covers the parser's happy
// path: only top-level `func Test*(t *testing.T)` decls are returned.
// Methods, package-private helpers, and non-Test* funcs must be filtered.
func TestExtractTestFuncNames_FindsTopLevelTests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x_test.go")
	src := `package foo

import "testing"

func TestAlpha(t *testing.T)        {}
func TestBeta(t *testing.T)         {}
func TestGamma_Subcase(t *testing.T) {}
func TestingHelper(t *testing.T)    {} // NOT a test — 'Testing' prefix breaks rule
func helper(t *testing.T)           {} // unexported helper
func (s *S) TestMethod(t *testing.T) {} // method, not top-level
func BenchmarkX(b *testing.B)       {} // benchmark, -run doesn't apply
func ExampleY()                     {} // example
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	got := extractTestFuncNames(path)
	want := map[string]bool{"TestAlpha": true, "TestBeta": true, "TestGamma_Subcase": true}
	if len(got) != len(want) {
		t.Fatalf("got %d names, want %d: %v", len(got), len(want), got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected name in result: %q", n)
		}
	}
}

// TestExtractTestFuncNames_AcceptsUnderscoreAndDigit covers the post-`Test`
// character classifier: A-Z, 0-9, _ are accepted. Lowercase letter after
// `Test` (e.g. `Testing`) is rejected — matches Go's testing package rules
// so we don't sweep `Testing*` helpers into `-run`.
func TestExtractTestFuncNames_AcceptsUnderscoreAndDigit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x_test.go")
	src := `package foo

import "testing"

func Test(t *testing.T)             {} // exactly "Test" — valid edge case
func Test_lowercase(t *testing.T)   {}
func Test123(t *testing.T)          {}
func Testing(t *testing.T)          {} // rejected — lowercase follows Test
func Testify(t *testing.T)          {} // rejected — lowercase follows Test
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	got := extractTestFuncNames(path)
	want := map[string]bool{"Test": true, "Test_lowercase": true, "Test123": true}
	if len(got) != len(want) {
		t.Fatalf("got %d names, want %d: %v", len(got), len(want), got)
	}
}

// TestExtractTestFuncNames_ParseErrorReturnsNil covers fail-soft: a broken
// _test.go file must NOT panic and must NOT pollute the result. Caller
// treats nil identical to empty → no -run flag → full pkg suite runs.
func TestExtractTestFuncNames_ParseErrorReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken_test.go")
	if err := os.WriteFile(path, []byte("package foo\n\nthis is not valid Go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := extractTestFuncNames(path); got != nil {
		t.Errorf("parse error must return nil, got %v", got)
	}
}

// TestBuildTestRunRegex_EmptyInputReturnsEmpty is the DS-Finding-1 lock:
// zero impacted test files → empty string return → caller MUST fall through
// to full pkg test. If this ever regresses to `^()$`, every certify in a
// non-test-file edit silently runs ZERO tests.
func TestBuildTestRunRegex_EmptyInputReturnsEmpty(t *testing.T) {
	if got := buildTestRunRegex(nil); got != "" {
		t.Errorf("nil input must return empty, got %q", got)
	}
	if got := buildTestRunRegex([]string{}); got != "" {
		t.Errorf("empty slice must return empty, got %q", got)
	}
}

// TestBuildTestRunRegex_NoTestsInFilesReturnsEmpty covers the second
// zero-coverage trap: files exist but extractTestFuncNames returns nil
// for all of them (e.g. file with only helpers). Must still return "".
func TestBuildTestRunRegex_NoTestsInFilesReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "helpers_test.go")
	src := `package foo

import "testing"

func helper(t *testing.T) {} // unexported, not a test
func Testify(t *testing.T) {} // wrong prefix shape
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := buildTestRunRegex([]string{path}); got != "" {
		t.Errorf("files with zero Test* funcs must return empty, got %q", got)
	}
}

// TestBuildTestRunRegex_DedupAndSort covers the determinism contract:
// duplicate names across files appear once; output is sorted so the
// command line is stable across batches (matters for go test cache).
func TestBuildTestRunRegex_DedupAndSort(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a_test.go")
	pathB := filepath.Join(dir, "b_test.go")
	if err := os.WriteFile(pathA, []byte("package foo\nimport \"testing\"\nfunc TestZeta(t *testing.T) {}\nfunc TestAlpha(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("package foo\nimport \"testing\"\nfunc TestAlpha(t *testing.T) {}\nfunc TestBeta(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := buildTestRunRegex([]string{pathA, pathB})
	if got != "^(TestAlpha|TestBeta|TestZeta)$" {
		t.Errorf("expected dedup+sort, got %q", got)
	}
}

// TestImpactedSamePkgTestFiles_FiltersCrossPkg covers the v1 scope guard:
// the impacted set can contain cross-pkg tests (from dep-graph BFS), but
// v1 narrowing only applies to tests in the mutated file's own package.
// Cross-pkg test files must be dropped here so the regex feeds tests
// that actually live in the binary `go test pkgPath` builds.
func TestImpactedSamePkgTestFiles_FiltersCrossPkg(t *testing.T) {
	workspace := t.TempDir()
	// Same-pkg test that's a peer of the mutated file.
	if err := os.MkdirAll(filepath.Join(workspace, "pkg", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "pkg", "foo", "x.go"), []byte("package foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "pkg", "foo", "x_test.go"), []byte("package foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Cross-pkg consumer — its test depends on pkg/foo/x.go via dep-graph.
	if err := os.MkdirAll(filepath.Join(workspace, "pkg", "consumer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "pkg", "consumer", "c_test.go"), []byte("package consumer\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	wal, err := rag.OpenWAL(filepath.Join(workspace, "wal.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	if err := rag.InitGraphRAG(wal); err != nil {
		t.Fatalf("InitGraphRAG: %v", err)
	}
	edges := []rag.GraphEdge{
		{SourceNode: "pkg/consumer/c_test.go", TargetNode: "pkg/foo/x.go"},
	}
	if err := rag.SaveGraphEdges(wal, edges); err != nil {
		t.Fatalf("SaveGraphEdges: %v", err)
	}

	got := impactedSamePkgTestFiles(wal, workspace, filepath.Join(workspace, "pkg", "foo", "x.go"))

	// Must contain the same-pkg test (absolute path).
	wantAbs := filepath.Join(workspace, "pkg", "foo", "x_test.go")
	found := false
	for _, p := range got {
		if p == wantAbs {
			found = true
		}
		if strings.Contains(p, "consumer") {
			t.Errorf("cross-pkg test leaked into same-pkg set: %q", p)
		}
	}
	if !found {
		t.Errorf("same-pkg x_test.go missing from result: %v", got)
	}
}

// TestImpactedSamePkgTestFiles_NilWalIsSilent covers fail-soft: with no
// dep-graph backing, must return nil cleanly (caller then sees empty
// regex → no narrowing). No panic, no log noise.
func TestImpactedSamePkgTestFiles_NilWalIsSilent(t *testing.T) {
	workspace := t.TempDir()
	if got := impactedSamePkgTestFiles(nil, workspace, "anything"); got != nil {
		t.Errorf("nil wal must return nil, got %v", got)
	}
}

// TestImpactedSamePkgTestFiles_PathEscapeRejected covers the workspace
// boundary: a mutated path outside workspace must return nil rather than
// leak ../ slashes into the impacted set or panic on filepath.Rel.
func TestImpactedSamePkgTestFiles_PathEscapeRejected(t *testing.T) {
	workspace := t.TempDir()
	wal, err := rag.OpenWAL(filepath.Join(workspace, "wal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	if err := rag.InitGraphRAG(wal); err != nil {
		t.Fatal(err)
	}

	// Path entirely outside workspace.
	outsider := filepath.Join(t.TempDir(), "evil.go")
	if got := impactedSamePkgTestFiles(wal, workspace, outsider); got != nil {
		t.Errorf("path outside workspace must return nil, got %v", got)
	}
}
