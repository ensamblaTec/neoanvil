package darwin

import (
	"testing"
	"time"
)

func sampleMetrics() []Metric {
	now := time.Now()
	return []Metric{
		{Package: "pkg/foo", Function: "HotPath", File: "pkg/foo/hot.go", Line: 10, CPUSec: 42.5, AllocBytes: 1000, CallCount: 500, Timestamp: now},
		{Package: "pkg/bar", Function: "Slow", File: "pkg/bar/slow.go", Line: 5, CPUSec: 10.0, AllocBytes: 200, CallCount: 100, Timestamp: now},
		{Package: "pkg/foo", Function: "HotPath", File: "pkg/foo/hot.go", Line: 10, CPUSec: 5.5, AllocBytes: 100, CallCount: 50, Timestamp: now.Add(-time.Hour)}, // aggregates
	}
}

// TestSelectHotspot_AggregatesAndRanks [Épica 230.A]
func TestSelectHotspot_AggregatesAndRanks(t *testing.T) {
	hs, ok := SelectHotspot(sampleMetrics())
	if !ok {
		t.Fatal("expected a hotspot, got ok=false")
	}
	if hs.Function != "HotPath" {
		t.Errorf("expected HotPath (48 s aggregated), got %s", hs.Function)
	}
	// 42.5 + 5.5 = 48.0
	if hs.CPUSeconds != 48.0 {
		t.Errorf("expected aggregated CPU 48.0, got %v", hs.CPUSeconds)
	}
	if hs.CallCount != 550 {
		t.Errorf("expected aggregated calls 550, got %d", hs.CallCount)
	}
}

// TestSelectHotspot_EmptyInput [Épica 230.A]
func TestSelectHotspot_EmptyInput(t *testing.T) {
	hs, ok := SelectHotspot(nil)
	if ok {
		t.Errorf("expected ok=false on empty metrics, got hotspot=%+v", hs)
	}
}

// TestSelectHotspot_SingleFunction [Épica 230.A]
func TestSelectHotspot_SingleFunction(t *testing.T) {
	metrics := []Metric{
		{Package: "pkg/x", Function: "F", File: "pkg/x/f.go", Line: 1, CPUSec: 1.0, CallCount: 1},
	}
	hs, ok := SelectHotspot(metrics)
	if !ok || hs.Function != "F" {
		t.Errorf("expected F hotspot, got %+v (ok=%v)", hs, ok)
	}
}
