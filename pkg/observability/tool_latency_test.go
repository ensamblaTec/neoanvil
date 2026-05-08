package observability

import (
	"sync"
	"testing"
	"time"
)

func TestToolLatency_ZeroWhenEmpty(t *testing.T) {
	tr := NewToolLatencyTracker(16)
	p50, p95, p99, n := tr.Percentiles("nonexistent")
	if p50 != 0 || p95 != 0 || p99 != 0 || n != 0 {
		t.Errorf("empty tracker must return zeros, got p50=%v p95=%v p99=%v n=%d",
			p50, p95, p99, n)
	}
}

func TestToolLatency_BasicPercentiles(t *testing.T) {
	tr := NewToolLatencyTracker(100)
	// Record 100 samples from 1ms to 100ms.
	for i := 1; i <= 100; i++ {
		tr.Record("neo_radar", time.Duration(i)*time.Millisecond)
	}
	p50, p95, p99, n := tr.Percentiles("neo_radar")
	if n != 100 {
		t.Errorf("expected 100 samples, got %d", n)
	}
	// Nearest-rank: p50 → index 49 → 50ms ± 1 (sort is deterministic).
	if p50 < 49*time.Millisecond || p50 > 51*time.Millisecond {
		t.Errorf("p50 should be ~50ms, got %v", p50)
	}
	if p95 < 94*time.Millisecond || p95 > 96*time.Millisecond {
		t.Errorf("p95 should be ~95ms, got %v", p95)
	}
	if p99 < 98*time.Millisecond || p99 > 100*time.Millisecond {
		t.Errorf("p99 should be ~99ms, got %v", p99)
	}
}

func TestToolLatency_RingWraps(t *testing.T) {
	tr := NewToolLatencyTracker(10)
	// Fill past capacity — the last 10 samples must dominate.
	for i := 1; i <= 50; i++ {
		tr.Record("tool", time.Duration(i)*time.Millisecond)
	}
	_, _, p99, n := tr.Percentiles("tool")
	if n != 10 {
		t.Errorf("ring must cap at 10, got %d", n)
	}
	// The retained window is samples 41..50 — p99 ≈ 50ms.
	if p99 < 49*time.Millisecond || p99 > 50*time.Millisecond {
		t.Errorf("p99 after wrap should track latest window, got %v", p99)
	}
	if total := tr.TotalCalls("tool"); total != 50 {
		t.Errorf("TotalCalls should not be capped by ring, got %d", total)
	}
}

func TestToolLatency_ToolsSorted(t *testing.T) {
	tr := NewToolLatencyTracker(4)
	tr.Record("zeta", time.Millisecond)
	tr.Record("alpha", time.Millisecond)
	tr.Record("mu", time.Millisecond)
	tools := tr.Tools()
	if len(tools) != 3 || tools[0] != "alpha" || tools[1] != "mu" || tools[2] != "zeta" {
		t.Errorf("Tools() must be sorted, got %v", tools)
	}
}

func TestToolLatency_ConcurrentRecord(t *testing.T) {
	// Race-safety: 8 goroutines hammering Record while another reads.
	tr := NewToolLatencyTracker(64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tr.Record(name, time.Duration(j)*time.Microsecond)
			}
		}("neo_radar")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 500; j++ {
			_, _, _, _ = tr.Percentiles("neo_radar")
		}
	}()
	wg.Wait()
	if tr.TotalCalls("neo_radar") != 1600 {
		t.Errorf("expected 1600 total, got %d", tr.TotalCalls("neo_radar"))
	}
}

func BenchmarkToolLatency_Record(b *testing.B) {
	tr := NewToolLatencyTracker(512)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Record("neo_radar", 100*time.Microsecond)
	}
}

func BenchmarkToolLatency_Percentiles(b *testing.B) {
	tr := NewToolLatencyTracker(512)
	for i := 0; i < 512; i++ {
		tr.Record("neo_radar", time.Duration(i)*time.Microsecond)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = tr.Percentiles("neo_radar")
	}
}
