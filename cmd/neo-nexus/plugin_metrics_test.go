package main

import (
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

// resetPluginMetrics wipes the global store so tests don't bleed into
// each other. Cannot use t.Cleanup directly on a sync.Map without iter,
// so we iterate and Delete. Safe in tests because they're sequential.
func resetPluginMetrics(t *testing.T) {
	t.Helper()
	pluginMetrics.Range(func(k, _ any) bool {
		pluginMetrics.Delete(k)
		return true
	})
}

// TestPluginMetric_RecordCall_OkAndError covers the basic success path
// and error path. Both go in the latency ring; only error increments
// the Errors counter. [154.E case 4]
func TestPluginMetric_RecordCall_OkAndError(t *testing.T) {
	resetPluginMetrics(t)
	m := getOrCreatePluginMetric("ds", "audit")
	m.recordCall(50*time.Millisecond, false)
	m.recordCall(100*time.Millisecond, true)
	if got := m.Calls.Load(); got != 2 {
		t.Errorf("Calls=%d want 2", got)
	}
	if got := m.Errors.Load(); got != 1 {
		t.Errorf("Errors=%d want 1", got)
	}
	if got := m.Rejections.Load(); got != 0 {
		t.Errorf("Rejections=%d want 0 (record call should not bump rejections)", got)
	}
	snap := m.snapshot()
	if snap.SampleCount != 2 {
		t.Errorf("SampleCount=%d want 2 (both ok and err in ring)", snap.SampleCount)
	}
	if snap.P50Ns < int64(50*time.Millisecond) {
		t.Errorf("P50Ns=%d want >= 50ms", snap.P50Ns)
	}
}

// TestPluginMetric_Rejection covers ACL/policy denial. Rejection MUST NOT
// touch the latency ring (DS audit gap 2 — sub-µs denial paths would skew
// p50). It MUST NOT bump Calls (different counter). [154.E case 2]
func TestPluginMetric_Rejection(t *testing.T) {
	resetPluginMetrics(t)
	m := getOrCreatePluginMetric("ds", "audit")
	m.recordRejection()
	m.recordRejection()
	if got := m.Calls.Load(); got != 0 {
		t.Errorf("Calls=%d want 0 (rejection != call)", got)
	}
	if got := m.Rejections.Load(); got != 2 {
		t.Errorf("Rejections=%d want 2", got)
	}
	if snap := m.snapshot(); snap.SampleCount != 0 {
		t.Errorf("SampleCount=%d want 0 (rejection should not enter latency ring)", snap.SampleCount)
	}
}

// TestPluginMetric_CacheHit covers idempotency cache short-circuit.
// Same invariants as rejection — separate counter, no latency sample.
// [154.E case 3]
func TestPluginMetric_CacheHit(t *testing.T) {
	resetPluginMetrics(t)
	m := getOrCreatePluginMetric("ds", "audit")
	m.recordCacheHit()
	if got := m.Calls.Load(); got != 0 {
		t.Errorf("Calls=%d want 0 (cache hit != real call)", got)
	}
	if got := m.CacheHits.Load(); got != 1 {
		t.Errorf("CacheHits=%d want 1", got)
	}
	if snap := m.snapshot(); snap.SampleCount != 0 {
		t.Errorf("SampleCount=%d want 0 (cache hit should not enter ring)", snap.SampleCount)
	}
}

// TestPluginMetric_Concurrency stresses the lock-free counter path with
// 100 goroutines × 50 calls each. atomic.Int64.Add must converge to the
// exact total; no race detector warnings. [154.E case 1]
func TestPluginMetric_Concurrency(t *testing.T) {
	resetPluginMetrics(t)
	m := getOrCreatePluginMetric("ds", "audit")
	const goroutines = 100
	const callsEach = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range callsEach {
				m.recordCall(time.Millisecond, false)
			}
		}()
	}
	wg.Wait()
	want := int64(goroutines * callsEach)
	if got := m.Calls.Load(); got != want {
		t.Errorf("Calls=%d want %d (atomic counter lost updates)", got, want)
	}
}

// TestPluginMetric_GetOrCreate_Idempotent verifies sync.Map.LoadOrStore
// returns the same entry across concurrent first-use calls — required so
// counters don't fragment across observers.
func TestPluginMetric_GetOrCreate_Idempotent(t *testing.T) {
	resetPluginMetrics(t)
	const goroutines = 50
	got := make([]*pluginMetricEntry, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			got[idx] = getOrCreatePluginMetric("ds", "audit")
		}(i)
	}
	wg.Wait()
	first := got[0]
	for i, e := range got {
		if e != first {
			t.Errorf("goroutine %d got different entry pointer (sync.Map fragmentation)", i)
		}
	}
}

