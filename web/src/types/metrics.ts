// [PILAR-XXVII/245.A] Mirror of pkg/observability.Snapshot — JSON is the
// contract boundary. Keep field names in sync with pkg/observability/
// snapshot.go. Manual rather than codegen because the schema is stable.

export interface Snapshot {
  schema_version: number;
  workspace_id: string;
  workspace_name: string;
  uptime_seconds: number;
  generated_at: string;
  memory: MemorySection;
  tools: ToolsSection;
  tokens: TokensSection;
  mutations: MutationsSection;
  recent_events: EventEntry[];
}

export interface MemorySection {
  heap_mb: number;
  stack_mb: number;
  goroutines: number;
  gc_runs: number;
  gc_pause_last_ms: number;
  cpg_heap_mb: number;
  cpg_heap_limit_mb: number;
  cpg_heap_pct: number;
  num_cpu: number;
  query_cache_hit_rate: number;
  text_cache_hit_rate: number;
  emb_cache_hit_rate: number;
}

export interface ToolsSection {
  top_by_calls: ToolStats[];
  top_by_errors: ToolStats[];
  top_by_p99: ToolStats[];
  total_calls_24h: number;
}

export interface ToolStats {
  name: string;
  calls: number;
  errors: number;
  error_rate: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  last_call_at: string;
}

export interface TokensSection {
  today_input_tokens: number;
  today_output_tokens: number;
  today_cost_usd: number;
  mcp_traffic: TokenBreakdown;
  internal_inference: TokenBreakdown;
  last_7_days: TokenDaySummary[];
}

export interface TokenBreakdown {
  input_tokens: number;
  output_tokens: number;
  cost_usd: number;
  by_agent: Record<string, number>;
  by_tool: Record<string, number>;
  by_prompt_type?: Record<string, number>;
}

export interface TokenDaySummary {
  day: string;
  mcp_input: number;
  mcp_output: number;
  internal_input: number;
  internal_output: number;
  cost_usd: number;
}

export interface MutationsSection {
  certified_24h: number;
  bypassed_24h: number;
  top_hotspots: HotspotEntry[];
}

export interface HotspotEntry {
  path: string;
  count: number;
}

export interface EventEntry {
  ts: string;
  type: string;
  severity?: string;
  payload?: Record<string, unknown>;
}

// /status payload from Nexus — thin status list.
export interface WorkspaceStatus {
  id: string;
  name: string;
  path: string;
  port: number;
  status: 'running' | 'starting' | 'error' | 'stopped' | 'unhealthy' | 'quarantined' | string;
  pid?: number;
  uptime_seconds?: number;
  last_ping_ago_seconds?: number;
  restarts?: number;
  // [Épica 248.C] Activity counters from Nexus proxy.
  last_tool_call_unix?: number; // 0 or absent = never used
  tool_call_count?: number;
  idle_seconds?: number;        // 0 if never used; computed server-side
  // [284.A] Project federation fields.
  project_id?: string;
  project_name?: string;
}

// /api/v1/metrics/summary envelope.
export interface SummaryResponse {
  schema_version: number;
  generated_at: string;
  total_workspaces: number;
  active: number;
  degraded: number;
  elapsed_ms: number;
  workspaces: SummaryChild[];
}

export interface SummaryChild {
  workspace_id: string;
  workspace_name: string;
  port: number;
  status: 'ok' | 'timeout' | 'error' | 'offline' | string;
  error?: string;
  metrics?: Snapshot;
  latency_ms: number;
}
