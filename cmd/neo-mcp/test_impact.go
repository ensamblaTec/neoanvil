// cmd/neo-mcp/test_impact.go — [Phase 2 MV / Speed-First] dep-graph-aware
// test-file impact analysis. Public API (`testsImpactedBy`) returns the set
// of *_test.go files that could be affected by changes to a set of mutated
// source files. Two complementary sources:
//
//  1. **Same-package tests.** Go test files in the same directory as the
//     mutated file. The GRAPH_EDGES dep-graph does NOT link these (Go
//     same-package files don't import each other), so directory globbing
//     is the right primitive — strictly necessary for completeness.
//
//  2. **Cross-package transitive-import tests** (Phase 2 MV+ 2026-05-15).
//     Reverse-BFS over the in-memory GRAPH_EDGES adjacency: from each
//     mutated file, walk UP through "who imports me" relations until
//     reaching a _test.go terminal or hitting the depth cap. Single
//     GetAllGraphEdges scan + inverted-index build (one alloc per build,
//     re-used across all mutated files in the batch) replaces per-file
//     bucket scans.
//
//     Why transitive: mutating pkg/B/x.go must surface
//     cmd/foo/foo_test.go when cmd/foo imports pkg/A which imports
//     pkg/B. One-hop alone misses this — the most common failure mode in
//     a multi-pkg codebase.
//
// The helper is **observability-only** at this stage. Wiring it into the
// certify pipeline's `go test -run` regex is a separate epic (requires
// symbol-level mapping, not just file-level, to deliver real narrowing).
// Today it's a foundation that future commits build on.

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// testsImpactBFSDepth caps the transitive reverse-walk so a corner-case
// "everything imports everything" workspace can't fan out unbounded.
// 5 hops covers the realistic chain (test → cmd → svc-pkg → util → leaf).
const testsImpactBFSDepth = 5

