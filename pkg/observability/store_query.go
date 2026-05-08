// pkg/observability/store_query.go — read-only queries for the API.
// [PILAR-XXVII/242.F]

package observability

import (
	"encoding/json"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ToolAggregate holds per-tool stats persisted across restarts.
type ToolAggregate struct {
	Name             string    `json:"name"`
	Calls            int       `json:"calls"`
	Errors           int       `json:"errors"`
	TotalDurationNs  int64     `json:"total_duration_ns"`
	P50Ns            int64     `json:"p50_ns"`
	P95Ns            int64     `json:"p95_ns"`
	P99Ns            int64     `json:"p99_ns"`
	LastCallAt       time.Time `json:"last_call_at"`
	// RecentDurs is the sliding-window of last 512 latencies.
	// Exported so encoding/json persists it across restarts; callers
	// should not depend on it — use P50/P95/P99 instead.
	RecentDurs       []int64   `json:"recent_durs,omitempty"`
}

// ErrorRate returns errors / calls, 0 when calls == 0.
func (a ToolAggregate) ErrorRate() float64 {
	if a.Calls == 0 {
		return 0
	}
	return float64(a.Errors) / float64(a.Calls)
}

// recomputePercentiles updates P50/P95/P99 from RecentDurs (sorted copy).
func (a *ToolAggregate) recomputePercentiles() {
	n := len(a.RecentDurs)
	if n == 0 {
		a.P50Ns = 0
		a.P95Ns = 0
		a.P99Ns = 0
		return
	}
	sorted := make([]int64, n)
	copy(sorted, a.RecentDurs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	a.P50Ns = sorted[n/2]
	a.P95Ns = sorted[(n*95)/100]
	a.P99Ns = sorted[(n*99)/100]
}

// ToolAggregates returns all tool aggregates keyed by tool name.
func (s *Store) ToolAggregates() map[string]ToolAggregate {
	out := make(map[string]ToolAggregate)
	if s == nil {
		return out
	}
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketToolAggregate))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var agg ToolAggregate
			if err := json.Unmarshal(v, &agg); err == nil {
				// Strip internal sliding window from the public map.
				agg.RecentDurs = nil
				out[string(k)] = agg
			}
			return nil
		})
	})
	return out
}

// MemStatsHistory returns snapshots with timestamp >= since.
func (s *Store) MemStatsHistory(since time.Time) []MemStatsSnapshot {
	var out []MemStatsSnapshot
	if s == nil {
		return out
	}
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketMemStatsRing))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var snap MemStatsSnapshot
			if err := json.Unmarshal(v, &snap); err == nil {
				if snap.Timestamp.After(since) || snap.Timestamp.Equal(since) {
					out = append(out, snap)
				}
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

// TokensBySource returns all token entries for the given day, grouped
// by source (mcp_traffic | internal_inference).
func (s *Store) TokensBySource(day string) map[TokenSource][]TokenEntry {
	out := make(map[TokenSource][]TokenEntry)
	if s == nil {
		return out
	}
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketTokensUsage))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e TokenEntry
			if err := json.Unmarshal(v, &e); err == nil {
				if day == "" || e.Day == day {
					out[e.Source] = append(out[e.Source], e)
				}
			}
			return nil
		})
	})
	return out
}

