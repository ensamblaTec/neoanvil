package main

// [SRE-90.A.2/B.2/B.3] Nexus-side risk ranking and preventive throttling.
//
// Consumes Oracle predictions from children (via /api/v1/oracle/risk polling)
// and maintains a risk ranking map. When a workspace crosses the danger
// threshold, Nexus throttles other workspaces to prioritize the critical one.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// RiskRanking tracks per-workspace failure probability. Thread-safe. [SRE-90.A.2]
type RiskRanking struct {
	mu      sync.RWMutex
	ranking map[string]sre.Prediction // key = workspace ID
}

// NewRiskRanking creates an empty risk ranking.
func NewRiskRanking() *RiskRanking {
	return &RiskRanking{ranking: make(map[string]sre.Prediction)}
}

// Update records a prediction for a workspace.
func (r *RiskRanking) Update(pred sre.Prediction) {
	r.mu.Lock()
	r.ranking[pred.WorkspaceID] = pred
	r.mu.Unlock()
}

// MostAtRisk returns the workspace with the highest FailProb24h, or nil.
func (r *RiskRanking) MostAtRisk() *sre.Prediction {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best *sre.Prediction
	for _, pred := range r.ranking {
		if best == nil || pred.FailProb24h > best.FailProb24h {
			copy := pred
			best = &copy
		}
	}
	return best
}

// All returns a snapshot of all predictions.
func (r *RiskRanking) All() map[string]sre.Prediction {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]sre.Prediction, len(r.ranking))
	maps.Copy(out, r.ranking)
	return out
}

// PollChildOracles periodically fetches /api/v1/oracle/risk from each running
// child and updates the risk ranking. [SRE-90.A.2]
func PollChildOracles(ctx context.Context, pool *nexus.ProcessPool, ranking *RiskRanking, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// [SRE-103.A] Oracle polling targets children on loopback — use the
	// internal-only guard to drop the fragile trusted_local_ports dependency.
	client := sre.SafeInternalHTTPClient(5)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range pool.List() {
				if p.Status != nexus.StatusRunning {
					continue
				}
				go func(proc nexus.WorkspaceProcess) {
					url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/oracle/risk", proc.Port)
					resp, err := client.Get(url) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT
					if err != nil {
						return
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						return
					}
					var risk struct {
						FailProb24h    float64 `json:"fail_prob_24h"`
						DominantSignal string  `json:"dominant_signal"`
						HeapTrend      float64 `json:"heap_trend_mb_per_min"`
						PowerTrend     float64 `json:"power_trend_w_per_min"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&risk); err != nil {
						return
					}
					ranking.Update(sre.Prediction{
						WorkspaceID:    proc.Entry.ID,
						FailProb24h:    risk.FailProb24h,
						DominantSignal: risk.DominantSignal,
						HeapTrend:      risk.HeapTrend,
						PowerTrend:     risk.PowerTrend,
						At:             time.Now(),
					})
				}(p)
			}
		}
	}
}

// PreventiveThrottle checks the risk ranking and applies CPU throttling
// to non-critical workspaces when one workspace is at high risk. [SRE-90.B.2]
// Returns a description of the rebalance action taken, or "" if none.
func PreventiveThrottle(pool *nexus.ProcessPool, ranking *RiskRanking, threshold float64) string {
	critical := ranking.MostAtRisk()
	if critical == nil || critical.FailProb24h < threshold {
		return ""
	}

	var throttled []string
	for _, p := range pool.List() {
		if p.Status != nexus.StatusRunning || p.Entry.ID == critical.WorkspaceID {
			continue
		}
		if p.PID > 0 {
			if err := sre.SetCPULimit(p.PID, 50); err != nil {
				log.Printf("[NEXUS-WARN] throttle pid=%d failed: %v", p.PID, err)
				continue
			}
			throttled = append(throttled, p.Entry.ID)
		}
	}

	if len(throttled) == 0 {
		return ""
	}

	msg := fmt.Sprintf("[NEXUS-EVENT] resource_rebalanced prioritized=%s throttled=%v reason=FailProb24h=%.2f_%s",
		critical.WorkspaceID, throttled, critical.FailProb24h, critical.DominantSignal)
	log.Print(msg)
	return msg
}
