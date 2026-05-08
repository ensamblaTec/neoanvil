// cmd/neo-mcp/tool_cache_warmup.go — pre-populates the RAG caches by
// replaying a list of (handler, target) pairs. [PILAR-XXV/194]
//
// Motivation: after make rebuild-restart, both caches start empty and
// the first N requests pay full HNSW/CPG cost. On a debugging session
// where the operator re-uses the same 5-10 targets across 50 queries,
// that first cold pass adds seconds of wall-time. Warmup lets the
// operator front-load the "hot set" in parallel and amortize the cost
// before the iteration starts.
//
// Design:
//   - Accepts a list of targets (typically file paths or semantic queries).
//   - For each target, fires the listed handlers in parallel (sem=4).
//   - Each handler call goes through the normal dispatcher, so cache
//     behaviour is identical to a user-issued call — no bypass paths.
//   - Returns a summary: total fills, average latency, parallel degree.

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

type CacheWarmupTool struct {
	radar      *RadarTool
	queryCache *rag.QueryCache // [203] source for from_recent mode
	textCache  *rag.TextCache  // [203] source for from_recent mode
}

func (t *CacheWarmupTool) Name() string { return "neo_cache_warmup" }

func (t *CacheWarmupTool) Description() string {
	return "SRE Tool: Pre-populates both RAG caches by dispatching SEMANTIC_CODE and/or BLAST_RADIUS against a list of targets in parallel. Use after make rebuild-restart to skip the cold-cache tax. Semaphore of 4 goroutines prevents HNSW saturation. from_recent:true auto-sources targets from the RecentMissTargets rings (196) — closes the observe→warm loop without the operator copy-pasting the list."
}

func (t *CacheWarmupTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"targets": map[string]any{
				"type":        "array",
				"description": "List of targets to warm up. For SEMANTIC_CODE each is a natural-language query; for BLAST_RADIUS each is a file path. Mixed lists fire both handlers when handlers is omitted. Ignored when from_recent=true.",
				"items":       map[string]any{"type": "string"},
			},
			"handlers": map[string]any{
				"type":        "array",
				"description": "Optional. Which handlers to warm. Subset of ['SEMANTIC_CODE', 'BLAST_RADIUS']. Default: both.",
				"items":       map[string]any{"type": "string", "enum": []string{"SEMANTIC_CODE", "BLAST_RADIUS"}},
			},
			"from_recent": map[string]any{
				"type":        "boolean",
				"description": "[Épica 203] Auto-source targets from the recent-miss rings of both caches (up to 20 targets combined). When true, 'targets' is ignored.",
			},
		},
		Required: []string{},
	}
}

// callResult captures one handler call outcome for the warmup summary.
// Extracted to package scope so helpers can return/consume it. [Épica 228]
type callResult struct {
	Handler string
	Target  string
	Err     string // empty on success
	DurMs   int64
}

