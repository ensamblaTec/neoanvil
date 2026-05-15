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