// TestPluginMetric_Snapshot_PercentileOrder verifies p50 ≤ p95 ≤ p99
// after sorting. Empty ring returns 0 for all.
func TestPluginMetric_Snapshot_PercentileOrder(t *testing.T) {
	resetPluginMetrics(t)
	m := getOrCreatePluginMetric("ds", "audit")
	// Empty ring.
	snap := m.snapshot()
	if snap.P50Ns != 0 || snap.P95Ns != 0 || snap.P99Ns != 0 {
		t.Errorf("empty ring percentiles=%d/%d/%d want all 0", snap.P50Ns, snap.P95Ns, snap.P99Ns)
	}
	// Insert 100 calls 1ms..100ms in mixed order.
	for _, n := range []int{50, 1, 100, 25, 75, 10, 90, 5, 60, 35} {
		m.recordCall(time.Duration(n)*time.Millisecond, false)
	}
	snap = m.snapshot()
	if !(snap.P50Ns <= snap.P95Ns && snap.P95Ns <= snap.P99Ns) {
		t.Errorf("percentiles not monotonic: p50=%d p95=%d p99=%d", snap.P50Ns, snap.P95Ns, snap.P99Ns)
	}
}

// TestHandlePluginMetrics_HTTP verifies the JSON shape of the endpoint.
func TestHandlePluginMetrics_HTTP(t *testing.T) {
	resetPluginMetrics(t)
	a := getOrCreatePluginMetric("ds", "audit")
	a.recordCall(10*time.Millisecond, false)
	b := getOrCreatePluginMetric("jira", "ticket")
	b.recordRejection()

	req := httptest.NewRequest("GET", "/api/v1/plugin_metrics", nil)
	w := httptest.NewRecorder()
	handlePluginMetrics(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d want 200", w.Code)
	}
	var resp struct {
		Plugins         []pluginMetricSnapshot `json:"plugins"`
		GeneratedAtUnix int64                  `json:"generated_at_unix"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(resp.Plugins) != 2 {
		t.Fatalf("len(plugins)=%d want 2", len(resp.Plugins))
	}
	if resp.GeneratedAtUnix == 0 {
		t.Errorf("GeneratedAtUnix is 0 — expected current Unix timestamp")
	}
	// Sorted by (plugin, tool): ds,audit comes before jira,ticket.
	if resp.Plugins[0].Plugin != "ds" || resp.Plugins[1].Plugin != "jira" {
		t.Errorf("entries not sorted by plugin: %+v", resp.Plugins)
	}
	if resp.Plugins[0].Calls != 1 || resp.Plugins[0].Rejections != 0 {
		t.Errorf("ds entry counters wrong: %+v", resp.Plugins[0])
	}
	if resp.Plugins[1].Rejections != 1 || resp.Plugins[1].Calls != 0 {
		t.Errorf("jira entry counters wrong: %+v", resp.Plugins[1])
	}
}

// TestPluginMetric_DurationFloor verifies record of a sub-microsecond
// duration still gets stored as 1ns (avoid colliding with empty-slot
// marker 0).
func TestPluginMetric_DurationFloor(t *testing.T) {
	resetPluginMetrics(t)
	m := getOrCreatePluginMetric("ds", "tiny")
	m.recordCall(0, false) // pathological — should still register
	if got := m.Calls.Load(); got != 1 {
		t.Errorf("Calls=%d want 1", got)
	}
	if snap := m.snapshot(); snap.SampleCount != 1 {
		t.Errorf("SampleCount=%d want 1 (zero-dur should still log a sample)", snap.SampleCount)
	}
}

// TestDetectPluginToolCall_EmptyLocalNameSkipped covers 154.C — when
// request name equals "<prefix>_" exactly, CutPrefix returns ("", true).
// The guard must skip dispatching with an empty tool name (would give
// the plugin undefined behavior). [F3 from DS audit round 2]
func TestDetectPluginToolCall_EmptyLocalNameSkipped(t *testing.T) {
	rt := makeRuntime(nil, []plugin.Connected{
		{Name: "deepseek", NamespacePrefix: "deepseek"},
	})
	// name == prefix + "_" exactly → CutPrefix("deepseek_", "deepseek_") = ("", true)
	body := []byte(`{"method":"tools/call","params":{"name":"deepseek_"}}`)
	conn, local := detectPluginToolCall(body, rt)
	if conn != nil {
		t.Errorf("empty localName must not dispatch; got plugin=%s local=%q", conn.Name, local)
	}
	if local != "" {
		t.Errorf("expected empty local on no-match, got %q", local)
	}
}
