package rag

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSourceExtensionsForLang locks the lang → extension mapping. If a new
// language gets added (or an existing alias changes) this test should fail
// loudly so the BRIEFING coverage metric doesn't silently drift on
// language-X workspaces. [SRE-LANG-AWARE-COVERAGE-2026-05-15]
func TestSourceExtensionsForLang(t *testing.T) {
	cases := []struct {
		lang string
		want []string
	}{
		{"go", []string{".go"}},
		{"golang", []string{".go"}},
		{"GO", []string{".go"}},
		{"  go  ", []string{".go"}}, // trim guard
		{"javascript", []string{".ts", ".tsx", ".js", ".jsx"}},
		{"typescript", []string{".ts", ".tsx", ".js", ".jsx"}},
		{"js", []string{".ts", ".tsx", ".js", ".jsx"}},
		{"ts", []string{".ts", ".tsx", ".js", ".jsx"}},
		{"python", []string{".py"}},
		{"py", []string{".py"}},
		{"rust", []string{".rs"}},
		{"rs", []string{".rs"}},
		{"", []string{".go"}},          // empty → legacy default
		{"klingon", []string{".go"}},   // unknown → legacy default
	}
	for _, tc := range cases {
		got := sourceExtensionsForLang(tc.lang)
		if !slicesEqual(got, tc.want) {
			t.Errorf("sourceExtensionsForLang(%q) = %v, want %v", tc.lang, got, tc.want)
		}
	}
}

// TestIndexCoverageWithLang_GoWorkspace covers the legacy Go-only path:
// IndexCoverage(...) (no lang) and IndexCoverageWithLang(..., "go") MUST
// produce the same number when both walk a Go-only workspace.
func TestIndexCoverageWithLang_GoWorkspace(t *testing.T) {
	ws := t.TempDir()
	mkFile := func(rel string) {
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a.go", "b.go", "sub/c.go", "x_test.go", "vendor/v.go", ".neo/n.go"} {
		mkFile(f)
	}

	// Only a.go, b.go, sub/c.go count: 3 source files (_test/vendor/.neo excluded).
	g := &Graph{Nodes: make([]Node, 3)} // 3 indexed nodes → 100%

	gotLegacy := IndexCoverage(g, ws)
	gotLang := IndexCoverageWithLang(g, ws, "go")
	if gotLegacy != gotLang {
		t.Errorf("Go workspace mismatch:\n  legacy: %v\n  with-lang: %v", gotLegacy, gotLang)
	}
	if gotLegacy != 1.0 {
		t.Errorf("expected 100%% coverage on 3 .go files / 3 nodes, got %v", gotLegacy)
	}
}

// TestIndexCoverageWithLang_FrontendWorkspace is the strategosia regression:
// a workspace with zero .go files but 4 .ts/.tsx files and a populated
// HNSW must NOT report 0% when dominant_lang="javascript". Legacy
// IndexCoverage stays at 0% (back-compat); the new helper produces the
// correct ratio.
func TestIndexCoverageWithLang_FrontendWorkspace(t *testing.T) {
	ws := t.TempDir()
	mkFile := func(rel string) {
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("export {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"page.tsx", "lib/util.ts", "comp/Card.tsx", "old.js", "node_modules/skip.ts", ".next/skip2.tsx"} {
		mkFile(f)
	}

	// page.tsx, lib/util.ts, comp/Card.tsx, old.js = 4 source files
	// (node_modules/.next excluded).
	g := &Graph{Nodes: make([]Node, 4)}

	gotLegacy := IndexCoverage(g, ws)
	if gotLegacy != 0.0 {
		t.Errorf("legacy IndexCoverage on zero-.go workspace must stay 0.0 for back-compat, got %v", gotLegacy)
	}

	gotLang := IndexCoverageWithLang(g, ws, "typescript")
	if gotLang != 1.0 {
		t.Errorf("typescript workspace with 4 indexed/4 source must be 100%%, got %v", gotLang)
	}
}

// TestIndexCoverageWithLang_PartialCoverage covers the cap-at-100% guard
// and the typical "indexed < total" case.
func TestIndexCoverageWithLang_PartialCoverage(t *testing.T) {
	ws := t.TempDir()
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		if err := os.WriteFile(filepath.Join(ws, f), []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 5 files, 2 indexed → 40%.
	g := &Graph{Nodes: make([]Node, 2)}
	got := IndexCoverageWithLang(g, ws, "go")
	if got != 0.4 {
		t.Errorf("2/5 = 40%%, got %v", got)
	}

	// 10 indexed > 5 total → cap at 100%.
	gCap := &Graph{Nodes: make([]Node, 10)}
	gotCap := IndexCoverageWithLang(gCap, ws, "go")
	if gotCap != 1.0 {
		t.Errorf("cap-at-100%% violated: got %v for 10 indexed / 5 total", gotCap)
	}
}

// TestIndexCoverageWithLang_NilGuards covers the defensive paths: nil
// graph or empty workspace must return 0.0 cleanly, no panic, no walk
// (perf budget — this is on the BRIEFING hot path indirectly).
func TestIndexCoverageWithLang_NilGuards(t *testing.T) {
	if got := IndexCoverageWithLang(nil, "/anywhere", "go"); got != 0.0 {
		t.Errorf("nil graph must return 0.0, got %v", got)
	}
	if got := IndexCoverageWithLang(&Graph{}, "", "go"); got != 0.0 {
		t.Errorf("empty workspace must return 0.0, got %v", got)
	}
}

// slicesEqual is a tiny no-import helper for the lang-mapping test.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
