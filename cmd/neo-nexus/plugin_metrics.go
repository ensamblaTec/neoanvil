// cmd/neo-nexus/plugin_metrics.go — observability for plugin tool calls.
// ÉPICA 154 / PILAR XXIX. Plugin calls dispatched via callPluginTool run
// inside the Nexus process; observability.GlobalStore lives in neo-mcp's
// process. Without this module, neo_tool_stats reports 0 calls for all
// plugin tools (deepseek_call/red_team_audit, jira_jira/get_context, etc.)
// even when the operator just spent $$$ on them.
//
// Architectural choice: store metrics in a process-wide sync.Map (NOT in
// pluginRuntime field) so they survive SIGHUP reload. Operators want
// longitudinal latency, not a reset on every config change. [DS audit gap 3]
//
// Concurrency: counters are atomic.Int64 (lock-free fast path); the ring
// buffer for latency samples is protected by a per-entry sync.Mutex held
// only during the slot write (~50 ns). The plugin call itself takes
// seconds — mutex contention is impossible in practice. Crucially, the
// metrics path does NOT touch pluginRuntime.mu, so it cannot serialize
// dispatch. [DS audit gap 4]

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// pluginMetricsRingSize bounds the latency sample ring per (plugin, tool).
// 512 is sufficient for current usage (plugin calls are seconds-long, peak
// volume ~10/min); raise via const if a high-throughput plugin appears.
const pluginMetricsRingSize = 512

// pluginMetricEntry tracks call counts and latency samples for one
// (plugin, tool) tuple. Counters are lock-free atomics; the ring buffer
// is mutex-guarded for the brief slot write.
//
// Status separation per DS audit gap 1:
//   - Calls       — successful or errored plugin invocations (CallToolWithMeta returned)
//   - Errors      — subset of Calls where CallToolWithMeta returned an error
//   - Rejections  — denials BEFORE plugin invocation (P-WSACL, P-POLICY)
//   - CacheHits   — idempotency cache short-circuited the call (no plugin I/O)
//
// Cache hits and rejections do NOT enter the latency ring (DS audit gap 2):
// their durations would skew p50/p95 toward unrealistic ~µs values when
// the operator wants to know "how fast does my plugin actually answer".
type pluginMetricEntry struct {
	Plugin string
	Tool   string

	Calls      atomic.Int64
	Errors     atomic.Int64
	Rejections atomic.Int64
	CacheHits  atomic.Int64

	mu       sync.Mutex
	ring     []int64 // durations in nanoseconds; 0 = empty slot
	ringHead int     // next slot to write (mod pluginMetricsRingSize)
}

func newPluginMetricEntry(plugin, tool string) *pluginMetricEntry {
	return &pluginMetricEntry{
		Plugin: plugin,
		Tool:   tool,
		ring:   make([]int64, pluginMetricsRingSize),
	}
}

// pluginMetrics is the process-wide store keyed by "<plugin>_<tool>".
// Survives SIGHUP reload of pluginRuntime — entries persist until the
// process exits or the entry is explicitly evicted.
var pluginMetrics sync.Map

// getOrCreatePluginMetric returns the entry for (plugin, tool), creating
// it lazily on first use. LoadOrStore is atomic so concurrent first-use
// callers all observe the same entry.
func getOrCreatePluginMetric(plugin, tool string) *pluginMetricEntry {
	key := plugin + "_" + tool
	if v, ok := pluginMetrics.Load(key); ok {
		return v.(*pluginMetricEntry)
	}
	entry := newPluginMetricEntry(plugin, tool)
	actual, _ := pluginMetrics.LoadOrStore(key, entry)
	return actual.(*pluginMetricEntry)
}

// recordCall registers a completed plugin invocation. dur is the wall
// time of CallToolWithMeta; isErr is true when the plugin returned an
// error envelope OR the RPC failed. Both ok and err calls go in the
// latency ring — operators want to see latency including failure modes.
func (e *pluginMetricEntry) recordCall(dur time.Duration, isErr bool) {
	e.Calls.Add(1)
	if isErr {
		e.Errors.Add(1)
	}
	d := int64(dur)
	if d <= 0 {
		d = 1 // sentinel: never record 0 (collides with empty-slot marker)
	}
	e.mu.Lock()
	e.ring[e.ringHead] = d
	e.ringHead = (e.ringHead + 1) % pluginMetricsRingSize
	e.mu.Unlock()
}

// recordRejection registers an ACL or policy denial. No latency sample
// because denial paths are sub-microsecond and would skew p50.
func (e *pluginMetricEntry) recordRejection() {
	e.Rejections.Add(1)
}

// recordCacheHit registers an idempotency cache short-circuit. No latency
// sample for the same reason as rejections (cache lookup is ~ns vs plugin
// call ~seconds — operator wants to know plugin-side latency).
func (e *pluginMetricEntry) recordCacheHit() {
	e.CacheHits.Add(1)
}

// pluginMetricSnapshot is the JSON-friendly view of a metric entry.
type pluginMetricSnapshot struct {
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

// snapshot copies the entry's current state into a JSON-safe value.
// Computes p50/p95/p99 from the ring buffer (sorted in-place on a copy
// so concurrent recordCall is unaffected).
func (e *pluginMetricEntry) snapshot() pluginMetricSnapshot {
	e.mu.Lock()
	durs := make([]int64, 0, pluginMetricsRingSize)
	for _, d := range e.ring {
		if d > 0 {
			durs = append(durs, d)
		}
	}
	e.mu.Unlock()
	slices.Sort(durs)
	return pluginMetricSnapshot{
		Plugin:      e.Plugin,
		Tool:        e.Tool,
		Calls:       e.Calls.Load(),
		Errors:      e.Errors.Load(),
		Rejections:  e.Rejections.Load(),
		CacheHits:   e.CacheHits.Load(),
		P50Ns:       percentileNs(durs, 50),
		P95Ns:       percentileNs(durs, 95),
		P99Ns:       percentileNs(durs, 99),
		SampleCount: int64(len(durs)),
	}
}

// percentileNs returns the requested percentile from a sorted ascending
// slice of durations. Returns 0 when the slice is empty.
func percentileNs(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted) * p) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// handlePluginMetrics serves GET /api/v1/plugin_metrics. Returns all
// entries in the global pluginMetrics map sorted by (plugin, tool).
// Read-only — never mutates state. Token gate respected by the API mux
// auth middleware (same pattern as /api/v1/plugins).
func handlePluginMetrics(w http.ResponseWriter, _ *http.Request) {
	var snapshots []pluginMetricSnapshot
	pluginMetrics.Range(func(_, v any) bool {
		e := v.(*pluginMetricEntry)
		snapshots = append(snapshots, e.snapshot())
		return true
	})
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Plugin != snapshots[j].Plugin {
			return snapshots[i].Plugin < snapshots[j].Plugin
		}
		return snapshots[i].Tool < snapshots[j].Tool
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plugins":           snapshots,
		"generated_at_unix": time.Now().Unix(),
	})
}
