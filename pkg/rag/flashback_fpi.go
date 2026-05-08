// FlashbackFPI — Flashback Performance Index. [SRE-35.2]
//
// Tracks hit/miss ratio of flashback injections and identifies top-offending
// modules (directories with the most misses = knowledge gaps).
//
// Counters are atomic (hot-path safe). BoltDB persistence runs during REM cycles
// for long-term trend analysis. [SRE-35 note #3]
package rag

import (
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
)

var bucketFPI = []byte("hnsw_fpi")

// FPISnapshot is a point-in-time snapshot persisted to BoltDB.
type FPISnapshot struct {
	Hits         int64              `json:"hits"`
	Misses       int64              `json:"misses"`
	HitRate      float64            `json:"hit_rate"`
	TopOffenders []ModuleStats      `json:"top_offenders"` // directories with most misses
	At           time.Time          `json:"at"`
}

// ModuleStats holds hit/miss counts for a single directory. [SRE-35.2.2]
type ModuleStats struct {
	Dir    string `json:"dir"`
	Hits   int64  `json:"hits"`
	Misses int64  `json:"misses"`
}

// perDirCounters holds atomic counters per directory.
type perDirCounters struct {
	hits   atomic.Int64
	misses atomic.Int64
}

// FlashbackFPI tracks the performance of flashback injections. [SRE-35.2.1]
// One instance per daemon — shared across all workspaces.
type FlashbackFPI struct {
	totalHits   atomic.Int64
	totalMisses atomic.Int64
	byDir       sync.Map // map[string]*perDirCounters
}

// NewFlashbackFPI returns an initialized FPI tracker.
func NewFlashbackFPI() *FlashbackFPI {
	return &FlashbackFPI{}
}

// RecordHit records a flashback hit (flashback was returned with dist < threshold).
// O(1) — safe on query hot-path.
func (f *FlashbackFPI) RecordHit(dir string) {
	f.totalHits.Add(1)
	f.countersFor(dir).hits.Add(1)
}

// RecordMiss records a flashback miss (no relevant result found).
// O(1) — safe on query hot-path.
func (f *FlashbackFPI) RecordMiss(dir string) {
	f.totalMisses.Add(1)
	f.countersFor(dir).misses.Add(1)
}

// HitRate returns the current hit rate as a fraction in [0, 1].
func (f *FlashbackFPI) HitRate() float64 {
	hits := f.totalHits.Load()
	misses := f.totalMisses.Load()
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// Snapshot builds a FPISnapshot. Called during REM cycle. [SRE-35 note #1]
func (f *FlashbackFPI) Snapshot(topN int) FPISnapshot {
	hits := f.totalHits.Load()
	misses := f.totalMisses.Load()
	total := hits + misses
	rate := float64(0)
	if total > 0 {
		rate = float64(hits) / float64(total)
	}

	// Collect per-dir stats.
	var dirStats []ModuleStats
	f.byDir.Range(func(k, v any) bool {
		c := v.(*perDirCounters)
		dirStats = append(dirStats, ModuleStats{
			Dir:    k.(string),
			Hits:   c.hits.Load(),
			Misses: c.misses.Load(),
		})
		return true
	})

	// Sort by misses descending — highest-offending first. [SRE-35.2.2]
	sort.Slice(dirStats, func(i, j int) bool {
		return dirStats[i].Misses > dirStats[j].Misses
	})
	if topN > 0 && len(dirStats) > topN {
		dirStats = dirStats[:topN]
	}

	return FPISnapshot{
		Hits:         hits,
		Misses:       misses,
		HitRate:      rate,
		TopOffenders: dirStats,
		At:           time.Now(),
	}
}

// PersistFPI saves the current FPI snapshot to BoltDB under key
// "fpi_YYYY-MM-DD" for long-term trend analysis. [SRE-35 note #3]
// MUST be called only during REM cycle.
func (wal *WAL) PersistFPI(fpi *FlashbackFPI, topN int) error {
	if wal == nil || wal.db == nil {
		return nil
	}
	snap := fpi.Snapshot(topN)
	return wal.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketFPI)
		if err != nil {
			return err
		}
		key := []byte("fpi_" + snap.At.UTC().Format("2006-01-02"))
		data, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

// LoadFPITrend returns the last `limit` daily FPI snapshots from BoltDB.
func (wal *WAL) LoadFPITrend(limit int) ([]FPISnapshot, error) {
	if wal == nil || wal.db == nil {
		return nil, nil
	}
	var results []FPISnapshot
	err := wal.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketFPI)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			if !hasPrefix(k, "fpi_") {
				continue
			}
			var s FPISnapshot
			if err := json.Unmarshal(v, &s); err == nil {
				results = append(results, s)
				if len(results) >= limit {
					break
				}
			}
		}
		return nil
	})
	return results, err
}

// countersFor returns or creates the per-directory counter pair.
func (f *FlashbackFPI) countersFor(dir string) *perDirCounters {
	v, _ := f.byDir.LoadOrStore(dir, &perDirCounters{})
	return v.(*perDirCounters)
}
