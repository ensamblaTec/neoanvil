package main

// api_metrics_test.go — Nexus summary scatter tests.
// [PILAR-XXVII/244.D]

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

// newMockChild returns an httptest server plus the port it's bound to.
func newMockChild(t *testing.T, handler http.HandlerFunc) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// httptest URLs look like http://127.0.0.1:PORT — extract the port.
	idx := strings.LastIndex(srv.URL, ":")
	port, err := strconv.Atoi(srv.URL[idx+1:])
	if err != nil {
		t.Fatalf("parse mock port: %v", err)
	}
	return srv, port
}

// TestScatterMetrics_HappyPath — one fast child returns a JSON blob,
// scatter picks it up and reports status=ok with latency.
func TestScatterMetrics_HappyPath(t *testing.T) {
	_, port := newMockChild(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":1,"workspace_id":"ws-ok"}`))
	})
	procs := []nexus.WorkspaceProcess{
		{
			Entry:  workspace.WorkspaceEntry{ID: "ws-ok", Name: "happy"},
			Port:   port,
			Status: nexus.StatusRunning,
		},
	}
	results := scatterMetrics(context.Background(), procs)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	r := results[0]
	if r.Status != "ok" {
		t.Errorf("Status = %q, want ok. err=%q", r.Status, r.Error)
	}
	if !strings.Contains(string(r.Metrics), "ws-ok") {
		t.Errorf("Metrics body missing ws-ok: %s", r.Metrics)
	}
	if r.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want ≥ 0", r.LatencyMs)
	}
}

// TestScatterMetrics_Timeout — slow child (> childMetricsTimeout) is
// flagged status=timeout without poisoning the slice.
func TestScatterMetrics_Timeout(t *testing.T) {
	_, port := newMockChild(t, func(w http.ResponseWriter, _ *http.Request) {
		// Sleep well past the 500 ms budget.
		time.Sleep(childMetricsTimeout + 200*time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	procs := []nexus.WorkspaceProcess{
		{
			Entry:  workspace.WorkspaceEntry{ID: "ws-slow", Name: "slow"},
			Port:   port,
			Status: nexus.StatusRunning,
		},
	}
	start := time.Now()
	results := scatterMetrics(context.Background(), procs)
	elapsed := time.Since(start)
	if elapsed > childMetricsTimeout+300*time.Millisecond {
		t.Errorf("scatter took %v — deadline should cap ~500ms", elapsed)
	}
	if results[0].Status != "timeout" {
		t.Errorf("Status = %q, want timeout", results[0].Status)
	}
}

// TestScatterMetrics_ChildError — child returns HTTP 500, scatter marks
// it status=error.
func TestScatterMetrics_ChildError(t *testing.T) {
	_, port := newMockChild(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	procs := []nexus.WorkspaceProcess{
		{
			Entry:  workspace.WorkspaceEntry{ID: "ws-err", Name: "angry"},
			Port:   port,
			Status: nexus.StatusRunning,
		},
	}
	results := scatterMetrics(context.Background(), procs)
	if results[0].Status != "error" {
		t.Errorf("Status = %q, want error", results[0].Status)
	}
	if !strings.Contains(results[0].Error, "500") {
		t.Errorf("Error = %q, want to mention 500", results[0].Error)
	}
}

// TestScatterMetrics_MixedFleet — one OK, one slow, one stopped → the
// aggregate response still lands within the per-child budget thanks to
// the concurrent fan-out.
func TestScatterMetrics_MixedFleet(t *testing.T) {
	_, okPort := newMockChild(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	_, slowPort := newMockChild(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(childMetricsTimeout + 100*time.Millisecond)
	})
	procs := []nexus.WorkspaceProcess{
		{Entry: workspace.WorkspaceEntry{ID: "ok"}, Port: okPort, Status: nexus.StatusRunning},
		{Entry: workspace.WorkspaceEntry{ID: "slow"}, Port: slowPort, Status: nexus.StatusRunning},
		{Entry: workspace.WorkspaceEntry{ID: "stopped"}, Port: 0, Status: nexus.StatusStopped},
	}
	start := time.Now()
	results := scatterMetrics(context.Background(), procs)
	elapsed := time.Since(start)

	// OK + slow run concurrently, so total elapsed should be close to
	// childMetricsTimeout, not 2×.
	if elapsed > childMetricsTimeout+300*time.Millisecond {
		t.Errorf("elapsed = %v, want ≈ %v (concurrent fan-out)", elapsed, childMetricsTimeout)
	}
	if results[0].Status != "ok" {
		t.Errorf("results[0] status = %q, want ok", results[0].Status)
	}
	if results[1].Status != "timeout" {
		t.Errorf("results[1] status = %q, want timeout", results[1].Status)
	}
	if results[2].Status != "offline" {
		t.Errorf("results[2] status = %q, want offline", results[2].Status)
	}
}

// The HTTP-handler layer (handleMetricsSummary) is intentionally NOT
// unit-tested here — *nexus.ProcessPool has unexported internals so we
// can't seed fake children without touching the package. The three
// scatterMetrics tests above cover the payload logic; the handler itself
// is a thin wrapper that only composes results + writes the envelope.
