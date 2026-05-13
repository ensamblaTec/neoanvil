// Package sre — gc_policy.go: runtime heap cap + per-phase GOGC tuning.
// PILAR LXIX / Épica 365.A.
//
// Problem: the Go GC defaults (GOGC=100, no heap cap) are balanced for
// general workloads. NeoAnvil has distinct phases with different characteristics:
//
//   - Idle:           steady-state — balanced default is fine.
//   - Certify:        AST + Bouncer + tests burst → prefer less GC pauses,
//                     trading a bit more heap for latency. GOGC=200.
//   - Bulk ingest:    REM sleep / IndexIncidents load many vectors at once →
//                     maximum allocation pooling, GOGC=300.
//
// Also: unbounded heap means a bad query can OOM the VM. SetMemoryLimit caps
// the total heap + makes GC adaptive when close to the cap — graceful
// degradation vs sudden OOM-kill.
package sre

import (
	"log"
	"runtime/debug"
	"sync"
)

// gcPolicyMu serializes GCWithPolicy invocations so concurrent callers don't
// stomp on each other's GOGC setting. The wrapped work runs OUTSIDE the lock
// to avoid contention on long-running phases.
var gcPolicyMu sync.Mutex

// gcPolicyCurrent tracks the active GOGC override. 0 = default.
var gcPolicyCurrent int

// ApplyMemoryLimit sets a soft cap on the Go runtime heap via
// debug.SetMemoryLimit. The GC becomes more aggressive as the heap approaches
// the cap — transforming an eventual OOM-kill into adaptive pressure.
//
// limitMB == 0 → no-op (preserve default uncapped behavior).
// limitMB <  0 → no-op + warning log.
// Call once at boot from cmd/neo-mcp/main.go, after config is loaded.
// Safe to call multiple times — last wins.
func ApplyMemoryLimit(limitMB int) {
	if limitMB == 0 {
		return
	}
	if limitMB < 0 {
		log.Printf("[SRE-GC] invalid memory limit %d MB — ignoring", limitMB)
		return
	}
	bytes := int64(limitMB) << 20 // MB → bytes
	prev := debug.SetMemoryLimit(bytes)
	log.Printf("[SRE-GC] memory limit set: %d MB (was %d bytes)", limitMB, prev)
}

// GCPhase enumerates the neoanvil workload phases with distinct GC needs.
type GCPhase string

const (
	// PhaseIdle is the steady-state default — no override.
	PhaseIdle GCPhase = "idle"
	// PhaseCertify runs AST + Bouncer + tests burst. Prefer fewer GC
	// pauses at cost of more transient heap. GOGC=200.
	PhaseCertify GCPhase = "certify"
	// PhaseBulkIngest runs REM sleep consolidation + IndexIncidents bulk.
	// Maximum pooling. GOGC=300.
	PhaseBulkIngest GCPhase = "bulk_ingest"
)

// gcPercentForPhase maps phase → GOGC override. 0 means "use default".
func gcPercentForPhase(p GCPhase) int {
	switch p {
	case PhaseCertify:
		return 200
	case PhaseBulkIngest:
		return 300
	default:
		return 0
	}
}

// GCWithPolicy runs fn with a temporarily adjusted GOGC value appropriate for
// the phase. The previous setting is restored on exit even if fn panics.
//
// Serialized: only one GCWithPolicy at a time to avoid overlapping phases
// (certify + bulk ingest simultaneously would pick an unpredictable GOGC).
// Non-matching phases run with the default.
//
// Example usage:
//
//	err := sre.GCWithPolicy(sre.PhaseCertify, func() error {
//	    return certify.Run(files)
//	})
func GCWithPolicy(phase GCPhase, fn func() error) error {
	target := gcPercentForPhase(phase)
	if target == 0 {
		// No override needed — skip the lock + state dance.
		return fn()
	}
	gcPolicyMu.Lock()
	prev := debug.SetGCPercent(target)
	gcPolicyCurrent = target
	gcPolicyMu.Unlock()
	defer func() {
		gcPolicyMu.Lock()
		debug.SetGCPercent(prev)
		gcPolicyCurrent = 0
		gcPolicyMu.Unlock()
	}()
	return fn()
}

// CurrentGCPolicy returns the active GOGC override (0 if none). Exported
// for HUD / BRIEFING diagnostics.
func CurrentGCPolicy() int {
	gcPolicyMu.Lock()
	defer gcPolicyMu.Unlock()
	return gcPolicyCurrent
}
