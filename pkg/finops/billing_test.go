package finops

import (
	"sync/atomic"
	"testing"
)

func resetState() {
	CurrentTCO = CloudCost{}
	atomic.StoreInt64(&GlobalThermalTicks, 0)
}

// TestIngestCostMetrics_Valid [Épica 231.H]
func TestIngestCostMetrics_Valid(t *testing.T) {
	resetState()
	total := IngestCostMetrics([]byte(`{"cpu_cost": 1.5, "ram_cost": 0.75}`))
	if total != 2.25 {
		t.Errorf("expected sum 2.25, got %v", total)
	}
	if CurrentTCO.CPUCost != 1.5 || CurrentTCO.RAMCost != 0.75 {
		t.Errorf("globals not set: %+v", CurrentTCO)
	}
}

// TestIngestCostMetrics_InvalidJSON [Épica 231.H]
func TestIngestCostMetrics_InvalidJSON(t *testing.T) {
	resetState()
	total := IngestCostMetrics([]byte("not json"))
	if total != 0.0 {
		t.Errorf("expected 0 penalty on bad JSON, got %v", total)
	}
}

// TestIngestCostMetrics_EmptyBody [Épica 231.H]
func TestIngestCostMetrics_EmptyBody(t *testing.T) {
	resetState()
	total := IngestCostMetrics([]byte(`{}`))
	if total != 0.0 {
		t.Errorf("expected 0 on empty object, got %v", total)
	}
}

// TestGetTotalPenalty [Épica 231.H]
func TestGetTotalPenalty(t *testing.T) {
	resetState()
	CurrentTCO = CloudCost{CPUCost: 2.0, RAMCost: 3.5}
	if p := GetTotalPenalty(); p != 5.5 {
		t.Errorf("expected 5.5, got %v", p)
	}
}

// TestIngestHardwareMetric_Atomic [Épica 231.H]
func TestIngestHardwareMetric_Atomic(t *testing.T) {
	resetState()
	for range 100 {
		IngestHardwareMetric(1000)
	}
	if got := atomic.LoadInt64(&GlobalThermalTicks); got != 100000 {
		t.Errorf("expected 100000 ns, got %d", got)
	}
}

// TestIngestHardwareMetric_Concurrent [Épica 231.H]
func TestIngestHardwareMetric_Concurrent(t *testing.T) {
	resetState()
	done := make(chan struct{})
	for range 10 {
		go func() {
			for range 100 {
				IngestHardwareMetric(10)
			}
			done <- struct{}{}
		}()
	}
	for range 10 {
		<-done
	}
	if got := atomic.LoadInt64(&GlobalThermalTicks); got != 10000 {
		t.Errorf("expected 10000 ns from 10×100×10, got %d", got)
	}
}
