// cmd/neo-mcp/tool_tool_stats.go — dedicated JSON view of the MCP tool
// latency tracker. [PILAR-XXV/207]
//
// The same tool_latency block lives inside neo_cache_stats (184) for
// operators who want a single-call snapshot. This tool exists because:
//   - It returns ONLY tool data, fitting in a smaller token budget
//     when the operator is focused on a performance issue, not a
//     cache tuning session.
//   - It accepts a sort_by arg so the output leads with the metric the
//     operator cares about (p99 latency, error rate, call volume).
//   - It's a pure read with no side effects — safe to poll.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// ToolStatsTool reports per-tool latency/error stats. workspace is used when
// scope:"project" is requested to load the project federation config. [339.A]
type ToolStatsTool struct {
	workspace string
}

func (t *ToolStatsTool) Name() string { return "neo_tool_stats" }

func (t *ToolStatsTool) Description() string {
	return "SRE Tool: Returns JSON with per-MCP-tool p50/p95/p99 latency, error counts, and call volume from the 512-sample ring buffer. Accepts sort_by to lead with the metric that matters (p99, errors, calls). Pure read, zero side effects."
}

func (t *ToolStatsTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"sort_by": map[string]any{
				"type":        "string",
				"description": "Which metric to sort descending by. 'p99' (default) surfaces the slowest-tail tools; 'errors' surfaces failing tools; 'calls' surfaces the most-used.",
				"enum":        []string{"p99", "p95", "p50", "errors", "calls"},
			},
			"top": map[string]any{
				"type":        "integer",
				"description": "How many tools to return after sorting. Default: all tools with at least 1 sample.",
			},
			"format": map[string]any{
				"type":        "string",
				"description": "[Épica 220] Output encoding. 'json' (default) for machine consumption; 'csv' for piping to analysis spreadsheets. CSV headers: name,p50_ms,p95_ms,p99_ms,window,lifetime,errors,error_rate.",
				"enum":        []string{"json", "csv"},
			},
			"scope": map[string]any{
				"type":        "string",
				"description": "[339.A] Aggregation scope. 'workspace' (default) returns stats for this workspace only. 'project' scatters to all member workspaces via Nexus and returns an aggregated view with per-workspace breakdown and project_token_budget_daily utilization.",
				"enum":        []string{"workspace", "project"},
			},
		},
		Required: []string{},
	}
}

type toolRow struct {
	Name      string        `json:"name"`
	P50       time.Duration `json:"p50_ns"`
	P95       time.Duration `json:"p95_ns"`
	P99       time.Duration `json:"p99_ns"`
	Window    int           `json:"window_count"`
	Lifetime  int           `json:"lifetime_count"`
	Errors    int           `json:"error_count"`
	ErrorRate float64       `json:"error_rate"`
}