// resolveWarmupTargets returns the deduped target list from args.
// from_recent=true short-circuits the explicit list by pulling from
// the RecentMissTargets rings. Nil result + nil error = friendly
// empty-case (caller renders info msg). [Épica 228]
func (t *CacheWarmupTool) resolveWarmupTargets(args map[string]any) ([]string, error) {
	if fromRecent, _ := args["from_recent"].(bool); fromRecent {
		return t.collectRecentMisses(), nil
	}
	rawTargets, _ := args["targets"].([]any)
	if len(rawTargets) == 0 {
		return nil, fmt.Errorf("cache_warmup: targets is required (or set from_recent:true)")
	}
	targets := make([]string, 0, len(rawTargets))
	for _, v := range rawTargets {
		if s, ok := v.(string); ok && s != "" {
			targets = append(targets, s)
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("cache_warmup: targets must contain at least one non-empty string")
	}
	return targets, nil
}

// collectRecentMisses walks both RecentMissTargets rings and returns
// deduped list. [Épica 228]
func (t *CacheWarmupTool) collectRecentMisses() []string {
	seen := make(map[string]struct{})
	var targets []string
	add := func(s string) {
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		targets = append(targets, s)
	}
	if t.queryCache != nil {
		for _, s := range t.queryCache.RecentMissTargets(10) {
			add(s)
		}
	}
	if t.textCache != nil {
		for _, s := range t.textCache.RecentMissTargets(10) {
			add(s)
		}
	}
	return targets
}

// resolveWarmupHandlers parses the handlers arg, returning the full
// default set when absent. [Épica 228]
func resolveWarmupHandlers(args map[string]any) ([]string, error) {
	handlers := []string{"SEMANTIC_CODE", "BLAST_RADIUS"}
	rawH, ok := args["handlers"].([]any)
	if !ok || len(rawH) == 0 {
		return handlers, nil
	}
	handlers = handlers[:0]
	for _, v := range rawH {
		if s, ok := v.(string); ok && (s == "SEMANTIC_CODE" || s == "BLAST_RADIUS") {
			handlers = append(handlers, s)
		}
	}
	if len(handlers) == 0 {
		return nil, fmt.Errorf("cache_warmup: no valid handlers — want SEMANTIC_CODE or BLAST_RADIUS")
	}
	return handlers, nil
}

// runWarmupBatch fires the (target × handler) cross product in parallel
// under a sem=4 semaphore. Returns per-call results + totals. [Épica 228]
func (t *CacheWarmupTool) runWarmupBatch(ctx context.Context, targets, handlers []string) ([]callResult, int64, int64) {
	const maxConcurrent = 4
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	var successes, failures int64
	results := make([]callResult, len(targets)*len(handlers))
	idx := 0
	for _, target := range targets {
		for _, handler := range handlers {
			slot := idx
			idx++
			wg.Add(1)
			go func(h, tgt string, slot int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				callStart := time.Now()
				var err error
				switch h {
				case "SEMANTIC_CODE":
					_, err = t.radar.handleSemanticCode(ctx, map[string]any{"target": tgt})
				case "BLAST_RADIUS":
					_, err = t.radar.handleBlastRadius(ctx, map[string]any{"target": tgt})
				}
				row := callResult{Handler: h, Target: tgt, DurMs: time.Since(callStart).Milliseconds()}
				if err != nil {
					atomic.AddInt64(&failures, 1)
					row.Err = err.Error()
				} else {
					atomic.AddInt64(&successes, 1)
				}
				results[slot] = row
			}(handler, target, slot)
		}
	}
	wg.Wait()
	return results, successes, failures
}

// formatWarmupReport assembles the markdown summary + failed-first
// per-call table. [Épica 228]
func formatWarmupReport(results []callResult, successes, failures int64, nTargets, nHandlers int, dur time.Duration) string {
	total := successes + failures
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Warmup complete — %d/%d succeeded (%d failed) across %d targets × %d handlers in %s (parallel=4).\n\n",
		successes, total, failures, nTargets, nHandlers, dur.Truncate(time.Millisecond))
	sb.WriteString("| handler | target | ms | status |\n|---------|--------|----|--------|\n")
	for _, r := range results {
		if r.Err != "" {
			fmt.Fprintf(&sb, "| %s | %s | %d | ❌ %s |\n", r.Handler, r.Target, r.DurMs, r.Err)
		}
	}
	for _, r := range results {
		if r.Err == "" {
			fmt.Fprintf(&sb, "| %s | %s | %d | ✓ |\n", r.Handler, r.Target, r.DurMs)
		}
	}
	return sb.String()
}

func (t *CacheWarmupTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	if t.radar == nil {
		return nil, fmt.Errorf("cache_warmup: radar not wired")
	}
	targets, err := t.resolveWarmupTargets(args)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ℹ️  No recent misses recorded — caches are warm or session is fresh. Run some queries first."}},
		}, nil
	}
	handlers, err := resolveWarmupHandlers(args)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	results, successes, failures := t.runWarmupBatch(ctx, targets, handlers)
	msg := formatWarmupReport(results, successes, failures, len(targets), len(handlers), time.Since(start))
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
	}, nil
}
