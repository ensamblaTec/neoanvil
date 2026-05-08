// cmd/neo-mcp/tool_cache.go — unified cache observability + control tool.
// [Épica 239]
//
// Consolidates six previously-independent MCP tools into a single entry
// point with an `action` enum. Reduces the agent-facing surface from 7
// cache-related tools to 2 (this one + neo_tool_stats, which is about
// tool latency, not caches).
//
// Actions:
//   stats    → JSON statistics across all layers (was neo_cache_stats)
//   flush    → bump Graph.Gen, invalidate every entry (was neo_cache_flush)
//   resize   → runtime capacity tuning (was neo_cache_resize)
//   warmup   → parallel pre-population (was neo_cache_warmup)
//   persist  → snapshot all layers to disk (was neo_cache_persist)
//   inspect  → per-target debug across layers (was neo_cache_inspect)
//
// Implementation: delegates to the existing *Tool types' Execute methods
// (kept intact, still individually testable) and forwards args verbatim.
// This preserves every piece of functionality and all edge-case
// coverage; the only schema change is the new `action` field at the top.

package main

import (
	"context"
	"fmt"
)

// CacheTool is the umbrella dispatcher. It holds the six sub-tool
// instances; routing is O(1) via a switch on args["action"].
type CacheTool struct {
	stats   *CacheStatsTool
	flush   *CacheFlushTool
	resize  *CacheResizeTool
	warmup  *CacheWarmupTool
	persist *CachePersistTool
	inspect *CacheInspectTool
}

func (t *CacheTool) Name() string { return "neo_cache" }

func (t *CacheTool) Description() string {
	return "SRE Tool: Unified cache observability + control for the three RAG cache layers (QueryCache / TextCache / EmbeddingCache). Dispatches via `action` to stats (live JSON), flush (invalidate-all via Gen bump, O(1)), resize (runtime capacity retune), warmup (parallel pre-populate from explicit list or recent misses), persist (disk snapshot), or inspect (per-target cross-layer debug). One entry point replaces 6 previously-separate tools."
}

func (t *CacheTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Which cache operation to perform. See per-action extra fields below.",
				"enum":        []string{"stats", "flush", "resize", "warmup", "persist", "inspect"},
			},
			// stats
			"include": map[string]any{
				"type":        "array",
				"description": "[stats] Optional filter. Subset of ['query_cache','text_cache','embedding_cache','search_paths','tool_latency']. Empty/absent = all.",
				"items":       map[string]any{"type": "string"},
			},
			"top_n": map[string]any{
				"type":        "integer",
				"description": "[stats] Top-N entries per cache (default 5, cap 50).",
			},
			// resize
			"scope": map[string]any{
				"type":        "string",
				"description": "[resize|persist] Which layer(s). For resize: query|text|both. For persist: query|text|embedding|all.",
				"enum":        []string{"query", "text", "both", "embedding", "all"},
			},
			"capacity": map[string]any{
				"type":        "integer",
				"description": "[resize] New capacity. 0 disables the selected layer.",
			},
			// warmup
			"targets": map[string]any{
				"type":        "array",
				"description": "[warmup] Targets to warm. For SEMANTIC_CODE each is a natural-language query; for BLAST_RADIUS each is a file path. Mixed lists fire both handlers when handlers is omitted.",
				"items":       map[string]any{"type": "string"},
			},
			"handlers": map[string]any{
				"type":        "array",
				"description": "[warmup] Optional. Which handlers to warm. Subset of ['SEMANTIC_CODE','BLAST_RADIUS']. Default: both.",
				"items":       map[string]any{"type": "string", "enum": []string{"SEMANTIC_CODE", "BLAST_RADIUS"}},
			},
			"from_recent": map[string]any{
				"type":        "boolean",
				"description": "[warmup] Auto-source targets from RecentMissTargets rings (up to 20). When true, 'targets' is ignored.",
			},
			// persist
			"query_top_n": map[string]any{
				"type":        "integer",
				"description": "[persist] Top-N for QueryCache. Default 32, cap 256.",
			},
			"text_top_n": map[string]any{
				"type":        "integer",
				"description": "[persist] Top-N for TextCache. Default 16 (bodies are larger), cap 64.",
			},
			"embedding_top_n": map[string]any{
				"type":        "integer",
				"description": "[persist] Top-N for EmbeddingCache. Default 64, cap 128.",
			},
			// inspect
			"target": map[string]any{
				"type":        "string",
				"description": "[inspect] Natural-language query (for SEMANTIC_CODE) or file path (for BLAST_RADIUS) to probe.",
			},
			"top_k": map[string]any{
				"type":        "integer",
				"description": "[inspect] QueryCache variant discriminator (default 5).",
			},
		},
		Required: []string{"action"},
	}
}

// Execute routes by `action` to the legacy sub-tool's Execute. Behaviour
// and output shape are identical to calling the old MCP tool directly —
// the only change is the entry point.
func (t *CacheTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	action, _ := args["action"].(string)
	switch action {
	case "stats":
		return t.stats.Execute(ctx, args)
	case "flush":
		return t.flush.Execute(ctx, args)
	case "resize":
		return t.resize.Execute(ctx, args)
	case "warmup":
		return t.warmup.Execute(ctx, args)
	case "persist":
		return t.persist.Execute(ctx, args)
	case "inspect":
		return t.inspect.Execute(ctx, args)
	case "":
		return nil, fmt.Errorf("neo_cache: action is required (one of: stats, flush, resize, warmup, persist, inspect)")
	default:
		return nil, fmt.Errorf("neo_cache: unknown action %q — valid: stats, flush, resize, warmup, persist, inspect", action)
	}
}
