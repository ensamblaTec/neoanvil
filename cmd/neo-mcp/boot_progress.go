// cmd/neo-mcp/boot_progress.go — boot-phase observability. ÉPICA 148 / PILAR XXIX.
//
// Motivation: HNSW WAL load can take ~6 min on a 3.3 GB database. While
// it runs, Nexus reports the child as `status=starting` for the whole
// duration with zero signal of "stuck vs slow loading". Operators see
// `/mcp` dialog dismissed and conclude the workspace is dead. This module
// surfaces progress as both periodic log lines and a structured
// `/boot_progress` endpoint so the agent (and BRIEFING from peers) can
// distinguish "alive, ETA Nm" from "hung".
//
// Approach: an atomic counters struct + a goroutine ticker that samples
// /proc/self/io read_bytes (Linux) every N seconds. Process-wide reads
// include things other than HNSW (config, logs), so the percentage is a
// LOWER bound — actual bytes read from hnsw.db could be lower (other
// reads inflate the counter), but the SIGNAL "we are reading" is reliable.
// On non-Linux, the elapsed-time output still gives the operator
// "alive for Nm" confirmation.
//
// Concurrency: counters are atomic.Int64; the ticker reads `done` channel
// to terminate cleanly when LoadGraph returns. Lock-free.

package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// bootProgressTickerInterval is how often the ticker logs progress. 5s
// balances visibility (operator wants signal soon) against log noise
// (10+ lines for a 60s load is plenty).
const bootProgressTickerInterval = 5 * time.Second

// BootProgress tracks phase + progress of long boot operations.
// All fields are atomic — readers can snapshot without a lock.
type BootProgress struct {
	startedUnix      atomic.Int64
	hnswBytesTotal   atomic.Int64
	hnswBytesReadMin atomic.Int64 // lower bound: process-wide read_bytes since start
	hnswReady        atomic.Bool
	// [ÉPICA 149.E] true when LoadHNSWSnapshot succeeded → BRIEFING shows
	// "boot=fast" mirror of the existing CPG line (Épica 263.D).
	hnswBootedFast atomic.Bool
}

// MarkHNSWBootedFast records that the HNSW graph was loaded from a
// fresh snapshot (149) rather than a cold WAL rebuild. Read by BRIEFING
// to render `boot=fast` in the HNSW status line.
func (b *BootProgress) MarkHNSWBootedFast() {
	b.hnswBootedFast.Store(true)
	b.hnswReady.Store(true)
	// total/read fields are not meaningful for fast-boot path; leave them
	// zero so /boot_progress consumers see hnsw_pct=0 + phase=ready (the
	// snapshot doesn't expose a "what bytes were read" metric).
}

// HNSWBootedFast returns whether the last boot used the snapshot path.
// Stays true for the lifetime of the process (operator wants to know
// "this process is running on a fast boot" for diagnostics).
func (b *BootProgress) HNSWBootedFast() bool {
	return b.hnswBootedFast.Load()
}

// globalBootProgress is the process-wide singleton. neo-mcp boot calls
// StartHNSWLoad once + FinishHNSWLoad on completion. The HTTP handler
// reads this concurrently with the ticker writing to it.
var globalBootProgress = &BootProgress{}

// StartHNSWLoad records the start timestamp and the WAL file size so
// the percentage reported by /boot_progress is meaningful. If walPath
// can't be stat'd the total stays 0 and the response shows just elapsed.
func (b *BootProgress) StartHNSWLoad(walPath string) {
	b.startedUnix.Store(time.Now().Unix())
	if info, err := os.Stat(walPath); err == nil {
		b.hnswBytesTotal.Store(info.Size())
	}
	b.hnswBytesReadMin.Store(0)
	b.hnswReady.Store(false)
}

// FinishHNSWLoad jumps the read counter to total and flips ready to true
// so the next snapshot reports 100% and `phase=ready`.
func (b *BootProgress) FinishHNSWLoad() {
	b.hnswBytesReadMin.Store(b.hnswBytesTotal.Load())
	b.hnswReady.Store(true)
}

// readProcSelfIORead parses /proc/self/io for the read_bytes line and
// returns its value. Linux-only — returns 0 with err on other OSes or
// when the file is missing/malformed. Cheap (~30 µs).
func readProcSelfIORead() (int64, error) {
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		// "read_bytes: N"
		if rest, ok := strings.CutPrefix(line, "read_bytes: "); ok {
			n, perr := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if perr != nil {
				return 0, perr
			}
			return n, nil
		}
	}
	return 0, nil
}

// snapshotForJSON computes the current progress view. Reads /proc/self/io
// at call time so the value is fresh; falls back to the last ticker
// observation if /proc is unavailable.
func (b *BootProgress) snapshotForJSON() map[string]any {
	startedUnix := b.startedUnix.Load()
	total := b.hnswBytesTotal.Load()
	readMin := b.hnswBytesReadMin.Load()
	ready := b.hnswReady.Load()
	// Refresh from /proc when we're still loading — gives the caller the
	// freshest possible value rather than stale ticker output. When
	// finished, prefer the stored total (we explicitly synced it).
	if !ready {
		if cur, err := readProcSelfIORead(); err == nil && cur > readMin {
			readMin = cur
		}
	}
	phase := "loading"
	if ready {
		phase = "ready"
	}
	pct := 0.0
	if total > 0 {
		pct = float64(readMin) / float64(total)
		if pct > 1.0 {
			pct = 1.0 // process-wide reads can exceed file size; cap at 100%
		}
	}
	elapsed := int64(0)
	if startedUnix > 0 {
		elapsed = time.Now().Unix() - startedUnix
	}
	return map[string]any{
		"phase":             phase,
		"hnsw_bytes_total":  total,
		"hnsw_bytes_read":   readMin,
		"hnsw_pct":          pct,
		"started_at_unix":   startedUnix,
		"elapsed_seconds":   elapsed,
	}
}

// runBootProgressTicker logs progress every bootProgressTickerInterval
// until done is closed. Designed to run as a goroutine started from
// bootRAG just before wal.LoadGraph and torn down right after. Logs
// land in nexus-<workspace>.log so the operator can `tail -f` them
// during a slow boot. [148.A]
func runBootProgressTicker(done <-chan struct{}) {
	ticker := time.NewTicker(bootProgressTickerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			snap := globalBootProgress.snapshotForJSON()
			pct := snap["hnsw_pct"].(float64)
			read := snap["hnsw_bytes_read"].(int64)
			total := snap["hnsw_bytes_total"].(int64)
			elapsed := snap["elapsed_seconds"].(int64)
			emitBootProgressLog(pct, read, total, elapsed)
		}
	}
}

// emitBootProgressLog writes a single [BOOT-PROGRESS] line. Extracted
// for testability — tests override bootProgressLogger to capture output.
func emitBootProgressLog(pct float64, read, total, elapsed int64) {
	bootProgressLogger("[BOOT-PROGRESS] phase=hnsw_load read=%dMB total=%dMB pct=%.1f%% elapsed=%ds",
		read>>20, total>>20, pct*100, elapsed)
}

// bootProgressLogger is overridable in tests. Defaults to log.Printf.
var bootProgressLogger = func(format string, args ...any) {
	log.Printf(format, args...)
}

// handleBootProgress serves GET /boot_progress. Read-only.
func handleBootProgress(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(globalBootProgress.snapshotForJSON())
}
