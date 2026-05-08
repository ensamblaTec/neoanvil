// cmd/neo-mcp/tool_cache_resize.go — runtime capacity adjustment for both
// RAG cache layers. [PILAR-XXV/191]
//
// Operators tuning cache behaviour during a session can:
//   - observe hit ratio + eviction rate via neo_cache_stats (184)
//   - flush everything via neo_cache_flush (187)
//   - now ALSO resize at runtime without a restart
//
// Config `query_cache_capacity` still controls the boot default; this
// tool overrides that value for the remaining process lifetime. The
// change survives until next make rebuild-restart, so experiments are
// reversible by default.

package main

import (
	"context"
	"fmt"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

type CacheResizeTool struct {
	queryCache *rag.QueryCache
	textCache  *rag.TextCache
}

func (t *CacheResizeTool) Name() string { return "neo_cache_resize" }

func (t *CacheResizeTool) Description() string {
	return "SRE Tool: Resizes one or both RAG cache layers at runtime. Accepts scope ('query' | 'text' | 'both') and capacity (int ≥ 0). Growing is O(1); shrinking evicts LRU tail until size fits. Capacity=0 disables the scoped cache. Override survives until next restart."
}

func (t *CacheResizeTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"scope": map[string]any{
				"type":        "string",
				"description": "Which cache to resize: 'query' (QueryCache — SEMANTIC_CODE IDs), 'text' (TextCache — BLAST_RADIUS/PROJECT_DIGEST/GRAPH_WALK), or 'both'.",
				"enum":        []string{"query", "text", "both"},
			},
			"capacity": map[string]any{
				"type":        "integer",
				"description": "New LRU capacity in number of entries. 0 disables the scoped cache. Typical values: 256 (default), 1024 (heavy session), 0 (benchmarking).",
			},
		},
		Required: []string{"scope", "capacity"},
	}
}

func (t *CacheResizeTool) Execute(_ context.Context, args map[string]any) (any, error) {
	scope, _ := args["scope"].(string)
	capF, _ := args["capacity"].(float64)
	capacity := int(capF)
	if capacity < 0 {
		return nil, fmt.Errorf("cache_resize: capacity must be ≥ 0, got %d", capacity)
	}

	var changes []string
	switch scope {
	case "query":
		if t.queryCache == nil {
			return nil, fmt.Errorf("cache_resize: query cache not wired")
		}
		old := t.queryCache.Capacity()
		t.queryCache.Resize(capacity)
		changes = append(changes, fmt.Sprintf("QueryCache: %d → %d", old, capacity))
	case "text":
		if t.textCache == nil {
			return nil, fmt.Errorf("cache_resize: text cache not wired")
		}
		old := t.textCache.Capacity()
		t.textCache.Resize(capacity)
		changes = append(changes, fmt.Sprintf("TextCache: %d → %d", old, capacity))
	case "both":
		if t.queryCache != nil {
			old := t.queryCache.Capacity()
			t.queryCache.Resize(capacity)
			changes = append(changes, fmt.Sprintf("QueryCache: %d → %d", old, capacity))
		}
		if t.textCache != nil {
			old := t.textCache.Capacity()
			t.textCache.Resize(capacity)
			changes = append(changes, fmt.Sprintf("TextCache: %d → %d", old, capacity))
		}
	default:
		return nil, fmt.Errorf("cache_resize: unknown scope %q (want 'query' | 'text' | 'both')", scope)
	}

	msg := "✅ Cache resized:\n"
	for _, c := range changes {
		msg += "  - " + c + "\n"
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
	}, nil
}
