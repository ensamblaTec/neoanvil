package main

// api_metrics.go — Nexus aggregator for the per-workspace observability
// snapshots. Two endpoints:
//
//   GET /api/v1/workspaces/<id>/metrics  → proxy to that child.
//   GET /api/v1/metrics/summary          → scatter-gather across running
//                                            children, 500 ms budget each.
//
// [PILAR-XXVII/244.C]

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

const (
	// Per-child timeout for the summary scatter. Aggressive so a slow
	// child can't block the aggregate response.
	childMetricsTimeout = 500 * time.Millisecond
)

// Shared HTTP clients — one Transport per call site would leak one
// connection pool per request. A single Client per semantic use
// amortises keep-alive and caps idle connections. [PILAR-XXVIII hotfix]
var (
	// metricsProxyClient: sync GET /api/v1/metrics via /api/v1/workspaces/<id>/metrics.
	// 5-second timeout matches the previous SafeInternalHTTPClient(5).
	metricsProxyClient = sre.SafeInternalHTTPClient(5)

	// summaryScatterClient: scatter-gather across children, each
	// bounded by childMetricsTimeout via context. Transport-level
	// timeout is generous (1s) — the context is the real cap.
	summaryScatterClient = sre.SafeInternalHTTPClient(1)
)

// childMetricsResult is the outcome of one child query during the
// scatter. Status is "ok" | "timeout" | "error" — surfaces to the HUD
// so operators see which workspace is degraded without reloading.
type childMetricsResult struct {
	WorkspaceID   string          `json:"workspace_id"`
	WorkspaceName string          `json:"workspace_name"`
	Port          int             `json:"port"`
	Status        string          `json:"status"`
	Error         string          `json:"error,omitempty"`
	Metrics       json.RawMessage `json:"metrics,omitempty"`
	LatencyMs     int64           `json:"latency_ms"`
}

// summaryResponse is the top-level shape for /api/v1/metrics/summary.
// SchemaVersion tracks breaking changes; consumers should branch on it.
type summaryResponse struct {
	SchemaVersion    int                  `json:"schema_version"`
	GeneratedAt      time.Time            `json:"generated_at"`
	TotalWorkspaces  int                  `json:"total_workspaces"`
	Active           int                  `json:"active"`
	Degraded         int                  `json:"degraded"`
	ElapsedMs        int64                `json:"elapsed_ms"`
	Workspaces       []childMetricsResult `json:"workspaces"`
}

const summarySchemaVersion = 1

// handleWorkspaceMetrics handles GET /api/v1/workspaces/<id>/metrics by
// proxying to the child's /api/v1/metrics endpoint. Returns 404 when
// the workspace is unknown, 503 when it's not running. [244.B]
func handleWorkspaceMetrics(pool *nexus.ProcessPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		const prefix = "/api/v1/workspaces/"
		const suffix = "/metrics"
		path := r.URL.Path
		if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
		if id == "" {
			http.NotFound(w, r)
			return
		}
		proc := findProcByID(pool, id)
		if proc == nil {
			http.Error(w, fmt.Sprintf(`{"error":"unknown workspace %q"}`, id), http.StatusNotFound)
			return
		}
		if proc.Status != nexus.StatusRunning {
			http.Error(w, fmt.Sprintf(`{"error":"workspace %q not running (status=%s)"}`, id, proc.Status), http.StatusServiceUnavailable)
			return
		}
		url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/metrics", proc.Port)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"build request: %v"}`, err), http.StatusInternalServerError)
			return
		}
		resp, err := metricsProxyClient.Do(req)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"child request: %v"}`, err), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=2")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// handleMetricsSummary handles GET /api/v1/metrics/summary — scatter-gather
// across all running children with per-child timeout. Never fails: slow /
// broken children come back as {status:"timeout"} or {status:"error"}
// without poisoning the aggregate response. [244.C]
func handleMetricsSummary(pool *nexus.ProcessPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		start := time.Now()
		procs := pool.List()
		resp := summaryResponse{
			SchemaVersion:   summarySchemaVersion,
			GeneratedAt:     start.UTC(),
			TotalWorkspaces: len(procs),
			Workspaces:      make([]childMetricsResult, 0, len(procs)),
		}

		results := scatterMetrics(r.Context(), procs)
		for _, res := range results {
			resp.Workspaces = append(resp.Workspaces, res)
			if res.Status == "ok" {
				resp.Active++
			} else {
				resp.Degraded++
			}
		}
		resp.ElapsedMs = time.Since(start).Milliseconds()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=2")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// scatterMetrics runs the per-child request concurrently, each bounded
// by childMetricsTimeout, and returns results in the same order as procs.
func scatterMetrics(parentCtx context.Context, procs []nexus.WorkspaceProcess) []childMetricsResult {
	results := make([]childMetricsResult, len(procs))
	var wg sync.WaitGroup
	client := summaryScatterClient // shared — Transport reused across requests
	for i := range procs {
		p := procs[i]
		results[i] = childMetricsResult{
			WorkspaceID:   p.Entry.ID,
			WorkspaceName: p.Entry.Name,
			Port:          p.Port,
			Status:        string(p.Status),
		}
		if p.Status != nexus.StatusRunning {
			// Not running — record as-is without a network call.
			results[i].Status = "offline"
			continue
		}
		wg.Add(1)
		go func(idx int, proc nexus.WorkspaceProcess) {
			defer wg.Done()
			callStart := time.Now()
			ctx, cancel := context.WithTimeout(parentCtx, childMetricsTimeout)
			defer cancel()
			url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/metrics", proc.Port)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				results[idx].Status = "error"
				results[idx].Error = err.Error()
				results[idx].LatencyMs = time.Since(callStart).Milliseconds()
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				results[idx].LatencyMs = time.Since(callStart).Milliseconds()
				if ctx.Err() == context.DeadlineExceeded {
					results[idx].Status = "timeout"
				} else {
					results[idx].Status = "error"
				}
				results[idx].Error = err.Error()
				return
			}
			defer func() { _ = resp.Body.Close() }()
			body, readErr := io.ReadAll(resp.Body)
			results[idx].LatencyMs = time.Since(callStart).Milliseconds()
			if readErr != nil {
				results[idx].Status = "error"
				results[idx].Error = readErr.Error()
				return
			}
			if resp.StatusCode != http.StatusOK {
				results[idx].Status = "error"
				results[idx].Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
				return
			}
			results[idx].Status = "ok"
			results[idx].Metrics = body
		}(i, p)
	}
	wg.Wait()
	return results
}

// findProcByID looks up a workspace process by exact ID. Returns nil
// when unknown.
func findProcByID(pool *nexus.ProcessPool, id string) *nexus.WorkspaceProcess {
	for _, p := range pool.List() {
		if p.Entry.ID == id {
			proc := p
			return &proc
		}
	}
	return nil
}