type tokenRow struct {
	Tool         string  `json:"tool"`
	Calls        int     `json:"calls"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// collectTokenRows aggregates token spend per tool from the observability
// store. Returns rows sorted by total_tokens descending. [Épica 311.A]
func collectTokenRows() []tokenRow {
	if observability.GlobalStore == nil {
		return nil
	}
	// Aggregate all entries regardless of day or source.
	byTool := make(map[string]*tokenRow)
	for _, entries := range observability.GlobalStore.TokensBySource("") {
		for _, e := range entries {
			if e.Tool == "" {
				continue
			}
			r, ok := byTool[e.Tool]
			if !ok {
				r = &tokenRow{Tool: e.Tool}
				byTool[e.Tool] = r
			}
			r.Calls += e.Calls
			r.InputTokens += e.InputTokens
			r.OutputTokens += e.OutputTokens
			r.TotalTokens += e.InputTokens + e.OutputTokens
			r.CostUSD += e.CostUSD
		}
	}
	rows := make([]tokenRow, 0, len(byTool))
	for _, r := range byTool {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].TotalTokens > rows[j].TotalTokens
	})
	return rows
}

// collectToolRows walks the observability tracker and builds one row per
// registered tool. Returns an empty slice when the tracker is unwired so
// the caller never needs to nil-guard. [Épica 228]
func collectToolRows() []toolRow {
	if observability.GlobalToolLatency == nil {
		return nil
	}
	names := observability.GlobalToolLatency.Tools()
	rows := make([]toolRow, 0, len(names))
	for _, name := range names {
		p50, p95, p99, window := observability.GlobalToolLatency.Percentiles(name)
		lifetime := observability.GlobalToolLatency.TotalCalls(name)
		errs := observability.GlobalToolLatency.ErrorCount(name)
		rate := observability.GlobalToolLatency.ErrorRate(name)
		rows = append(rows, toolRow{
			Name: name, P50: p50, P95: p95, P99: p99,
			Window: window, Lifetime: lifetime,
			Errors: errs, ErrorRate: rate,
		})
	}
	return rows
}

// sortToolRows sorts descending by the caller's metric of choice.
// Unknown metrics fall through to p99 (the default operators want when
// investigating tail latency). [Épica 228]
func sortToolRows(rows []toolRow, sortBy string) {
	less := func(i, j int) bool { return rows[i].P99 > rows[j].P99 }
	switch sortBy {
	case "p50":
		less = func(i, j int) bool { return rows[i].P50 > rows[j].P50 }
	case "p95":
		less = func(i, j int) bool { return rows[i].P95 > rows[j].P95 }
	case "errors":
		less = func(i, j int) bool { return rows[i].Errors > rows[j].Errors }
	case "calls":
		less = func(i, j int) bool { return rows[i].Lifetime > rows[j].Lifetime }
	}
	sort.SliceStable(rows, less)
}

// formatToolStatsCSV renders rows as a flat CSV — easy to paste into a
// spreadsheet for longer-term performance tracking across sessions. [Épica 228]
func formatToolStatsCSV(rows []toolRow) string {
	var sb strings.Builder
	sb.WriteString("name,p50_ms,p95_ms,p99_ms,window,lifetime,errors,error_rate\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%s,%.3f,%.3f,%.3f,%d,%d,%d,%.4f\n",
			r.Name,
			float64(r.P50.Microseconds())/1000.0,
			float64(r.P95.Microseconds())/1000.0,
			float64(r.P99.Microseconds())/1000.0,
			r.Window, r.Lifetime, r.Errors, r.ErrorRate,
		)
	}
	return sb.String()
}

// pluginMetricRow is a single entry from Nexus GET /api/v1/plugin_metrics.
// Fields mirror pluginMetricSnapshot in cmd/neo-nexus/plugin_metrics.go.
type pluginMetricRow struct {
	Plugin      string `json:"plugin"`
	Tool        string `json:"tool"`
	Calls       int64  `json:"calls"`
	Errors      int64  `json:"errors"`
	Rejections  int64  `json:"rejections"`
	CacheHits   int64  `json:"cache_hits"`
	P50Ns       int64  `json:"p50_ns"`
	P95Ns       int64  `json:"p95_ns"`
	P99Ns       int64  `json:"p99_ns"`
	SampleCount int64  `json:"sample_count"`
}

// fetchNexusPluginMetrics calls GET /api/v1/plugin_metrics on the Nexus
// dispatcher and returns the rows sorted by (plugin, tool). Returns nil
// when Nexus is unreachable or the endpoint is unavailable — callers must
// handle nil gracefully. [154.F]
func fetchNexusPluginMetrics() []pluginMetricRow {
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return nil
	}
	client := sre.SafeInternalHTTPClient(2)
	url := nexusBase + "/api/v1/plugin_metrics" //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase loopback-only via SafeInternalHTTPClient
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Plugins []pluginMetricRow `json:"plugins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	return body.Plugins
}

// wsStatsResult holds stats gathered from one member workspace in a scatter. [339.A]
type wsStatsResult struct {
	name   string
	tools  []toolRow
	tokens []tokenRow
}

