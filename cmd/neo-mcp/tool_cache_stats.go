// cmd/neo-mcp/tool_cache_stats.go — dedicated MCP tool that exposes the
// full state of both cache layers, the alternative search path counters,
// and the top MCP tool-call latency percentiles. [PILAR-XXV/184]
//
// Why a dedicated tool instead of another BRIEFING section:
//   - BRIEFING output competes for token budget with the master plan and
//     session state. Operators who only want to verify cache behaviour
//     should not have to re-request BRIEFING.
//   - JSON output (as structured MCP content) is machine-readable and
//     can feed external dashboards without line-parsing the markdown
//     BRIEFING returns.
//   - Zero side-effects: pure read. Safe to call repeatedly in tight
//     loops during cache tuning.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/state"
)

// cacheSnapshotInfo inspects the 3 on-disk snapshot paths under
// workspace/.neo/db/ and reports whether each exists + its size + mtime.
// Returned shape is stable JSON so dashboards can render it directly.
// [Épica 217]
func cacheSnapshotInfo(workspace string) map[string]any {
	paths := map[string]string{
		"query_cache":     filepath.Join(workspace, cacheSnapshotRelPath),
		"text_cache":      filepath.Join(workspace, textCacheSnapshotRelPath),
		"embedding_cache": filepath.Join(workspace, embCacheSnapshotRelPath),
	}
	out := make(map[string]any, len(paths))
	for key, path := range paths {
		entry := map[string]any{"path": path, "exists": false}
		if fi, err := os.Stat(path); err == nil {
			entry["exists"] = true
			entry["size_bytes"] = fi.Size()
			entry["mtime"] = fi.ModTime().UTC().Format(time.RFC3339)
		}
		out[key] = entry
	}
	return out
}

// window5m is the canonical short-window size surfaced in cache_stats —
// long enough to smooth out boot spikes, short enough to reflect the
// current workload. [Épica 195]
const window5m = 5 * time.Minute

// CacheStatsTool inspects both Rag cache layers plus the per-tool
// latency histogram. Held as pointers so the tool sees live state —
// not a snapshot taken at registration time.
type CacheStatsTool struct {
	queryCache     *rag.QueryCache
	textCache      *rag.TextCache
	embCache       *rag.Cache[[]float32] // [199]
	hotFiles       *rag.HotFilesCache    // [LARGE-PROJECT/A 2026-05-13] per-workspace file content cache
	workspace      string                // [217] used to render snapshot paths
	knowledgeStats func() (hot, total int) // [295.E] nil when KnowledgeStore not available
}

func (t *CacheStatsTool) Name() string { return "neo_cache_stats" }

func (t *CacheStatsTool) Description() string {
	return "SRE Tool: Returns live statistics for both RAG cache layers (QueryCache for SEMANTIC_CODE IDs, TextCache for BLAST_RADIUS / PROJECT_DIGEST / GRAPH_WALK markdown), the binary/hybrid/int8 HNSW search path counters, and the top MCP tool latency percentiles. Pure read, zero side effects — safe to poll during cache tuning."
}

func (t *CacheStatsTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"include": map[string]any{
				"type":        "array",
				"description": "[Épica 209 + LARGE-PROJECT 2026-05-13] Optional filter. Subset of ['query_cache', 'text_cache', 'embedding_cache', 'hot_files', 'planner_cache', 'search_paths', 'tool_latency', 'knowledge_store', 'pagerank_cache']. Empty/absent = all. Use to trim the output when focused on one layer.",
				"items":       map[string]any{"type": "string"},
			},
			"top_n": map[string]any{
				"type":        "integer",
				"description": "[Épica 216] How many top-hit entries to return per cache (default 5, cap 50). Only affects top_5/top_N output field; lifetime stats unchanged.",
			},
		},
		Required: []string{},
	}
}

// parseStatsIncludeFilter turns the raw "include" arg into a set. Returns
// nil when no filter was passed — caller treats nil as "everything". [Épica 228]
func parseStatsIncludeFilter(args map[string]any) map[string]struct{} {
	raw, ok := args["include"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			set[s] = struct{}{}
		}
	}
	return set
}

// parseStatsTopN reads the top_n override, defaulting to 5 and capping
// at 50 so the JSON doesn't balloon past tuning value. [Épica 228]
func parseStatsTopN(args map[string]any) int {
	topN := 5
	if v, ok := args["top_n"].(float64); ok && v > 0 {
		topN = int(v)
	}
	if topN > 50 {
		topN = 50
	}
	return topN
}