// testsImpactedBy returns a sorted, deduplicated slice of workspace-relative
// _test.go paths that could be impacted by changes to any file in
// `mutatedFiles`. Both `workspace` and `mutatedFiles` use workspace-relative
// slash-paths (matching the GRAPH_EDGES key shape). `wal` may be nil — the
// cross-package dep-graph reverse-walk silently skips when the bucket is
// missing or unreachable.
//
// Bounded: O(N_edges) one-shot for the reverse-index build, plus
// O(visited) for the BFS. For typical workspaces with O(thousands) of
// edges this stays sub-millisecond per call regardless of batch size.
func testsImpactedBy(wal *rag.WAL, workspace string, mutatedFiles []string) []string {
	seen := make(map[string]struct{}, len(mutatedFiles)*4)
	collectSamePackageTests(workspace, mutatedFiles, seen)
	collectTransitiveTests(wal, mutatedFiles, seen)

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// collectSamePackageTests adds every _test.go file in each mutated file's
// directory (one ReadDir per unique dir) into `seen`. Skips the mutated
// file itself if it happens to be a test. Necessary because Go same-package
// files don't import each other → no dep-graph edge to find them via BFS.
func collectSamePackageTests(workspace string, mutatedFiles []string, seen map[string]struct{}) {
	dirsScanned := make(map[string]struct{}, len(mutatedFiles))
	for _, mf := range mutatedFiles {
		mf = filepath.ToSlash(mf)
		dir := filepath.ToSlash(filepath.Dir(mf))
		if _, done := dirsScanned[dir]; done {
			continue
		}
		dirsScanned[dir] = struct{}{}
		entries, err := os.ReadDir(filepath.Join(workspace, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(dir, name))
			if rel == mf {
				continue
			}
			seen[rel] = struct{}{}
		}
	}
}

// collectTransitiveTests does a depth-bounded reverse-BFS in the dep-graph,
// collecting any _test.go file reachable from the mutated set. Builds an
// in-memory inverted index once (target → sources) so a 20-file batch
// shares one O(N_edges) scan. _test.go files are terminals: included in
// `seen` but not further walked from (a test importing pkg/foo doesn't
// imply OTHER tests are impacted via that path).
func collectTransitiveTests(wal *rag.WAL, mutatedFiles []string, seen map[string]struct{}) {
	if wal == nil {
		return
	}
	allEdges, err := rag.GetAllGraphEdges(wal)
	if err != nil || len(allEdges) == 0 {
		return
	}
	reverse := make(map[string][]string, len(allEdges))
	for src, targets := range allEdges {
		for _, t := range targets {
			reverse[t] = append(reverse[t], src)
		}
	}
	visited := make(map[string]struct{}, len(mutatedFiles))
	queue := make([]string, 0, len(mutatedFiles))
	for _, mf := range mutatedFiles {
		s := filepath.ToSlash(mf)
		queue = append(queue, s)
		visited[s] = struct{}{}
	}
	for depth := 0; depth < testsImpactBFSDepth && len(queue) > 0; depth++ {
		next := make([]string, 0, len(queue))
		for _, current := range queue {
			for _, src := range reverse[current] {
				src = filepath.ToSlash(src)
				if _, seenN := visited[src]; seenN {
					continue
				}
				visited[src] = struct{}{}
				if strings.HasSuffix(src, "_test.go") {
					seen[src] = struct{}{}
					continue // test file is a terminal — don't walk further
				}
				next = append(next, src)
			}
		}
		queue = next
	}
}

// extractTestFuncNames parses a _test.go file and returns the names of its
// top-level `func TestXxx(*testing.T)` declarations. Used by Phase 2.2 to
// build the `-run "^(TestA|TestB|...)$"` regex passed to `go test`. Returns
// nil on parse error, non-test file, or no Test* funcs — callers MUST treat
// nil/empty the same and skip the -run narrowing entirely (DS Finding 1:
// empty regex `^()$` matches no tests and would silently zero-out coverage).
//
// Filters:
//   - Top-level only (no methods, no closures): Receiver == nil
//   - Prefix `Test` AND (next-char-uppercase OR no-next-char): excludes
//     `Testing`-style helpers BUT keeps `Test_lowercase` (Go convention).
//   - Excludes `Benchmark*`, `Example*`, `Fuzz*` — `-run` doesn't apply to
//     those subcommand types anyway.
func extractTestFuncNames(testFilePath string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, testFilePath, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var names []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name == nil {
			continue
		}
		name := fn.Name.Name
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		// Disambiguate Test vs Testing/TestHelper: per Go's testing package,
		// the rune after "Test" must be uppercase, digit, or '_' (or the name
		// is exactly "Test"). Anything else (e.g. "Testing") is NOT a test.
		if len(name) > 4 {
			r := name[4]
			isValid := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
			if !isValid {
				continue
			}
		}
		names = append(names, name)
	}
	return names
}

