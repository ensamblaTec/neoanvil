// pkg/state/planner_cache.go — memoization for master_plan.md parse results.
// Tier 1C of the LARGE-PROJECT roadmap (2026-05-13).
//
// Problem: ReadActivePhase and ReadOpenTasks are called from every BRIEFING
// invocation plus handleReadMasterPlan. Each call re-reads master_plan.md
// (~50KB on strategos) and re-parses line-by-line. For workspaces with
// frequent BRIEFING calls, this is observable I/O + CPU overhead and
// contributes to the `⚠️ TokenBudget: neo_radar(NK out)` warning.
//
// Solution: keep a per-workspace cache of the parsed results keyed by
// (mtime, size). When master_plan.md is unchanged, return the cached parse
// directly. When it changes (mtime or size differs), invalidate and re-parse.
//
// Cache scope: process-local, no TTL (relies on mtime invalidation).
// Concurrency: sync.RWMutex; the parse itself happens outside the lock to
// avoid blocking readers on a slow rebuild.

package state

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// planParseEntry caches the output of ReadActivePhase + ReadOpenTasks for a
// single (workspace, mtime, size) tuple. Errors are cached too — a missing
// master_plan.md returns the same error consistently until file appears.
type planParseEntry struct {
	mtime          time.Time
	size           int64
	activePhase    string
	activePhaseErr error
	openTasks      string
	openTasksErr   error
}

var (
	planCacheMu sync.RWMutex
	planCache   = make(map[string]*planParseEntry) // workspace → entry

	// Atomic counters for observability — Stats() reads these without locking.
	planCacheHits   atomic.Int64
	planCacheMisses atomic.Int64
	planCacheStale  atomic.Int64 // invalidations on mtime/size mismatch
)

// PlannerCacheStats reports cache effectiveness. Exposed for future neo_cache
// stats integration.
type PlannerCacheStats struct {
	Entries int   `json:"entries"`
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
	Stale   int64 `json:"stale_invalidations"`
}

// GetPlannerCacheStats returns a coherent snapshot of cache counters.
func GetPlannerCacheStats() PlannerCacheStats {
	planCacheMu.RLock()
	entries := len(planCache)
	planCacheMu.RUnlock()
	return PlannerCacheStats{
		Entries: entries,
		Hits:    planCacheHits.Load(),
		Misses:  planCacheMisses.Load(),
		Stale:   planCacheStale.Load(),
	}
}

// InvalidatePlannerCache forcibly removes the cached parse for a workspace.
// Used by MARK_DONE handler after rewriting master_plan.md.
func InvalidatePlannerCache(workspace string) {
	planCacheMu.Lock()
	delete(planCache, workspace)
	planCacheMu.Unlock()
}

// statMasterPlan returns the file info for master_plan.md, or nil + error
// when the file is unreadable. Used to validate cache freshness without
// loading the file content.
func statMasterPlan(workspace string) (os.FileInfo, error) {
	path := filepath.Join(workspace, ".neo", "master_plan.md")
	return os.Stat(path)
}

// lookupCachedPlan returns the cached entry for workspace if mtime + size
// match the on-disk file. Returns (nil, false) on cache miss, on file
// vanish, or on stale entry.
func lookupCachedPlan(workspace string) (*planParseEntry, bool) {
	info, err := statMasterPlan(workspace)
	if err != nil {
		// File missing: invalidate cache (the next caller's ReadFile will
		// fail with the canonical error message).
		planCacheMu.Lock()
		if _, ok := planCache[workspace]; ok {
			delete(planCache, workspace)
			planCacheStale.Add(1)
		}
		planCacheMu.Unlock()
		return nil, false
	}
	planCacheMu.RLock()
	entry, ok := planCache[workspace]
	planCacheMu.RUnlock()
	if !ok {
		planCacheMisses.Add(1)
		return nil, false
	}
	if !entry.mtime.Equal(info.ModTime()) || entry.size != info.Size() {
		planCacheMu.Lock()
		delete(planCache, workspace)
		planCacheMu.Unlock()
		planCacheStale.Add(1)
		planCacheMisses.Add(1)
		return nil, false
	}
	planCacheHits.Add(1)
	return entry, true
}

// storeCachedPlan installs an entry computed from a fresh parse. mtime + size
// come from the same Stat call that preceded the parse, so the cache stays
// coherent with on-disk content as observed by this process.
func storeCachedPlan(workspace string, info os.FileInfo, activePhase string, activePhaseErr error, openTasks string, openTasksErr error) {
	entry := &planParseEntry{
		mtime:          info.ModTime(),
		size:           info.Size(),
		activePhase:    activePhase,
		activePhaseErr: activePhaseErr,
		openTasks:      openTasks,
		openTasksErr:   openTasksErr,
	}
	planCacheMu.Lock()
	planCache[workspace] = entry
	planCacheMu.Unlock()
}
