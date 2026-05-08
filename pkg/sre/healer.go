package sre

// healer.go — SelfHealerDaemon: Épica 57 autonomous survival reflexes.
//
// Three survival mechanisms:
//   [57.1] Panic Reaper   — supervised goroutine registry with heartbeat watchdog.
//                           Goroutines that miss pings for >StaleThreshold are restarted.
//   [57.2] Thermal Rollback — when RAPL stays above CriticalWatts for N consecutive
//                           ticks, stash uncommitted git changes to reduce I/O heat.
//   [57.3] OOM Guard      — when heap exceeds OOMThresholdMB, force GC + OS memory
//                           release via debug.FreeOSMemory().

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// PoolFlusher is implemented by memx.ObservablePool — allows OOMGuard to
// reset pool metrics without a direct import of memx (breaks import cycle).
type PoolFlusher interface {
	Flush() // releases all pooled slabs back to the GC
}

// SupervisedRoutine is a goroutine that has registered with the Reaper.
type SupervisedRoutine struct {
	Name    string
	Restart func()     // called when zombie detected
	lastPing atomic.Int64 // unix nanoseconds of last Ping()
}

// Ping updates the liveness timestamp. Call this inside the goroutine's main loop.
func (sr *SupervisedRoutine) Ping() {
	sr.lastPing.Store(time.Now().UnixNano())
}

// SelfHealerDaemon coordinates all three survival reflexes. [SRE-57]
type SelfHealerDaemon struct {
	workspace     string
	routines      sync.Map      // name → *SupervisedRoutine
	StaleThreshold time.Duration // default 90s — zombie if no ping received
	CriticalWatts  float64       // default 80.0W — thermal rollback threshold
	OOMThresholdMB uint64        // default 512 MB heap

	thermalTicks atomic.Int32 // consecutive ticks above CriticalWatts
}

// NewSelfHealerDaemon creates a daemon bound to workspace with conservative defaults.
func NewSelfHealerDaemon(workspace string) *SelfHealerDaemon {
	return &SelfHealerDaemon{
		workspace:      workspace,
		StaleThreshold: 90 * time.Second,
		CriticalWatts:  80.0,
		OOMThresholdMB: 512,
	}
}

// Register adds a goroutine to the supervised set and returns its handle. [SRE-57.1]
// The caller MUST call handle.Ping() periodically inside its loop.
func (h *SelfHealerDaemon) Register(name string, restart func()) *SupervisedRoutine {
	sr := &SupervisedRoutine{Name: name, Restart: restart}
	sr.lastPing.Store(time.Now().UnixNano()) // initialise as alive
	h.routines.Store(name, sr)
	log.Printf("[HEALER] Supervised routine registered: %s", name)
	return sr
}

// Unregister removes a routine from supervision (call on clean shutdown). [SRE-57.1]
func (h *SelfHealerDaemon) Unregister(name string) {
	h.routines.Delete(name)
}

// RunReaper scans all supervised routines and restarts any that are stale. [SRE-57.1]
// Call this from the homeostasis goroutine every tick.
func (h *SelfHealerDaemon) RunReaper() int {
	now := time.Now().UnixNano()
	stale := int64(h.StaleThreshold)
	restarted := 0

	h.routines.Range(func(_, v any) bool {
		sr := v.(*SupervisedRoutine)
		age := now - sr.lastPing.Load()
		if age > stale {
			log.Printf("[REAPER] Zombie detected: %s (silent for %.1fs) — restarting.", sr.Name, float64(age)/1e9)
			go sr.Restart()
			sr.lastPing.Store(time.Now().UnixNano()) // reset to avoid tight restart loop
			restarted++
		}
		return true
	})
	return restarted
}