// fetchMemberToolStats calls neo_tool_stats on a single remote workspace and returns the result.
func fetchMemberToolStats(ctx context.Context, client *http.Client, nexusBase, mp, id, sortBy string, topN int) wsStatsResult {
	payload, mErr := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "neo_tool_stats",
			"arguments": map[string]any{
				"sort_by": sortBy,
				"top":     topN,
				"scope":   "workspace",
			},
		},
	})
	if mErr != nil {
		return wsStatsResult{}
	}
	url := nexusBase + "/workspaces/" + id + "/mcp/message" //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: nexusBase loopback-only
	req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if rErr != nil {
		return wsStatsResult{}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := client.Do(req)
	if doErr != nil {
		return wsStatsResult{}
	}
	defer resp.Body.Close() //nolint:errcheck
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if jErr := json.NewDecoder(resp.Body).Decode(&rpc); jErr != nil || len(rpc.Result.Content) == 0 {
		return wsStatsResult{}
	}
	var statsOut struct {
		Tools      []toolRow  `json:"tools"`
		TokenSpend []tokenRow `json:"token_spend"`
	}
	if uErr := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &statsOut); uErr != nil {
		return wsStatsResult{}
	}
	return wsStatsResult{name: filepath.Base(mp), tools: statsOut.Tools, tokens: statsOut.TokenSpend}
}

// aggregateToolRows merges tool latency rows across workspaces: max tail latencies, sum counts.
func aggregateToolRows(results []wsStatsResult, sortBy string) []toolRow {
	toolByName := make(map[string]*toolRow)
	for i := range results {
		for _, r := range results[i].tools {
			agg, ok := toolByName[r.Name]
			if !ok {
				cp := r
				toolByName[r.Name] = &cp
				continue
			}
			if r.P99 > agg.P99 { agg.P99 = r.P99 }
			if r.P95 > agg.P95 { agg.P95 = r.P95 }
			if r.P50 > agg.P50 { agg.P50 = r.P50 }
			agg.Window += r.Window
			agg.Lifetime += r.Lifetime
			agg.Errors += r.Errors
		}
	}
	out := make([]toolRow, 0, len(toolByName))
	for _, r := range toolByName {
		if r.Lifetime > 0 {
			r.ErrorRate = float64(r.Errors) / float64(r.Lifetime)
		}
		out = append(out, *r)
	}
	sortToolRows(out, sortBy)
	return out
}

// aggregateTokenRows merges token-spend rows across workspaces and returns the total.
func aggregateTokenRows(results []wsStatsResult) ([]tokenRow, int) {
	tokenByTool := make(map[string]*tokenRow)
	var total int
	for i := range results {
		for _, r := range results[i].tokens {
			total += r.TotalTokens
			agg, ok := tokenByTool[r.Tool]
			if !ok {
				cp := r
				tokenByTool[r.Tool] = &cp
				continue
			}
			agg.Calls += r.Calls
			agg.InputTokens += r.InputTokens
			agg.OutputTokens += r.OutputTokens
			agg.TotalTokens += r.TotalTokens
			agg.CostUSD += r.CostUSD
		}
	}
	out := make([]tokenRow, 0, len(tokenByTool))
	for _, r := range tokenByTool {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalTokens > out[j].TotalTokens })
	return out, total
}