// TokensLast7Days aggregates tokens per day across sources for the last
// 7 days. Returns entries sorted by day ascending.
func (s *Store) TokensLast7Days() []TokenDaySummary {
	if s == nil {
		return nil
	}
	now := s.now()
	byDay := make(map[string]*TokenDaySummary)
	cutoff := now.AddDate(0, 0, -7)
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketTokensUsage))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e TokenEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return nil
			}
			t, err := time.Parse("2006-01-02", e.Day)
			if err != nil {
				return nil
			}
			if t.Before(cutoff) {
				return nil
			}
			sum := byDay[e.Day]
			if sum == nil {
				sum = &TokenDaySummary{Day: e.Day}
				byDay[e.Day] = sum
			}
			if e.Source == SourceMCPTraffic {
				sum.MCPInput += e.InputTokens
				sum.MCPOutput += e.OutputTokens
			} else {
				sum.InternalInput += e.InputTokens
				sum.InternalOutput += e.OutputTokens
			}
			sum.CostUSD += e.CostUSD
			return nil
		})
	})
	out := make([]TokenDaySummary, 0, len(byDay))
	for _, sum := range byDay {
		out = append(out, *sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// TokenDaySummary is one row in the 7-day stacked view.
type TokenDaySummary struct {
	Day            string  `json:"day"`
	MCPInput       int     `json:"mcp_input"`
	MCPOutput      int     `json:"mcp_output"`
	InternalInput  int     `json:"internal_input"`
	InternalOutput int     `json:"internal_output"`
	CostUSD        float64 `json:"cost_usd"`
}

// MutationsLast24h returns certified + bypassed counts for the last 24 h.
func (s *Store) MutationsLast24h() (certified, bypassed int, topHotspots []HotspotEntry) {
	if s == nil {
		return 0, 0, nil
	}
	cutoff := s.now().Add(-24 * time.Hour)
	counts := make(map[string]int)
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketMutationsHistory))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e MutationEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return nil
			}
			if e.Timestamp.Before(cutoff) {
				return nil
			}
			if e.Bypassed {
				bypassed++
			} else {
				certified++
			}
			counts[e.Path]++
			return nil
		})
	})
	for path, n := range counts {
		topHotspots = append(topHotspots, HotspotEntry{Path: path, Count: n})
	}
	sort.Slice(topHotspots, func(i, j int) bool { return topHotspots[i].Count > topHotspots[j].Count })
	if len(topHotspots) > 10 {
		topHotspots = topHotspots[:10]
	}
	return
}

// HotspotEntry is one entry in the mutations top-N list.
type HotspotEntry struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// RecentEvents returns the last n events (latest first).
func (s *Store) RecentEvents(n int) []EventEntry {
	if s == nil || n <= 0 {
		return nil
	}
	var out []EventEntry
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketEventsRing))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		// Iterate in reverse (newest first).
		for k, v := c.Last(); k != nil && len(out) < n; k, v = c.Prev() {
			var e EventEntry
			if err := json.Unmarshal(v, &e); err == nil {
				out = append(out, e)
			}
		}
		return nil
	})
	return out
}

// TotalRecords is the in-memory counter of writes observed this session.
func (s *Store) TotalRecords() uint64 {
	if s == nil {
		return 0
	}
	return s.totalRecords.Load()
}

// TotalFlushes is the in-memory counter of successful flushes.
func (s *Store) TotalFlushes() uint64 {
	if s == nil {
		return 0
	}
	return s.totalFlushes.Load()
}

// CallExport is a single tool call record for session-portability export. [130.1]
type CallExport struct {
	Tool       string    `json:"tool"`
	Action     string    `json:"action,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	DurationMs int64     `json:"duration_ms"`
	Status     string    `json:"status"`
	InBytes    int       `json:"in_bytes"`
	OutBytes   int       `json:"out_bytes"`
}

// RecentCalls returns the last n tool-call records from today's (and
// yesterday's, if needed) BoltDB bucket, sorted newest-first. [130.1]
func (s *Store) RecentCalls(n int) []CallExport {
	if s == nil || n <= 0 {
		return nil
	}
	var out []CallExport
	now := time.Now().UTC()
	days := []string{now.Format("20060102"), now.AddDate(0, 0, -1).Format("20060102")}
	_ = s.db.View(func(tx *bolt.Tx) error {
		for _, day := range days {
			b := tx.Bucket([]byte(toolCallsBucketPrefix + day))
			if b == nil {
				continue
			}
			c := b.Cursor()
			for k, v := c.Last(); k != nil && len(out) < n*2; k, v = c.Prev() {
				var rec toolCallRecord
				if err := json.Unmarshal(v, &rec); err != nil {
					continue
				}
				out = append(out, CallExport{
					Tool:       rec.Name,
					Action:     rec.Action,
					Timestamp:  rec.Timestamp,
					DurationMs: rec.DurNs / int64(time.Millisecond),
					Status:     rec.Status,
					InBytes:    rec.InBytes,
					OutBytes:   rec.OutBytes,
				})
			}
			if len(out) >= n {
				break
			}
		}
		return nil
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
