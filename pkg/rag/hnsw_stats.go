// Package rag — HNSW Graph Stats & Cognitive Drift Monitor. [SRE-35.1]
//
// Design constraints:
//   - Dense computation (stats + drift) runs ONLY during REM cycle to avoid
//     impacting active query latency. [SRE-35 note #1]
//   - Capacity warnings emitted when a workspace approaches max_vectors_per_workspace.
//     [SRE-35 note #2]
//   - Drift ring buffer is O(1) record; O(N) average runs during REM. [SRE-35 note #1]
package rag

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"go.etcd.io/bbolt"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
)

const (
	// DefaultMaxVectorsPerWorkspace is the soft capacity limit per workspace.
	// Override via neo.yaml rag.max_nodes_per_workspace. [SRE-35 Zero-Hardcoding]
	DefaultMaxVectorsPerWorkspace = 50_000

	// DefaultCapacityWarnRatio is used when neo.yaml rag.workspace_capacity_warn_pct is 0.
	DefaultCapacityWarnRatio = float64(0.80)

	// DefaultDriftThreshold is used when neo.yaml rag.drift_threshold is 0.
	DefaultDriftThreshold = float32(0.45)

	// driftRingSize is the window for the rolling average. Not configurable — changing
	// this would require re-allocating the fixed array and has no SRE benefit.
	driftRingSize = 100
)

// GraphStats is a point-in-time snapshot of HNSW graph vitals. [SRE-35.1.1]
// ComputedAt is zero if stats have never been computed (i.e., no REM cycle yet).
type GraphStats struct {
	TotalNodes              int            `json:"total_nodes"`
	TotalEdges              int            `json:"total_edges"`
	AvgEdgesPerNode         float64        `json:"avg_edges_per_node"`
	LayerDistribution       map[int]int    `json:"layer_distribution"`
	VectorsByWorkspace      map[string]int `json:"vectors_by_workspace"`
	MemorySizeBytes         int64          `json:"memory_size_bytes"`
	DiskSizeBytes           int64          `json:"disk_size_bytes"`
	WorkspaceCapacityWarning []string      `json:"workspace_capacity_warnings"` // [SRE-35 note #2]
	ComputedAt              time.Time      `json:"computed_at"`
}

// ComputeGraphStats traverses the in-memory graph and WAL to produce GraphStats.
// MUST be called only during REM cycle — never on the query hot-path. [SRE-35 note #1]
// capacityWarnPct is the fraction [0,1] above which a workspace triggers EventMemoryCapacity.
// Pass 0 to use DefaultCapacityWarnRatio (0.80). Read from neo.yaml rag.workspace_capacity_warn_pct.
func ComputeGraphStats(graph *Graph, wal *WAL, dbPath string, maxPerWorkspace int, capacityWarnPct float64) *GraphStats {
	if graph == nil {
		return &GraphStats{ComputedAt: time.Now()}
	}
	if maxPerWorkspace <= 0 {
		maxPerWorkspace = DefaultMaxVectorsPerWorkspace
	}
	if capacityWarnPct <= 0 {
		capacityWarnPct = DefaultCapacityWarnRatio
	}

	stats := &GraphStats{
		TotalNodes:        len(graph.Nodes),
		TotalEdges:        len(graph.Edges),
		LayerDistribution: make(map[int]int),
		VectorsByWorkspace: make(map[string]int),
		ComputedAt:        time.Now(),
	}

	// Avg edges per node.
	if stats.TotalNodes > 0 {
		stats.AvgEdgesPerNode = float64(stats.TotalEdges) / float64(stats.TotalNodes)
	}

	// Layer distribution.
	for _, n := range graph.Nodes {
		stats.LayerDistribution[int(n.Layer)]++
	}

	// Memory size: vectors (float32 = 4B each) + nodes (16B each) + edges (4B each).
	stats.MemorySizeBytes = int64(len(graph.Vectors))*4 +
		int64(len(graph.Nodes))*16 +
		int64(len(graph.Edges))*4

	// Disk size from bbolt file stat.
	if dbPath != "" {
		if fi, err := os.Stat(dbPath); err == nil {
			stats.DiskSizeBytes = fi.Size()
		}
	}

	// Vectors by workspace — requires WAL scan. O(N) but only during REM.
	if wal != nil {
		docs, err := wal.GetDocsByWorkspace("") // empty = all workspaces
		if err == nil {
			for _, d := range docs {
				wsID := d.WorkspaceID
				if wsID == "" {
					wsID = "legacy"
				}
				stats.VectorsByWorkspace[wsID]++
			}
		}
	}

	// Capacity warnings: emit warning for workspaces at >= capacityWarnPct of max. [SRE-35 note #2]
	for wsID, count := range stats.VectorsByWorkspace {
		if float64(count)/float64(maxPerWorkspace) >= capacityWarnPct {
			stats.WorkspaceCapacityWarning = append(stats.WorkspaceCapacityWarning, wsID)
			log.Printf("[SRE-35] Workspace %q at %.0f%% memory capacity (%d/%d vectors)",
				wsID, float64(count)/float64(maxPerWorkspace)*100, count, maxPerWorkspace)
		}
	}

	return stats
}

