// cmd/neo-mcp/tool_cache_flush.go — explicit invalidation of both RAG
// cache layers by bumping Graph.Gen. [PILAR-XXV/187]
//
// Why a dedicated flush tool when bypass_cache exists:
//   - bypass_cache (183) is per-call: it refreshes ONE entry and leaves
//     every other stale entry alive.
//   - neo_cache_flush is session-wide: it invalidates EVERY cached entry
//     at once by bumping the generation counter. Next lookup in any
//     cache evicts lazily on mismatch.
//   - Use case: after a large manual edit or external indexing job
//     finishes, one flush clears the decks without 256+ bypass_cache
//     calls.
//
// Implementation: a single atomic increment on Graph.Gen. O(1), no
// locks, no iteration over cache entries.

package main

import (
	"context"
	"fmt"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

type CacheFlushTool struct {
	graph *rag.Graph
}

func (t *CacheFlushTool) Name() string { return "neo_cache_flush" }

func (t *CacheFlushTool) Description() string {
	return "SRE Tool: Invalidates every entry in both RAG caches (QueryCache + TextCache) by bumping the Graph generation counter. Next lookup for any key evicts lazily. Zero cost O(1), no iteration. Use after a manual edit + no-certify or an external ingest job that the caches were not aware of."
}

func (t *CacheFlushTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type:       "object",
		Properties: map[string]any{},
		Required:   []string{},
	}
}

func (t *CacheFlushTool) Execute(_ context.Context, _ map[string]any) (any, error) {
	if t.graph == nil {
		return nil, fmt.Errorf("cache_flush: graph not wired")
	}
	newGen := t.graph.Gen.Add(1)
	msg := fmt.Sprintf("✅ Cache generation bumped to %d — every cached entry will miss on next lookup and evict lazily.", newGen)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
	}, nil
}
