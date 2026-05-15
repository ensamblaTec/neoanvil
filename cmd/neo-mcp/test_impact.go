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
//  2. **Cross-package direct-import tests.** Reverse-walks GRAPH_EDGES one
//     hop from the mutated file: any source file with an edge pointing at
//     the mutated file gets included if it's a _test.go file.
//
// One-hop instead of transitive BFS keeps the helper bounded (~ms per
// mutated file in workspaces with thousands of edges) and predictable.
// Transitive analysis is a deliberate non-goal for the MV — it would
// require visited-set bookkeeping AND would surface tests that are only
// indirectly impacted, which CI's full suite catches anyway.
//
// The helper is **observability-only** at this stage. Wiring it into the
// certify pipeline's `go test -run` regex is a separate epic (requires
// symbol-level mapping, not just file-level, to deliver real narrowing).
// Today it's a foundation that future commits build on.

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// testsImpactedBy returns a sorted, deduplicated slice of workspace-relative
// _test.go paths that could be impacted by changes to any file in
// `mutatedFiles`. Both `workspace` and `mutatedFiles` use workspace-relative
// slash-paths (matching the GRAPH_EDGES key shape). `wal` may be nil — the
// cross-package dep-graph reverse-walk silently skips when the bucket is
// missing or unreachable.
//
// Bounded: same-package tests via one ReadDir per unique mutated-file
// directory; cross-package via one full GRAPH_EDGES scan per mutated file
// (GetImpactedNodes already does this). For typical workspaces with
// O(thousands) of edges this stays sub-millisecond per call.
func testsImpactedBy(wal *rag.WAL, workspace string, mutatedFiles []string) []string {
	seen := make(map[string]struct{}, len(mutatedFiles)*4)

	// Cache directory listings across mutated files in the same package
	// so a 20-file batch in one package doesn't re-ReadDir 20 times.
	dirsScanned := make(map[string]struct{}, len(mutatedFiles))

	for _, mf := range mutatedFiles {
		mf = filepath.ToSlash(mf)
		dir := filepath.ToSlash(filepath.Dir(mf))

		// (1) Same-package tests.
		if _, done := dirsScanned[dir]; !done {
			dirsScanned[dir] = struct{}{}
			absDir := filepath.Join(workspace, dir)
			if entries, err := os.ReadDir(absDir); err == nil {
				for _, e := range entries {
					name := e.Name()
					if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
						continue
					}
					rel := filepath.ToSlash(filepath.Join(dir, name))
					if rel == mf {
						continue // skip the mutated file itself if it IS a test
					}
					seen[rel] = struct{}{}
				}
			}
		}

		// (2) Cross-package direct-import tests (one-hop reverse-walk).
		if wal == nil {
			continue
		}
		impacted, err := rag.GetImpactedNodes(wal, mf)
		if err != nil {
			continue
		}
		for _, src := range impacted {
			src = filepath.ToSlash(src)
			if strings.HasSuffix(src, "_test.go") {
				seen[src] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