// ============================================================================
// DriftMonitor — rolling average of query cosine distances. [SRE-35.1.2]
// ============================================================================

// DriftMonitor tracks the rolling average cosine distance of the last N queries.
// RecordDistance is O(1) and safe to call on the query hot-path.
// ComputeDrift is O(N) and MUST only run during REM cycle. [SRE-35 note #1]
type DriftMonitor struct {
	mu        sync.Mutex
	ring      [driftRingSize]float32
	head      int
	count     int
	bus       *pubsub.Bus
	threshold float32 // configurable via neo.yaml rag.drift_threshold
}

// NewDriftMonitor creates a DriftMonitor that publishes EventCognitiveDrift to bus.
// threshold is the rolling-average distance above which an alert fires (neo.yaml rag.drift_threshold).
// Pass 0 to use Defaultd.threshold (0.45).
func NewDriftMonitor(bus *pubsub.Bus, threshold float32) *DriftMonitor {
	if threshold <= 0 {
		threshold = DefaultDriftThreshold
	}
	return &DriftMonitor{bus: bus, threshold: threshold}
}

// RecordDistance records a query distance into the ring buffer.
// O(1) — safe on hot-path.
func (d *DriftMonitor) RecordDistance(dist float32) {
	d.mu.Lock()
	d.ring[d.head%driftRingSize] = dist
	d.head++
	if d.count < driftRingSize {
		d.count++
	}
	d.mu.Unlock()
}

// ComputeDrift calculates the rolling average and emits EventCognitiveDrift if
// avg > d.threshold. Returns the average distance.
// MUST be called only during REM cycle. [SRE-35 note #1]
func (d *DriftMonitor) ComputeDrift() float32 {
	d.mu.Lock()
	n := d.count
	if n == 0 {
		d.mu.Unlock()
		return 0
	}
	var sum float32
	for i := 0; i < n; i++ {
		sum += d.ring[i]
	}
	d.mu.Unlock()

	avg := sum / float32(n)

	if avg > d.threshold && d.bus != nil {
		d.bus.Publish(pubsub.Event{
			Type: pubsub.EventCognitiveDrift,
			Payload: map[string]any{
				"avg_distance": avg,
				"threshold":    d.threshold,
				"samples":      n,
				"alert":        "Knowledge graph entropy exceeds threshold — consider re-ingestion",
			},
		})
		log.Printf("[SRE-35] EventCognitiveDrift: avg_distance=%.3f > threshold=%.2f (samples=%d)",
			avg, d.threshold, n)
	}

	return avg
}

// DriftStats returns a JSON-serializable snapshot of the monitor state.
func (d *DriftMonitor) DriftStats() map[string]any {
	avg := d.ComputeDrift()
	d.mu.Lock()
	n := d.count
	d.mu.Unlock()
	return map[string]any{
		"avg_distance": avg,
		"threshold":    d.threshold,
		"samples":      n,
		"drifting":     avg > d.threshold,
	}
}

// ============================================================================
// Persistent graph stats cache — stored in BoltDB for trend analysis. [SRE-35 note #3]
// ============================================================================

const bucketNameRAGMetrics = "rag_metrics"

var bucketRAGMetrics = []byte(bucketNameRAGMetrics)

// PersistStats saves the current GraphStats snapshot to BoltDB under key
// "graph_stats_YYYY-MM-DD". Called by the REM cycle for trend analysis. [SRE-35 note #3]
func (wal *WAL) PersistStats(stats *GraphStats) error {
	if wal == nil || wal.db == nil {
		return nil
	}
	return wal.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketRAGMetrics)
		if err != nil {
			return err
		}
		key := []byte("graph_stats_" + stats.ComputedAt.UTC().Format("2006-01-02"))
		data, err := json.Marshal(stats)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

// LoadStatsTrend returns the last `limit` daily GraphStats snapshots from BoltDB.
func (wal *WAL) LoadStatsTrend(limit int) ([]*GraphStats, error) {
	if wal == nil || wal.db == nil {
		return nil, nil
	}
	var results []*GraphStats
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRAGMetrics)
		if b == nil {
			return nil
		}
		// Iterate in reverse (newest first) using cursor.
		c := b.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			if !hasPrefix(k, "graph_stats_") {
				continue
			}
			var s GraphStats
			if err := json.Unmarshal(v, &s); err == nil {
				results = append(results, &s)
				if len(results) >= limit {
					break
				}
			}
		}
		return nil
	})
	return results, err
}

func hasPrefix(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}