// buildTestRunRegex composes the `^(A|B|...)$` regex for `go test -run`
// from same-pkg impacted _test.go files. Returns "" when there are no
// names to narrow on (caller MUST fall through to running the full pkg
// suite — DS Finding 1).
//
// `samePkgTestFiles` are absolute paths to _test.go files in the same
// directory as the mutated file. Dedupe + sort the test names so the
// command is deterministic (matters for `go test` cache key stability).
func buildTestRunRegex(samePkgTestFiles []string) string {
	if len(samePkgTestFiles) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(samePkgTestFiles)*4)
	for _, p := range samePkgTestFiles {
		for _, n := range extractTestFuncNames(p) {
			seen[n] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return ""
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return "^(" + strings.Join(names, "|") + ")$"
}

// integrationTaggedTestFiles scans pkgPath for `*_test.go` files carrying a
// `//go:build integration` (or `// +build integration` legacy) build tag —
// Phase 2.4 / Speed-First always-run escape hatch. Those tests are
// unconditionally included in the -run regex even when the dep-graph
// reverse-walk doesn't reach them, on the principle that operators who
// explicitly tag a file as "integration" want it to run when its package is
// touched. Returns absolute paths matching impactedSamePkgTestFiles shape so
// the caller can union the two slices.
//
// Nil-safe: missing dir returns nil; parse errors return nil for that file.
// Scope: only the pkgPath directory (no recursion); siblings only.
func integrationTaggedTestFiles(workspace, pkgPath string) []string {
	if pkgPath == "" {
		return nil
	}
	entries, err := os.ReadDir(pkgPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		full := filepath.Join(pkgPath, name)
		body, readErr := os.ReadFile(full) //nolint:gosec // G304-WORKSPACE-CANON: caller passes validated workspace pkg dir
		if readErr != nil {
			continue
		}
		// Build tag must appear before the package declaration, per Go spec.
		// Scan only the first ~30 lines to keep this cheap.
		head := body
		if len(head) > 2048 {
			head = head[:2048]
		}
		if bytesContainsIntegrationBuildTag(head) {
			out = append(out, full)
		}
	}
	return out
}

// bytesContainsIntegrationBuildTag returns true when head contains a Go
// build constraint that activates only with the `integration` tag. Matches
// both new (`//go:build`) and legacy (`// +build`) syntax. Conservative:
// only triggers on a bare `integration` token to avoid false positives
// from `integrationdev` etc.
func bytesContainsIntegrationBuildTag(head []byte) bool {
	s := string(head)
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "//go:build") && !strings.HasPrefix(t, "// +build") {
			continue
		}
		// Split on common separators used by build constraints (space, comma,
		// ampersand, pipe, parens) and look for a bare `integration` token.
		fields := strings.FieldsFunc(t, func(r rune) bool {
			return r == ' ' || r == ',' || r == '&' || r == '|' || r == '(' || r == ')' || r == '!'
		})
		for _, f := range fields {
			if f == "integration" {
				return true
			}
		}
	}
	return false
}

// buildTestRunRegexWithAllowlist composes the `^(A|B|...)$` regex from
// the merged impacted+integration set PLUS an explicit operator allowlist
// (cfg.SRE.TestImpactAlwaysRun). Same dedup+sort discipline as
// buildTestRunRegex. Returns "" when nothing to narrow on — caller MUST
// fall through to full pkg test (DS Finding 1). [Phase 2.4]
//
// Names in `alwaysRunNames` are added verbatim (assumed Go-identifier shape
// per the operator); not validated since they're operator-supplied. The
// regex chars in test func names are still impossible (Go spec).
func buildTestRunRegexWithAllowlist(samePkgTestFiles, alwaysRunNames []string) string {
	seen := make(map[string]struct{}, len(samePkgTestFiles)*4+len(alwaysRunNames))
	for _, p := range samePkgTestFiles {
		for _, n := range extractTestFuncNames(p) {
			seen[n] = struct{}{}
		}
	}
	for _, n := range alwaysRunNames {
		if strings.TrimSpace(n) == "" {
			continue
		}
		seen[n] = struct{}{}
	}
	if len(seen) == 0 {
		return ""
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return "^(" + strings.Join(names, "|") + ")$"
}

// impactedSamePkgTestFiles returns absolute paths to _test.go files in the
// SAME directory as `mutatedAbsPath`, drawn from the workspace-relative
// impacted set produced by testsImpactedBy. Cross-pkg test files in the
// impacted set are dropped — v1 narrowing only applies within the test
// binary that `runGoBouncer` runs (i.e., the mutated file's package).
// Cross-pkg expansion is a deliberate v2 epic.
func impactedSamePkgTestFiles(wal *rag.WAL, workspace, mutatedAbsPath string) []string {
	if wal == nil || workspace == "" || mutatedAbsPath == "" {
		return nil
	}
	relMutated, err := filepath.Rel(workspace, mutatedAbsPath)
	if err != nil || strings.HasPrefix(relMutated, "..") {
		return nil
	}
	relMutated = filepath.ToSlash(relMutated)
	wantPkgDir := filepath.ToSlash(filepath.Dir(relMutated))
	impacted := testsImpactedBy(wal, workspace, []string{relMutated})
	var out []string
	for _, rel := range impacted {
		if filepath.ToSlash(filepath.Dir(rel)) == wantPkgDir {
			out = append(out, filepath.Join(workspace, rel))
		}
	}
	return out
}