// ThermalRollback monitors cumulative thermal ticks and stashes git changes
// when RAPL stays above CriticalWatts for 3 consecutive ticks. [SRE-57.2]
// Returns a non-empty message if a rollback was attempted.
func (h *SelfHealerDaemon) ThermalRollback(watts float64) string {
	if watts < h.CriticalWatts {
		h.thermalTicks.Store(0)
		return ""
	}

	ticks := h.thermalTicks.Add(1)
	log.Printf("[HEALER] Thermal critical tick %d: %.2fW > %.2fW threshold", ticks, watts, h.CriticalWatts)

	if ticks < 3 {
		return ""
	}

	// 3 consecutive critical ticks — stash uncommitted changes.
	// IMPORTANT: gitStash is dispatched asynchronously so the homeostasis
	// goroutine is never blocked by slow disk I/O under thermal pressure.
	// The select below gives the stash 5 s; if it misses, we log and move on —
	// the next tick will retry (thermalTicks was already reset to 0).
	h.thermalTicks.Store(0) // reset before acting

	type stashResult struct {
		msg string
		err error
	}
	done := make(chan stashResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), gitStashTimeout)
		defer cancel()
		msg, err := h.gitStash(ctx)
		done <- stashResult{msg, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			if errors.Is(r.err, context.DeadlineExceeded) {
				log.Printf("[HEALER] ThermalRollback git stash timed out (%s) — skipped under thermal pressure.", gitStashTimeout)
				return fmt.Sprintf("[THERMAL-ROLLBACK] %.1fW critical — stash timed out, will retry next tick", watts)
			}
			log.Printf("[HEALER] ThermalRollback git stash failed: %v", r.err)
			return fmt.Sprintf("[THERMAL-ROLLBACK] stash failed: %v", r.err)
		}
		log.Printf("[HEALER] ThermalRollback: git stash executed — %s", r.msg)
		return fmt.Sprintf("[THERMAL-ROLLBACK] %.1fW critical — uncommitted changes stashed. Restore with: git stash pop", watts)
	case <-time.After(gitStashTimeout + 500*time.Millisecond):
		// Goroutine is still running (process kill pending) — homeostasis loop must not wait.
		log.Printf("[HEALER] ThermalRollback: homeostasis unblocked — stash goroutine running in background.")
		return fmt.Sprintf("[THERMAL-ROLLBACK] %.1fW critical — stash dispatched async (disk under pressure)", watts)
	}
}

// gitStashTimeout is the maximum time gitStash may block the caller.
// Kept short: thermal emergency demands fast I/O decisions, not correct ones.
const gitStashTimeout = 5 * time.Second

// gitStash runs `git stash` in the workspace to preserve but shelve hot changes.
// ctx must carry a deadline — callers should use context.WithTimeout.
func (h *SelfHealerDaemon) gitStash(ctx context.Context) (string, error) {
	absWS, err := filepath.Abs(h.workspace)
	if err != nil {
		return "", fmt.Errorf("invalid workspace: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "stash", "--include-untracked", "-m", "[SRE-57] thermal-emergency auto-stash") //nolint:gosec // G204-LITERAL-BIN
	cmd.Dir = absWS
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		return string(out), ctx.Err() // surface timeout/cancel explicitly
	}
	return string(out), err
}

// OOMGuard checks runtime heap and forces GC + OS memory release if over threshold. [SRE-57.3]
// pool may be nil — if provided, Flush() is called first to release Arena slabs.
// Returns true if emergency cleanup was triggered.
func (h *SelfHealerDaemon) OOMGuard(pool PoolFlusher) bool {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	heapMB := ms.HeapInuse / (1024 * 1024)
	if heapMB < h.OOMThresholdMB {
		return false
	}

	log.Printf("[OOM-GUARD] Heap %.0f MB > threshold %d MB — forcing Arena flush + GC.", float64(heapMB), h.OOMThresholdMB)

	if pool != nil {
		pool.Flush()
		log.Printf("[OOM-GUARD] Arena PMEM pool flushed.")
	}

	runtime.GC()
	debug.FreeOSMemory()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	log.Printf("[OOM-GUARD] Post-cleanup heap: %.0f MB (freed %.0f MB).",
		float64(after.HeapInuse)/(1024*1024),
		float64(ms.HeapInuse-after.HeapInuse)/(1024*1024),
	)
	return true
}
