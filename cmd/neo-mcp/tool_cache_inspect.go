// cmd/neo-mcp/tool_cache_inspect.go — per-target debug view across all
// three cache layers. [PILAR-XXV/218]
//
// Motivation: operator sees a SEMANTIC_CODE or BLAST_RADIUS call NOT
// hitting cache when they expected it to. Is the key misspelled?
// Generation stale? Never cached? Fell out of LRU? neo_cache_stats
// returns aggregate counters; this tool pinpoints the single (target)
// query's state across all three layers.

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

type CacheInspectTool struct {
	queryCache *rag.QueryCache
	textCache  *rag.TextCache
	embCache   *rag.Cache[[]float32]
	graph      *rag.Graph
}

func (t *CacheInspectTool) Name() string { return "neo_cache_inspect" }

func (t *CacheInspectTool) Description() string {
	return "SRE Tool: Inspects a single target across all three cache layers. Reports presence, hit count, variant, and (for the text cache) which handlers hold an entry on this target. Use when a query is NOT hitting cache and you want to pinpoint why."
}

func (t *CacheInspectTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "The natural-language query (for SEMANTIC_CODE) or file path (for BLAST_RADIUS) to look up.",
			},
			"top_k": map[string]any{
				"type":        "integer",
				"description": "Optional variant discriminator for SEMANTIC_CODE / QueryCache lookup (default 5 — matches handleSemanticCode).",
			},
		},
		Required: []string{"target"},
	}
}

func (t *CacheInspectTool) Execute(_ context.Context, args map[string]any) (any, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("cache_inspect: target is required")
	}
	topK := 5
	if v, ok := args["top_k"].(float64); ok && v > 0 {
		topK = int(v)
	}
	currentGen := uint64(0)
	if t.graph != nil {
		currentGen = t.graph.Gen.Load()
	}

	out := map[string]any{
		"target":      target,
		"top_k":       topK,
		"current_gen": currentGen,
	}

	// [Épica 227] All inspection reads use Peek so the tool doesn't
	// distort the hit/miss counters it's meant to observe.
	if t.queryCache != nil {
		qKey := rag.NewQueryCacheKey("SEMANTIC_CODE|"+target, topK)
		if cached, ok := t.queryCache.Peek(qKey, currentGen); ok {
			out["query_cache"] = map[string]any{
				"present":     true,
				"result_size": len(cached),
			}
		} else {
			out["query_cache"] = map[string]any{"present": false}
		}
	}

	if t.embCache != nil {
		eKey := rag.NewCacheKey("EMBED|"+target, 0)
		if vec, ok := t.embCache.Peek(eKey, currentGen); ok {
			out["embedding_cache"] = map[string]any{
				"present": true,
				"vec_dim": len(vec),
			}
		} else {
			out["embedding_cache"] = map[string]any{"present": false}
		}
	}

	if t.textCache != nil {
		variants := []struct {
			Handler string
			Variant int
		}{
			{"BLAST_RADIUS", 0},
			{"PROJECT_DIGEST", 10},
			{"GRAPH_WALK", 2},
		}
		textHits := []map[string]any{}
		for _, v := range variants {
			key := rag.NewTextCacheKey(v.Handler, target, v.Variant)
			if _, ok := t.textCache.Peek(key, currentGen); ok {
				textHits = append(textHits, map[string]any{
					"handler": v.Handler,
					"variant": v.Variant,
				})
			}
		}
		out["text_cache"] = map[string]any{
			"probed":    []string{"BLAST_RADIUS", "PROJECT_DIGEST", "GRAPH_WALK"},
			"hits":      textHits,
			"hit_count": len(textHits),
		}
	}

	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("cache_inspect: %w", err)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(buf)}},
	}, nil
}
