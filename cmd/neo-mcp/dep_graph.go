// dep_graph.go — file→file dependency edges for the RAG dep-graph
// (the GRAPH_EDGES bucket). BLAST_RADIUS reads this graph to compute impact +
// PageRank; before this wiring the writer (rag.SaveGraphEdges) had zero callers
// and the graph was always empty, so every BLAST_RADIUS silently fell back to
// the AST scope with confidence:medium. The resolvers here turn a source file's
// raw import list into the set of OTHER workspace files it depends on, keyed by
// workspace-relative path so they match how BLAST_RADIUS queries.
// [BLAST_RADIUS dep-graph fix 2026-05-14]
package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// workspaceModulePath reads the `module` path from the workspace's go.mod.
// Returns "" when there is no go.mod (non-Go workspace) — callers then skip Go
// import resolution and only relative TS/JS/Python imports resolve.
func workspaceModulePath(workspace string) string {
	data, err := os.ReadFile(filepath.Join(workspace, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// fileDepEdges resolves the imports of srcRel (a workspace-relative source
// path) to the file→file GraphEdges it produces — one edge per OTHER workspace
// file srcRel depends on. Go import paths under modulePath map to every
// non-test source file in the package directory; TS/JS/Python relative imports
// resolve against srcRel's directory. External/stdlib/unresolvable imports and
// self-edges are dropped. Every returned edge has SourceNode == srcRel.
func fileDepEdges(workspace, modulePath, srcRel string, imports []string) []rag.GraphEdge {
	ext := filepath.Ext(srcRel)
	var targets []string
	for _, imp := range imports {
		switch ext {
		case ".go":
			targets = append(targets, goImportToFiles(workspace, modulePath, imp)...)
		case ".ts", ".tsx", ".js", ".jsx", ".py":
			if f := relativeImportToFile(workspace, srcRel, imp, ext); f != "" {
				targets = append(targets, f)
			}
		}
	}
	seen := make(map[string]struct{}, len(targets))
	edges := make([]rag.GraphEdge, 0, len(targets))
	for _, tgt := range targets {
		if tgt == "" || tgt == srcRel {
			continue // skip self-edges and unresolved
		}
		if _, dup := seen[tgt]; dup {
			continue
		}
		seen[tgt] = struct{}{}
		edges = append(edges, rag.GraphEdge{SourceNode: srcRel, TargetNode: tgt, Relation: "imports"})
	}
	return edges
}

// goImportToFiles maps a Go import path to the workspace-relative non-test .go
// files of that package's directory. Returns nil for stdlib / third-party
// imports (not under modulePath), the module root itself, and directories with
// no Go files. Build-tagged files are included — an over-approximation that is
// the safe direction for a blast radius.
func goImportToFiles(workspace, modulePath, importPath string) []string {
	if modulePath == "" || importPath == modulePath {
		return nil
	}
	rel, ok := strings.CutPrefix(importPath, modulePath+"/")
	if !ok {
		return nil // not under the workspace module — stdlib or third-party
	}
	dir := filepath.Join(workspace, filepath.FromSlash(rel))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.ToSlash(filepath.Join(rel, name)))
	}
	return files
}

// backfillDepGraph populates the GRAPH_EDGES bucket from a cheap import-only
// walk of the workspace — no embeddings, just file read + regex + path stat.
// It is the snapshot-boot safety net: bootstrapWorkspace only writes edges for
// files it (re)embeds, and on a fast-boot from the hnsw.bin snapshot it embeds
// nothing, so without this BLAST_RADIUS would stay on graph_status:empty
// forever. No-ops when GRAPH_EDGES already has edges (a prior cold boot or the
// per-certify refresh populated it) — so the only redundant run is the very
// first cold boot, where bootstrapWorkspace populates concurrently with
// identical content. Meant to run in its own goroutine; never blocks boot.
func backfillDepGraph(workspace string, wal *rag.WAL, graph *rag.Graph, cfg *config.NeoConfig) {
	if existing, err := rag.GetAllGraphEdges(wal); err == nil && len(existing) > 0 {
		return // dep-graph already populated — nothing to backfill
	}
	modulePath := workspaceModulePath(workspace)
	var indexed int
	walkErr := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			for _, ignore := range cfg.Workspace.IgnoreDirs {
				if name == ignore || name == ".neo" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		ext := filepath.Ext(path)
		if !isSupportedExt(ext, cfg.Workspace.AllowedExtensions) {
			return nil
		}
		data, readErr := os.ReadFile(path) //nolint:gosec // G304-WORKSPACE-CANON
		if readErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		edges := fileDepEdges(workspace, modulePath, relSlash, extractImports(string(data), ext))
		if len(edges) == 0 {
			return nil
		}
		if err := rag.ReplaceFileEdges(wal, relSlash, edges); err != nil {
			log.Printf("[SRE-WARN] dep-graph backfill %s: %v", relSlash, err)
			return nil
		}
		indexed++
		return nil
	})
	if walkErr != nil {
		log.Printf("[SRE-WARN] dep-graph backfill walk: %v", walkErr)
	}
	if indexed > 0 {
		// Bump the graph generation so any radar-cache entry computed during
		// the backfill window (GRAPH_EDGES still empty/partial) is invalidated.
		// The TextCache/QueryCache gen-guard tracks graph.Gen; a dep-graph write
		// otherwise leaves it untouched, so a BLAST_RADIUS that lands mid-boot
		// would cache a stale `empty` result for the whole session. One-time at
		// boot — the radar caches are cold then, so over-invalidation is free.
		graph.Gen.Add(1)
		log.Printf("[BLAST_RADIUS] dep-graph backfill: %d files → GRAPH_EDGES (cache gen bumped)", indexed)
	}
}

// relativeImportToFile resolves a TS/JS/Python relative import against the
// importing file's directory, trying the common file-extension forms. Returns
// "" when the import is non-relative (a bare package import) or cannot be
// resolved to a file inside the workspace.
func relativeImportToFile(workspace, srcRel, imp, ext string) string {
	if !strings.HasPrefix(imp, ".") {
		return "" // bare package import — not a workspace file
	}
	base := filepath.Join(filepath.Dir(srcRel), filepath.FromSlash(imp))
	var candidates []string
	switch ext {
	case ".py":
		candidates = []string{base + ".py", filepath.Join(base, "__init__.py")}
	default: // ts/tsx/js/jsx family
		candidates = []string{
			base + ".ts", base + ".tsx", base + ".js", base + ".jsx",
			filepath.Join(base, "index.ts"), filepath.Join(base, "index.js"),
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(workspace, c)); err == nil && !st.IsDir() {
			return filepath.ToSlash(c)
		}
	}
	return ""
}
