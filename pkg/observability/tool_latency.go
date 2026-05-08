// Package observability — per-tool MCP call latency histogram. [PILAR-XXV/178]
//
// Tracks the last N latency samples per tool using a circular ring buffer,
// then computes p50/p95/p99 on demand via a sort-copy. Memory is bounded
// (sizeof(time.Duration) × ringCap × tools) ≈ 8 × 512 × 15 = 60 KB for a
// neoanvil deployment — negligible.
//
// Lock discipline: one sync.RWMutex per tracker instance. Record() takes the
// write lock briefly to append; Percentiles() takes the read lock, copies
// the ring into a local slice, releases the lock, then sorts the copy. So
// concurrent tool calls never block reads, and reads never stall writes.

package observability

import (
	"sort"
	"sync"
	"time"
)

// ToolLatencyTracker is the package-level singleton accessed by the MCP
// handler. Created once in main.go, wired into mcpHandler via the
// observability package the handler already imports.
type ToolLatencyTracker struct {
	mu      sync.RWMutex
	ringCap int
	calls   map[string]*latencyRing
}

//go:align 64 // [366.A] per-tool ring is stored in a map value; alignment ensures that when
// the runtime places two rings adjacently their hot fields (pos, total) don't share a cache line.
type latencyRing struct {
	samples  []time.Duration
	pos      int  // next write index
	filled   bool // true once we've wrapped around
	total    int  // total Record() calls this tool has seen
	errCount int  // total errored Record() calls [188]
}

// GlobalToolLatency is the package-level singleton read by HUD_STATE /
// BRIEFING without needing an injected dependency. Main sets this when it
// creates its own tracker; handlers read without coupling to main.
var GlobalToolLatency *ToolLatencyTracker

// NewToolLatencyTracker returns a tracker that retains the last `ringCap`
// samples per tool. A reasonable default is 512 — enough for stable p99.
// Also sets GlobalToolLatency if the global is nil so early main startup
// does not lose the first samples.
func NewToolLatencyTracker(ringCap int) *ToolLatencyTracker {
	if ringCap <= 0 {
		ringCap = 512
	}
	t := &ToolLatencyTracker{
		ringCap: ringCap,
		calls:   make(map[string]*latencyRing, 32),
	}
	if GlobalToolLatency == nil {
		GlobalToolLatency = t
	}
	return t
}

// Record appends a single latency sample for the named tool. Thread-safe.
// Kept for backwards compatibility — new call sites should prefer RecordErr
// which also tallies failures for the error-rate metric.
func (t *ToolLatencyTracker) Record(tool string, dur time.Duration) {
	t.RecordErr(tool, dur, false)
}

// RecordErr is the full-fidelity recorder. Set errored=true when the
// tool call returned a non-nil error; the ring still stores the duration
// (failures are often the slow tail we care about).
func (t *ToolLatencyTracker) RecordErr(tool string, dur time.Duration, errored bool) {
	if t == nil || tool == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.calls[tool]
	if !ok {
		r = &latencyRing{samples: make([]time.Duration, t.ringCap)}
		t.calls[tool] = r
	}
	r.samples[r.pos] = dur
	r.pos = (r.pos + 1) % t.ringCap
	if r.pos == 0 {
		r.filled = true
	}
	r.total++
	if errored {
		r.errCount++
	}
}

// ErrorRate returns err_count / total for a tool as a fraction in [0, 1].
// Returns 0 for tools that never ran.
func (t *ToolLatencyTracker) ErrorRate(tool string) float64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.calls[tool]
	if !ok || r.total == 0 {
		return 0
	}
	return float64(r.errCount) / float64(r.total)
}

// ErrorCount returns the lifetime error counter for a tool. Returns 0 for
// tools that never errored or never ran.
func (t *ToolLatencyTracker) ErrorCount(tool string) int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if r, ok := t.calls[tool]; ok {
		return r.errCount
	}
	return 0
}

// Percentiles returns p50/p95/p99 (in nanoseconds) and the total number of
// samples observed for this tool. `count` is the ring window size, not the
// total — for that, use TotalCalls(). Returns all zeros when the tool has
// not been called yet.
func (t *ToolLatencyTracker) Percentiles(tool string) (p50, p95, p99 time.Duration, count int) {
	if t == nil {
		return 0, 0, 0, 0
	}
	t.mu.RLock()
	r, ok := t.calls[tool]
	if !ok {
		t.mu.RUnlock()
		return 0, 0, 0, 0
	}
	// Copy to local slice to minimize lock hold time.
	n := t.ringCap
	if !r.filled {
		n = r.pos
	}
	cp := make([]time.Duration, n)
	if !r.filled {
		copy(cp, r.samples[:n])
	} else {
		copy(cp, r.samples)
	}
	t.mu.RUnlock()
	if n == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return pct(cp, 0.50), pct(cp, 0.95), pct(cp, 0.99), n
}

// TotalCalls returns the lifetime Record() count for a tool — not bounded
// by ring capacity. Useful for BRIEFING to show call volume alongside p99.
func (t *ToolLatencyTracker) TotalCalls(tool string) int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if r, ok := t.calls[tool]; ok {
		return r.total
	}
	return 0
}

// Tools returns the set of tools that have been Record()ed at least once.
// Sorted alphabetically for deterministic BRIEFING output.
func (t *ToolLatencyTracker) Tools() []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	out := make([]string, 0, len(t.calls))
	for name := range t.calls {
		out = append(out, name)
	}
	t.mu.RUnlock()
	sort.Strings(out)
	return out
}

// pct returns the sample at percentile q (0.0–1.0) from a pre-sorted slice.
// Uses nearest-rank (rounds down) — standard for monitoring use cases.
func pct(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * q)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
