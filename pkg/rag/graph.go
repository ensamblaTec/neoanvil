package rag

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.etcd.io/bbolt"
)

type GraphEdge struct {
	SourceNode string `json:"source"`
	TargetNode string `json:"target"`
	Relation   string `json:"relation"`
}

func InitGraphRAG(wal *WAL) error {
	return wal.db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("GRAPH_EDGES"))
		return err
	})
}

func GetImpactedNodes(wal *WAL, target string) ([]string, error) {
	var impacted []string
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("GRAPH_EDGES"))
		if b == nil {
			return fmt.Errorf("GRAPH_EDGES bucket not found")
		}

		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var edge GraphEdge
			if err := json.Unmarshal(v, &edge); err == nil {
				if edge.TargetNode == target {
					impacted = append(impacted, edge.SourceNode)
				}
			}
		}
		return nil
	})
	return impacted, err
}

func SaveGraphEdges(wal *WAL, edges []GraphEdge) error {
	return wal.db.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("GRAPH_EDGES"))
		if b == nil {
			return fmt.Errorf("GRAPH_EDGES bucket not found")
		}
		for _, edge := range edges {
			data, err := json.Marshal(edge)
			if err != nil {
				continue
			}

			key := []byte(edge.SourceNode + "->" + edge.TargetNode)
			if err := b.Put(key, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceFileEdges atomically replaces every outgoing edge of sourceFile in the
// GRAPH_EDGES bucket: it deletes all existing "<sourceFile>-><target>" keys,
// then writes the supplied edges. This makes per-file edge updates idempotent —
// re-indexing a file after it drops an import leaves no stale edge behind, which
// a plain SaveGraphEdges (Put-only) would. Every edge in `edges` must have
// SourceNode == sourceFile; an empty `edges` slice simply clears the file.
func ReplaceFileEdges(wal *WAL, sourceFile string, edges []GraphEdge) error {
	return wal.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("GRAPH_EDGES"))
		if b == nil {
			return fmt.Errorf("GRAPH_EDGES bucket not found")
		}
		// Collect the stale "<sourceFile>->*" keys first — deleting under a
		// live cursor is fragile in bbolt — then delete them.
		prefix := []byte(sourceFile + "->")
		var stale [][]byte
		c := b.Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			kc := make([]byte, len(k))
			copy(kc, k)
			stale = append(stale, kc)
		}
		for _, k := range stale {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		// Write the fresh edge set.
		for _, edge := range edges {
			data, err := json.Marshal(edge)
			if err != nil {
				continue
			}
			key := []byte(edge.SourceNode + "->" + edge.TargetNode)
			if err := b.Put(key, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// IndexCoverage returns the fraction of non-test .go files in workspace that have
// a corresponding node in the HNSW graph. Returns 0.0 on empty workspace or nil graph.
// O(files) — only called on BLAST_RADIUS fallback, not on hot paths.
//
// LEGACY: hardcoded to .go files. For multi-language workspaces (TypeScript
// frontends, Python, Rust, etc.) this reports 0.0 even when the HNSW is fully
// populated with the actual language's content. New callers should use
// IndexCoverageWithLang and pass the workspace's `dominant_lang` so the file
// counter matches the indexed corpus. Kept here as a thin wrapper for the 8+
// call sites still relying on the .go default while they migrate.
func IndexCoverage(g *Graph, workspace string) float64 {
	return IndexCoverageWithLang(g, workspace, "")
}

// IndexCoverageWithLang returns indexed_nodes / total_source_files_in_workspace
// where the "source file" filter is driven by `dominantLang`:
//
//	go                              → .go
//	javascript/typescript/js/ts     → .ts .tsx .js .jsx
//	python/py                       → .py
//	rust/rs                         → .rs
//	"" or unknown                   → .go (legacy fallback so IndexCoverage
//	                                   stays back-compat for old callers)
//
// All filters skip _test.go, /vendor/, and .neo/. Cap at 100% when the index
// has more nodes than discovered files (re-ingest residue, multi-pass embed).
// [SRE-LANG-AWARE-COVERAGE-2026-05-15] Detected via strategosia-frontend
// reporting RAG 0% despite a 4 GB HNSW: the workspace had zero .go files
// (Next.js frontend) so total=0 yielded 0.0 regardless of indexed content.
func IndexCoverageWithLang(g *Graph, workspace, dominantLang string) float64 {
	if g == nil || workspace == "" {
		return 0.0
	}
	indexed := len(g.Nodes)
	extensions := sourceExtensionsForLang(dominantLang)

	var total int
	_ = filepath.Walk(workspace, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(workspace, path)
		rel = filepath.ToSlash(rel)
		// Skip excluded paths regardless of extension. Check both top-level
		// (HasPrefix) AND nested (Contains "/<dir>/") so workspace layouts
		// with top-level `vendor/`, `node_modules/`, `.next/` get excluded
		// — the original IndexCoverage only handled the nested case, which
		// over-counted Go projects with conventional root-level vendor dirs.
		// [SRE-LANG-AWARE-COVERAGE-2026-05-15]
		if strings.Contains(rel, "/vendor/") || strings.HasPrefix(rel, "vendor/") ||
			strings.HasPrefix(rel, ".neo/") ||
			strings.Contains(rel, "/node_modules/") || strings.HasPrefix(rel, "node_modules/") ||
			strings.Contains(rel, "/.next/") || strings.HasPrefix(rel, ".next/") {
			return nil
		}
		// Go-specific: still skip _test.go even when "go" matches.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, ext := range extensions {
			if strings.HasSuffix(path, ext) {
				total++
				return nil
			}
		}
		return nil
	})

	if total == 0 {
		return 0.0
	}
	if indexed > total {
		indexed = total // cap at 100%
	}
	return float64(indexed) / float64(total)
}

// sourceExtensionsForLang maps the workspace `dominant_lang` to the file
// extensions that count toward `total` in IndexCoverageWithLang. Empty
// or unrecognized lang defaults to .go for back-compat with the legacy
// IndexCoverage callers (strategos / neoanvil = Go-heavy).
func sourceExtensionsForLang(lang string) []string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "go", "golang":
		return []string{".go"}
	case "javascript", "typescript", "js", "ts":
		return []string{".ts", ".tsx", ".js", ".jsx"}
	case "python", "py":
		return []string{".py"}
	case "rust", "rs":
		return []string{".rs"}
	default:
		return []string{".go"}
	}
}

// GetAllGraphEdges extrae la topología completa desde BoltDB a la RAM.
func GetAllGraphEdges(wal *WAL) (map[string][]string, error) {
	edges := make(map[string][]string)
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("GRAPH_EDGES"))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var edge GraphEdge
			if err := json.Unmarshal(v, &edge); err == nil {
				edges[edge.SourceNode] = append(edges[edge.SourceNode], edge.TargetNode)
			}
		}
		return nil
	})
	return edges, err
}
