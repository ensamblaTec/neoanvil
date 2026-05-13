// cmd/neo-mcp/tool_cache_persist.go — operator-invoked disk snapshot
// of the QueryCache hot set. [PILAR-XXV/197]
//
// Usage pattern:
//   1. Work a session until QueryCache has populated naturally
//   2. Call neo_cache_persist BEFORE make rebuild-restart
//   3. On next boot, the hot entries are auto-loaded — zero cold-start
//
// Default top-N = 32. File lives at workspace/.neo/db/query_cache.snapshot.json.

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

// cacheSnapshotRelPath is the default location within the workspace where
// the QueryCache hot-entry snapshot lives. Centralised constant so the
// tool and main.go's boot loader agree.
const cacheSnapshotRelPath = ".neo/db/query_cache.snapshot.json"

// textCacheSnapshotRelPath is the companion path for TextCache. [Épica 200]
const textCacheSnapshotRelPath = ".neo/db/text_cache.snapshot.json"

// embCacheSnapshotRelPath is the companion path for EmbeddingCache. [Épica 210]
const embCacheSnapshotRelPath = ".neo/db/embedding_cache.snapshot.json"

// hotFilesCacheSnapshotRelPath holds path+mtime+size of recently-touched
// files. Persisted on shutdown so the cache warms on next boot. Content
// itself is NOT persisted — Load re-reads files only when mtime+size
// still match (so no stale content is served post-restart). [LARGE-PROJECT/A]
const hotFilesCacheSnapshotRelPath = ".neo/db/hotfile_cache.snapshot.json"

type CachePersistTool struct {
	queryCache *rag.QueryCache
	textCache  *rag.TextCache        // [200]
	embCache   *rag.Cache[[]float32] // [210]
	workspace  string
}

func (t *CachePersistTool) Name() string { return "neo_cache_persist" }

func (t *CachePersistTool) Description() string {
	return "SRE Tool: Serialises the top-N entries of all three cache layers to disk under workspace/.neo/db/. Next make rebuild-restart auto-loads them — the session starts warm instead of cold. Defaults: query=32, text=16, embedding=64. Scope selects which caches to persist."
}

func (t *CachePersistTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"scope": map[string]any{
				"type":        "string",
				"description": "Which caches to persist: 'query' | 'text' | 'embedding' | 'all' (default).",
				"enum":        []string{"query", "text", "embedding", "all", "both"},
			},
			"query_top_n": map[string]any{
				"type":        "integer",
				"description": "Top-N for QueryCache. Default 32, cap 256.",
			},
			"text_top_n": map[string]any{
				"type":        "integer",
				"description": "Top-N for TextCache. Default 16 (bodies are larger), cap 64.",
			},
			"embedding_top_n": map[string]any{
				"type":        "integer",
				"description": "Top-N for EmbeddingCache. Default 64 (vectors are dense float32 arrays), cap 128.",
			},
		},
		Required: []string{},
	}
}

// boundedIntArg extracts an int arg bounded by [default, cap]. Shared by
// all three top-N fields so capping stays consistent. [Épica 228]
func boundedIntArg(args map[string]any, key string, def, cap int) int {
	n := def
	if v, ok := args[key].(float64); ok && v > 0 {
		n = int(v)
	}
	if n > cap {
		n = cap
	}
	return n
}

// parsePersistArgs normalises scope + the three top-N overrides. "both"
// is a historical alias for "all". [Épica 228]
func parsePersistArgs(args map[string]any) (scope string, queryN, textN, embN int) {
	scope, _ = args["scope"].(string)
	if scope == "" || scope == "both" {
		scope = "all"
	}
	queryN = boundedIntArg(args, "query_top_n", 32, 256)
	textN = boundedIntArg(args, "text_top_n", 16, 64)
	embN = boundedIntArg(args, "embedding_top_n", 64, 128)
	return
}

// persistQuery writes the QueryCache snapshot. Returns a one-line
// human-readable summary on success. [Épica 228]
func (t *CachePersistTool) persistQuery(n int) (string, error) {
	if t.queryCache == nil {
		return "", fmt.Errorf("cache_persist: query cache not wired")
	}
	path := filepath.Join(t.workspace, cacheSnapshotRelPath)
	if err := t.queryCache.SaveSnapshot(path, n); err != nil {
		return "", fmt.Errorf("cache_persist query: %w", err)
	}
	return fmt.Sprintf("QueryCache: up to %d entries → %s", n, path), nil
}

// persistText writes the TextCache snapshot. [Épica 228]
func (t *CachePersistTool) persistText(n int) (string, error) {
	if t.textCache == nil {
		return "", fmt.Errorf("cache_persist: text cache not wired")
	}
	path := filepath.Join(t.workspace, textCacheSnapshotRelPath)
	if err := t.textCache.SaveSnapshot(path, n); err != nil {
		return "", fmt.Errorf("cache_persist text: %w", err)
	}
	return fmt.Sprintf("TextCache: up to %d entries → %s", n, path), nil
}

// persistEmbedding writes the EmbeddingCache snapshot. [Épica 228]
func (t *CachePersistTool) persistEmbedding(n int) (string, error) {
	if t.embCache == nil {
		return "", fmt.Errorf("cache_persist: embedding cache not wired")
	}
	path := filepath.Join(t.workspace, embCacheSnapshotRelPath)
	if err := t.embCache.SaveSnapshotJSON(path, n); err != nil {
		return "", fmt.Errorf("cache_persist embedding: %w", err)
	}
	return fmt.Sprintf("EmbeddingCache: up to %d entries → %s", n, path), nil
}

// formatPersistReport renders the bullet-list success message. [Épica 228]
func formatPersistReport(msgs []string) map[string]any {
	var sb strings.Builder
	sb.WriteString("✅ Cache snapshot written:")
	for _, m := range msgs {
		sb.WriteString("\n  - ")
		sb.WriteString(m)
	}
	sb.WriteString("\n(next boot loads them automatically)")
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": sb.String()}},
	}
}

func (t *CachePersistTool) Execute(_ context.Context, args map[string]any) (any, error) {
	scope, queryN, textN, embN := parsePersistArgs(args)

	var msgs []string
	if scope == "query" || scope == "all" {
		msg, err := t.persistQuery(queryN)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	if scope == "text" || scope == "all" {
		msg, err := t.persistText(textN)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	if scope == "embedding" || scope == "all" {
		msg, err := t.persistEmbedding(embN)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("cache_persist: unknown scope %q", scope)
	}
	return formatPersistReport(msgs), nil
}