// buildQueryCacheSection renders the query_cache block or returns nil
// when the cache is unwired. [Épica 228]
func (t *CacheStatsTool) buildQueryCacheSection(topN int) map[string]any {
	if t.queryCache == nil {
		return nil
	}
	h, m, e, sz := t.queryCache.Stats()
	top := t.queryCache.Top(topN)
	topList := make([]map[string]any, 0, len(top))
	for _, entry := range top {
		topList = append(topList, map[string]any{
			"target":      entry.Target,
			"top_k":       entry.TopK,
			"result_size": entry.ResultSize,
			"hit_count":   entry.HitCount,
		})
	}
	wh, wm, wr := t.queryCache.WindowedHitRatio(window5m)
	return map[string]any{
		"hits":             h,
		"misses":           m,
		"evictions":        e,
		"size":             sz,
		"hit_ratio":        t.queryCache.HitRatio(),
		"top_5":            topList,
		"window_5m":        map[string]any{"hits": wh, "misses": wm, "hit_ratio": wr},
		"warmup_suggested": t.queryCache.RecentMissTargets(10),
	}
}

// buildTextCacheSection renders the text_cache block including the
// per-handler occupancy breakdown. Returns nil when unwired. [Épica 228]
func (t *CacheStatsTool) buildTextCacheSection(topN int) map[string]any {
	if t.textCache == nil {
		return nil
	}
	h, m, e, sz := t.textCache.Stats()
	top := t.textCache.Top(topN)
	topList := make([]map[string]any, 0, len(top))
	for _, entry := range top {
		topList = append(topList, map[string]any{
			"handler":   entry.Handler,
			"target":    entry.Target,
			"variant":   entry.Variant,
			"hit_count": entry.HitCount,
		})
	}
	wh, wm, wr := t.textCache.WindowedHitRatio(window5m)
	byHandler := t.textCache.HandlerBreakdown()
	breakdown := make(map[string]any, len(byHandler))
	for handler, agg := range byHandler {
		breakdown[handler] = map[string]any{"size": agg.Size, "total_hits": agg.TotalHits}
	}
	return map[string]any{
		"hits":             h,
		"misses":           m,
		"evictions":        e,
		"size":             sz,
		"hit_ratio":        t.textCache.HitRatio(),
		"top_5":            topList,
		"window_5m":        map[string]any{"hits": wh, "misses": wm, "hit_ratio": wr},
		"warmup_suggested": t.textCache.RecentMissTargets(10),
		"by_handler":       breakdown,
	}
}

// buildEmbeddingCacheSection renders the embedding_cache block. Returns
// nil when the cache is unwired. [Épica 228]
func (t *CacheStatsTool) buildEmbeddingCacheSection() map[string]any {
	if t.embCache == nil {
		return nil
	}
	h, m, e, sz := t.embCache.Stats()
	wh, wm, wr := t.embCache.WindowedHitRatio(window5m)
	return map[string]any{
		"hits":             h,
		"misses":           m,
		"evictions":        e,
		"size":             sz,
		"hit_ratio":        t.embCache.HitRatio(),
		"window_5m":        map[string]any{"hits": wh, "misses": wm, "hit_ratio": wr},
		"warmup_suggested": t.embCache.RecentMissTargets(10),
	}
}

// buildHotFilesCacheSection renders the hot_files block — per-workspace
// LRU file-content cache (LARGE-PROJECT/A 2026-05-13, commit 5d61e2d).
// Returns nil when unwired. Counters are coherent with the other cache
// layers' format so dashboards can render them identically.
func (t *CacheStatsTool) buildHotFilesCacheSection() map[string]any {
	if t.hotFiles == nil {
		return nil
	}
	s := t.hotFiles.Stats()
	return map[string]any{
		"hits":                s.Hits,
		"misses":              s.Misses,
		"stale_invalidations": s.Stale,
		"evictions":           s.Evictions,
		"entry_count":         s.EntryCount,
		"total_bytes":         s.TotalBytes,
		"capacity_bytes":      s.CapBytes,
		"hit_ratio":           s.HitRatio,
	}
}

