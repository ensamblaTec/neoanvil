package main

// api.go — HTTP client against Nexus' observability endpoints.
// [PILAR-XXVII/246.B]

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Snapshot mirrors pkg/observability.Snapshot. We copy the shape here
// (not import it) because cmd/neo-tui is a separate Go module with its
// own go.mod — pulling observability would drag BoltDB + friends into
// the TUI binary. JSON is the contract boundary.
type Snapshot struct {
	SchemaVersion int              `json:"schema_version"`
	WorkspaceID   string           `json:"workspace_id"`
	WorkspaceName string           `json:"workspace_name"`
	UptimeSeconds int64            `json:"uptime_seconds"`
	GeneratedAt   time.Time        `json:"generated_at"`
	Memory        MemorySection    `json:"memory"`
	Tools         ToolsSection     `json:"tools"`
	Tokens        TokensSection    `json:"tokens"`
	Mutations     MutationsSection `json:"mutations"`
	Events        []Event          `json:"recent_events"`
}

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

type ToolsSection struct {
	TopByCalls  []ToolStats `json:"top_by_calls"`
	TopByErrors []ToolStats `json:"top_by_errors"`
	TopByP99    []ToolStats `json:"top_by_p99"`
	Total24h    int         `json:"total_calls_24h"`
}

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

type TokensSection struct {
	TodayInputTokens  int               `json:"today_input_tokens"`
	TodayOutputTokens int               `json:"today_output_tokens"`
	TodayCostUSD      float64           `json:"today_cost_usd"`
	MCPTraffic        TokenBreakdown    `json:"mcp_traffic"`
	InternalInference TokenBreakdown    `json:"internal_inference"`
	Last7Days         []TokenDaySummary `json:"last_7_days"`
}

type TokenBreakdown struct {
	InputTokens  int            `json:"input_tokens"`
	OutputTokens int            `json:"output_tokens"`
	CostUSD      float64        `json:"cost_usd"`
	ByAgent      map[string]int `json:"by_agent"`
	ByTool       map[string]int `json:"by_tool"`
	ByPromptType map[string]int `json:"by_prompt_type,omitempty"`
}

type TokenDaySummary struct {
	Day            string  `json:"day"`
	MCPInput       int     `json:"mcp_input"`
	MCPOutput      int     `json:"mcp_output"`
	InternalInput  int     `json:"internal_input"`
	InternalOutput int     `json:"internal_output"`
	CostUSD        float64 `json:"cost_usd"`
}

type MutationsSection struct {
	Certified24h int            `json:"certified_24h"`
	Bypassed24h  int            `json:"bypassed_24h"`
	TopHotspots  []HotspotEntry `json:"top_hotspots"`
}

type HotspotEntry struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

type Event struct {
	Timestamp time.Time      `json:"ts"`
	Type      string         `json:"type"`
	Severity  string         `json:"severity,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// Client fetches metrics from Nexus. Zero-value Client uses default
// Nexus addr on localhost.
type Client struct {
	NexusBase string      // e.g. http://127.0.0.1:9000
	HTTP      http.Client // timeout 1.5 s — we poll every 3 s
}

// NewClient builds a Client with sensible defaults.
func NewClient(nexusBase string) *Client {
	if nexusBase == "" {
		nexusBase = "http://127.0.0.1:9000"
	}
	return &Client{
		NexusBase: nexusBase,
		HTTP:      http.Client{Timeout: 1500 * time.Millisecond},
	}
}

// FetchMetrics GETs /api/v1/workspaces/<id>/metrics.
func (c *Client) FetchMetrics(ctx context.Context, workspaceID string) (*Snapshot, error) {
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace id required")
	}
	url := fmt.Sprintf("%s/api/v1/workspaces/%s/metrics", c.NexusBase, workspaceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET %s → HTTP %d: %s", url, resp.StatusCode, truncate(string(body), 160))
	}
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return &snap, nil
}

// WorkspaceSummary is the per-child row from GET /status (Nexus).
type WorkspaceSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Port   int    `json:"port"`
	Status string `json:"status"`
}

// FetchStatus is the lightweight "who's running" probe used for
// workspace resolution when --workspace isn't supplied.
func (c *Client) FetchStatus(ctx context.Context) ([]WorkspaceSummary, error) {
	url := c.NexusBase + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s → HTTP %d", url, resp.StatusCode)
	}
	var items []WorkspaceSummary
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	return items, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