// scatterProjectStats gathers neo_tool_stats from every member workspace in the
// project federation via Nexus MCP proxy, then aggregates the results.
// Also computes token budget utilization when project.token_budget_daily is set.
func (t *ToolStatsTool) scatterProjectStats(ctx context.Context, topN int, sortBy string) (map[string]any, error) {
	nexusBase := nexusDispatcherBase()
	if nexusBase == "" {
		return nil, fmt.Errorf("Nexus dispatcher not reachable — scope:project requires Nexus running")
	}
	pc, err := config.LoadProjectConfig(t.workspace)
	if err != nil {
		return nil, fmt.Errorf("load project config: %w", err)
	}
	if pc == nil {
		return nil, fmt.Errorf("no .neo-project/neo.yaml found — scope:project requires project federation")
	}

	memberIDs := nexusMemberWorkspaceIDs(nexusBase, pc.MemberWorkspaces)
	client := sre.SafeInternalHTTPClient(5)
	var mu sync.Mutex
	var wgResults []wsStatsResult
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)

	for memberPath, wsID := range memberIDs {
		wg.Add(1)
		go func(mp, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result := fetchMemberToolStats(ctx, client, nexusBase, mp, id, sortBy, topN)
			if result.name != "" {
				mu.Lock()
				wgResults = append(wgResults, result)
				mu.Unlock()
			}
		}(memberPath, wsID)
	}
	wg.Wait()

	selfTools := collectToolRows()
	sortToolRows(selfTools, sortBy)
	wgResults = append(wgResults, wsStatsResult{name: filepath.Base(t.workspace), tools: selfTools, tokens: collectTokenRows()})

	aggTools := aggregateToolRows(wgResults, sortBy)
	aggTokens, totalTokens := aggregateTokenRows(wgResults)

	type wsSummary struct {
		Workspace  string     `json:"workspace"`
		ToolCount  int        `json:"tool_count"`
		TotalCalls int        `json:"total_calls"`
		TokenSpend []tokenRow `json:"token_spend"`
	}
	wsRows := make([]wsSummary, 0, len(wgResults))
	for _, ws := range wgResults {
		var totalCalls int
		for _, r := range ws.tools {
			totalCalls += r.Lifetime
		}
		wsRows = append(wsRows, wsSummary{
			Workspace:  ws.name,
			ToolCount:  len(ws.tools),
			TotalCalls: totalCalls,
			TokenSpend: ws.tokens,
		})
	}

	out := map[string]any{
		"scope":                   "project",
		"project_name":            pc.ProjectName,
		"generated_at":            time.Now().Format(time.RFC3339),
		"sort_by":                 sortBy,
		"aggregated_tools":        aggTools,
		"aggregated_token_spend":  aggTokens,
		"workspaces":              wsRows,
	}
	if pc.TokenBudgetDaily > 0 {
		remaining := pc.TokenBudgetDaily - totalTokens
		utilPct := math.Round(float64(totalTokens)/float64(pc.TokenBudgetDaily)*10000) / 100
		out["token_budget_daily"] = pc.TokenBudgetDaily
		out["token_budget_used"] = totalTokens
		out["token_budget_remaining"] = remaining
		out["budget_utilization_pct"] = utilPct
		if remaining < 0 {
			out["budget_exceeded"] = true
		}
	}
	return out, nil
}

func (t *ToolStatsTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	if observability.GlobalToolLatency == nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "{\"note\": \"tool latency tracker not initialised yet\"}"}},
		}, nil
	}
	sortBy, _ := args["sort_by"].(string)
	if sortBy == "" {
		sortBy = "p99"
	}
	topN := 0
	if v, ok := args["top"].(float64); ok && v > 0 {
		topN = int(v)
	}

	// scope:project — scatter to all member workspaces via Nexus. [339.A]
	if scope, _ := args["scope"].(string); scope == "project" {
		out, err := t.scatterProjectStats(ctx, topN, sortBy)
		if err != nil {
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("{\"error\": %q}", err.Error())}},
			}, nil
		}
		buf, mErr := json.MarshalIndent(out, "", "  ")
		if mErr != nil {
			return nil, fmt.Errorf("tool_stats: %w", mErr)
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(buf)}},
		}, nil
	}

	rows := collectToolRows()
	sortToolRows(rows, sortBy)
	if topN > 0 && len(rows) > topN {
		rows = rows[:topN]
	}

	if format, _ := args["format"].(string); format == "csv" {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": formatToolStatsCSV(rows)}},
		}, nil
	}

	out := map[string]any{
		"generated_at":   time.Now().Format(time.RFC3339),
		"sort_by":        sortBy,
		"tools":          rows,
		"token_spend":    collectTokenRows(),
		"plugin_metrics": fetchNexusPluginMetrics(), // [154.F] nil when Nexus unreachable
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("tool_stats: %w", err)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(buf)}},
	}, nil
}