// buildPlannerCacheSection renders the planner_cache block — per-workspace
// memoization of ReadActivePhase + ReadOpenTasks (LARGE-PROJECT/C 2026-05-13,
// commit a141f9a). Package-level cache so no nil-guard needed; always returns
// a valid map (even if no plans parsed yet).
func buildPlannerCacheSection() map[string]any {
	s := state.GetPlannerCacheStats()
	hits, misses := s.Hits, s.Misses
	ratio := 0.0
	if total := hits + misses; total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return map[string]any{
		"hits":                hits,
		"misses":              misses,
		"stale_invalidations": s.Stale,
		"entries":             s.Entries,
		"hit_ratio":           ratio,
	}
}

// buildPageRankCacheSection exposes the CPG PageRank memoisation
// counters. [Épica 228]
func buildPageRankCacheSection() map[string]any {
	prHits, prMisses := cpg.PageRankCacheStats()
	ratio := 0.0
	if total := prHits + prMisses; total > 0 {
		ratio = float64(prHits) / float64(total)
	}
	return map[string]any{
		"hits":      prHits,
		"misses":    prMisses,
		"hit_ratio": ratio,
	}
}

// buildToolLatencySection emits per-tool latency percentiles. Returns an
// empty map when the observability singleton is unwired — still a valid
// JSON object for dashboards. [Épica 228]
func buildToolLatencySection() map[string]any {
	latency := map[string]any{}
	if observability.GlobalToolLatency == nil {
		return latency
	}
	for _, name := range observability.GlobalToolLatency.Tools() {
		p50, p95, p99, n := observability.GlobalToolLatency.Percentiles(name)
		total := observability.GlobalToolLatency.TotalCalls(name)
		errCount := observability.GlobalToolLatency.ErrorCount(name)
		latency[name] = map[string]any{
			"p50_ns":         p50.Nanoseconds(),
			"p95_ns":         p95.Nanoseconds(),
			"p99_ns":         p99.Nanoseconds(),
			"window_count":   n,
			"lifetime_count": total,
			"error_count":    errCount,
			"error_rate":     observability.GlobalToolLatency.ErrorRate(name),
		}
	}
	return latency
}

func (t *CacheStatsTool) Execute(_ context.Context, args map[string]any) (any, error) {
	includeSet := parseStatsIncludeFilter(args)
	topN := parseStatsTopN(args)
	includes := func(section string) bool {
		if includeSet == nil {
			return true
		}
		_, ok := includeSet[section]
		return ok
	}

	out := map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
	}
	if t.workspace != "" {
		out["snapshots"] = cacheSnapshotInfo(t.workspace)
	}
	if includes("query_cache") {
		if s := t.buildQueryCacheSection(topN); s != nil {
			out["query_cache"] = s
		}
	}
	if includes("text_cache") {
		if s := t.buildTextCacheSection(topN); s != nil {
			out["text_cache"] = s
		}
	}
	if includes("embedding_cache") {
		if s := t.buildEmbeddingCacheSection(); s != nil {
			out["embedding_cache"] = s
		}
	}
	// [LARGE-PROJECT/A 2026-05-13] HotFilesCache observability — gated on
	// availability (tool may run pre-RadarTool init where the cache is unwired).
	if includes("hot_files") {
		if s := t.buildHotFilesCacheSection(); s != nil {
			out["hot_files"] = s
		}
	}
	// [LARGE-PROJECT/C 2026-05-13] PlannerCache observability — always available
	// (pkg-level global). Empty stats are valid output.
	if includes("planner_cache") {
		out["planner_cache"] = buildPlannerCacheSection()
	}
	if includes("search_paths") {
		out["search_paths"] = map[string]any{
			"binary_count": rag.SearchBinaryCount(),
			"hybrid_count": rag.HybridSearchCount(),
			"int8_count":   rag.SearchInt8Count(),
		}
	}
	if includes("pagerank_cache") {
		out["pagerank_cache"] = buildPageRankCacheSection()
	}
	if includes("tool_latency") {
		out["tool_latency"] = buildToolLatencySection()
	}
	// [295.E] Knowledge Store stats — included when knowledgeStats is wired (project mode).
	if t.knowledgeStats != nil && includes("knowledge_store") {
		hot, total := t.knowledgeStats()
		out["knowledge_store"] = map[string]any{
			"hot":   hot,
			"total": total,
			"cold":  total - hot,
		}
	}

	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(buf)}},
	}, nil
}
