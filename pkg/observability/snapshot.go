// pkg/observability/snapshot.go — unified read model for HUD + TUI.
// [PILAR-XXVII/244]

package observability

import (
	"runtime"
	"sort"
	"time"
)

const snapshotSchemaVersion = 1

// Snapshot is the canonical read-model served at /api/v1/metrics. Every
// field is a lossy aggregate — the Store itself remains the source of
// truth and can always be re-queried for drill-down.
type Snapshot struct {
	SchemaVersion int             `json:"schema_version"`
	WorkspaceID   string          `json:"workspace_id"`
	WorkspaceName string          `json:"workspace_name"`
	UptimeSeconds int64           `json:"uptime_seconds"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Memory        MemorySection   `json:"memory"`
	Tools         ToolsSection    `json:"tools"`
	Tokens        TokensSection   `json:"tokens"`
	Mutations     MutationsSection `json:"mutations"`
	Events        []EventEntry    `json:"recent_events"`
}

// MemorySection captures the most recent MemStatsSnapshot plus a few
// process-level numbers that weren't persisted (NumCPU).
type MemorySection struct {
	HeapMB         float64 `json:"heap_mb"`
	StackMB        float64 `json:"stack_mb"`
	Goroutines     int     `json:"goroutines"`
	GCRuns         uint32  `json:"gc_runs"`
	GCPauseLastMs  float64 `json:"gc_pause_last_ms"`
	CPGHeapMB      int     `json:"cpg_heap_mb"`
	CPGHeapLimitMB int     `json:"cpg_heap_limit_mb"`
	CPGHeapPct     int     `json:"cpg_heap_pct"`
	NumCPU         int     `json:"num_cpu"`
	QueryCacheHit  float64 `json:"query_cache_hit_rate"`
	TextCacheHit   float64 `json:"text_cache_hit_rate"`
	EmbCacheHit    float64 `json:"emb_cache_hit_rate"`
}

// ToolsSection aggregates per-tool stats across the persisted bucket.
type ToolsSection struct {
	TopByCalls  []ToolStats `json:"top_by_calls"`
	TopByErrors []ToolStats `json:"top_by_errors"`
	TopByP99    []ToolStats `json:"top_by_p99"`
	Total24h    int         `json:"total_calls_24h"`
}

// ToolStats is one row in a Top-N list.
type ToolStats struct {
	Name       string    `json:"name"`
	Calls      int       `json:"calls"`
	Errors     int       `json:"errors"`
	ErrorRate  float64   `json:"error_rate"`
	P50Ms      float64   `json:"p50_ms"`
	P95Ms      float64   `json:"p95_ms"`
	P99Ms      float64   `json:"p99_ms"`
	LastCallAt time.Time `json:"last_call_at"`
}

// TokensSection breaks per-day token flow by source (mcp_traffic vs
// internal_inference) and provides a week-long trend.
type TokensSection struct {
	TodayInputTokens  int               `json:"today_input_tokens"`
	TodayOutputTokens int               `json:"today_output_tokens"`
	TodayCostUSD      float64           `json:"today_cost_usd"`
	MCPTraffic        TokenBreakdown    `json:"mcp_traffic"`
	InternalInference TokenBreakdown    `json:"internal_inference"`
	Last7Days         []TokenDaySummary `json:"last_7_days"`
}

// TokenBreakdown is the per-source rollup for the current day.
type TokenBreakdown struct {
	InputTokens  int            `json:"input_tokens"`
	OutputTokens int            `json:"output_tokens"`
	CostUSD      float64        `json:"cost_usd"`
	ByAgent      map[string]int `json:"by_agent"`
	ByTool       map[string]int `json:"by_tool"`
	ByPromptType map[string]int `json:"by_prompt_type,omitempty"`
}

// MutationsSection covers the last-24 h certified + bypassed counts and
// top files by certification count.
type MutationsSection struct {
	Certified24h int            `json:"certified_24h"`
	Bypassed24h  int            `json:"bypassed_24h"`
	TopHotspots  []HotspotEntry `json:"top_hotspots"`
}

// Snapshot builds a read-only view of the store. Idempotent: two calls
// with no intervening writes return the same data.
func (s *Store) Snapshot(workspaceID, workspaceName string, bootUnix int64) Snapshot {
	if s == nil {
		return Snapshot{
			SchemaVersion: snapshotSchemaVersion,
			WorkspaceID:   workspaceID,
			WorkspaceName: workspaceName,
			GeneratedAt:   time.Now().UTC(),
			Memory:        MemorySection{NumCPU: runtime.NumCPU()},
		}
	}

	now := s.now().UTC()
	uptime := int64(0)
	if bootUnix > 0 {
		uptime = now.Unix() - bootUnix
	}

	return Snapshot{
		SchemaVersion: snapshotSchemaVersion,
		WorkspaceID:   workspaceID,
		WorkspaceName: workspaceName,
		UptimeSeconds: uptime,
		GeneratedAt:   now,
		Memory:        s.buildMemorySection(),
		Tools:         s.buildToolsSection(),
		Tokens:        s.buildTokensSection(now),
		Mutations:     s.buildMutationsSection(),
		Events:        s.RecentEvents(50),
	}
}

func (s *Store) buildMemorySection() MemorySection {
	hist := s.MemStatsHistory(s.now().Add(-10 * time.Minute))
	mem := MemorySection{NumCPU: runtime.NumCPU()}
	if len(hist) > 0 {
		latest := hist[len(hist)-1]
		mem.HeapMB = latest.HeapMB
		mem.StackMB = latest.StackMB
		mem.Goroutines = latest.Goroutines
		mem.GCRuns = latest.GCRuns
		mem.GCPauseLastMs = float64(latest.GCPauseLastNs) / 1e6
		mem.CPGHeapMB = latest.CPGHeapMB
		mem.CPGHeapLimitMB = latest.CPGHeapLimitMB
		if latest.CPGHeapLimitMB > 0 {
			mem.CPGHeapPct = (latest.CPGHeapMB * 100) / latest.CPGHeapLimitMB
		}
		mem.QueryCacheHit = latest.QueryCacheHit
		mem.TextCacheHit = latest.TextCacheHit
		mem.EmbCacheHit = latest.EmbCacheHit
	}
	return mem
}

func (s *Store) buildToolsSection() ToolsSection {
	aggs := s.ToolAggregates()
	rows := make([]ToolStats, 0, len(aggs))
	total := 0
	for _, a := range aggs {
		rows = append(rows, toolAggToStats(a))
		total += a.Calls
	}
	return ToolsSection{
		TopByCalls:  topToolStats(rows, func(a, b ToolStats) bool { return a.Calls > b.Calls }, 10),
		TopByErrors: topToolStats(rows, func(a, b ToolStats) bool { return a.Errors > b.Errors }, 10),
		TopByP99:    topToolStats(rows, func(a, b ToolStats) bool { return a.P99Ms > b.P99Ms }, 10),
		Total24h:    total,
	}
}

func toolAggToStats(a ToolAggregate) ToolStats {
	return ToolStats{
		Name:       a.Name,
		Calls:      a.Calls,
		Errors:     a.Errors,
		ErrorRate:  a.ErrorRate(),
		P50Ms:      float64(a.P50Ns) / 1e6,
		P95Ms:      float64(a.P95Ns) / 1e6,
		P99Ms:      float64(a.P99Ns) / 1e6,
		LastCallAt: a.LastCallAt,
	}
}

// topToolStats copies, sorts by the supplied less-than, and truncates.
func topToolStats(in []ToolStats, less func(a, b ToolStats) bool, n int) []ToolStats {
	out := make([]ToolStats, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (s *Store) buildTokensSection(now time.Time) TokensSection {
	day := now.Format("2006-01-02")
	bySrc := s.TokensBySource(day)
	sec := TokensSection{
		MCPTraffic:        aggregateBreakdown(bySrc[SourceMCPTraffic]),
		InternalInference: aggregateBreakdown(bySrc[SourceInternalInference]),
		Last7Days:         s.TokensLast7Days(),
	}
	sec.TodayInputTokens = sec.MCPTraffic.InputTokens + sec.InternalInference.InputTokens
	sec.TodayOutputTokens = sec.MCPTraffic.OutputTokens + sec.InternalInference.OutputTokens
	sec.TodayCostUSD = sec.MCPTraffic.CostUSD + sec.InternalInference.CostUSD
	return sec
}

// aggregateBreakdown rolls a list of same-source entries into one
// TokenBreakdown with by-agent / by-tool / by-prompt-type maps.
func aggregateBreakdown(entries []TokenEntry) TokenBreakdown {
	b := TokenBreakdown{
		ByAgent:      map[string]int{},
		ByTool:       map[string]int{},
		ByPromptType: map[string]int{},
	}
	for _, e := range entries {
		b.InputTokens += e.InputTokens
		b.OutputTokens += e.OutputTokens
		b.CostUSD += e.CostUSD
		tot := e.InputTokens + e.OutputTokens
		if e.Agent != "" {
			b.ByAgent[e.Agent] += tot
		}
		if e.Tool != "" {
			b.ByTool[e.Tool] += tot
		}
		if e.PromptType != "" {
			b.ByPromptType[e.PromptType] += tot
		}
	}
	// Keep the map empty-but-non-nil so JSON encoders serialise {}.
	if len(b.ByPromptType) == 0 {
		b.ByPromptType = nil
	}
	return b
}

func (s *Store) buildMutationsSection() MutationsSection {
	cert, byp, tops := s.MutationsLast24h()
	return MutationsSection{
		Certified24h: cert,
		Bypassed24h:  byp,
		TopHotspots:  tops,
	}
}
