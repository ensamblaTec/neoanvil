// Package pubsub provides a thread-safe in-process publish/subscribe event bus
// used to stream NeoAnvil system events (RAPL, Bouncer, Flashback, MCTS) to the
// Operator HUD dashboard via SSE. [SRE-32.1.1]
package pubsub

import (
	"sync"
	"time"
)

// EventType identifies the category of a system event.
type EventType string

const (
	EventHeartbeat   EventType = "heartbeat"    // periodic vitals: watts, goroutines, state
	EventBouncer     EventType = "bouncer"       // certification result (pass/fail + file)
	EventFlashback   EventType = "flashback"     // episodic memory injection
	EventMCTS        EventType = "mcts"          // MCTS tree snapshot
	EventChaos       EventType = "chaos"         // chaos drill result
	EventAutoApprove    EventType = "auto_approve"      // [SRE-34.3.2] Watchdog auto-approved a command — [AUTO-APPROVED] tag
	EventInference      EventType = "inference"         // [SRE-34.2.1] Inference gateway decision (level + confidence)
	EventCognitiveDrift EventType = "cognitive_drift"   // [SRE-35.1.2] Rolling avg query distance > threshold
	EventMemoryCapacity EventType = "memory_capacity"   // [SRE-35.1.1] Workspace approaching vector capacity limit
	EventArenaThresh    EventType = "arena_thresh"      // [SRE-36.3.2] Pool miss-rate > 20% — pool too small for current load
	EventGCPressure     EventType = "gc_pressure"       // [SRE-36.1.3] NumGC rose > 5 during a single file ingestion
	EventConfigReloaded EventType = "config_reloaded"   // hot-reload: safe neo.yaml fields updated at runtime
	EventKineticAnomaly EventType = "kinetic_anomaly"   // [SRE-44] hardware anomaly: power/heap/GC deviation > threshold
	EventGhostCycle     EventType = "ghost_cycle"       // [SRE-50] ghost mode auto-approved a tool cycle
	EventPolicyVeto     EventType = "policy_veto"       // [SRE-40] constitutional rule vetoed an action
	EventDreamResult    EventType = "dream_result"      // [SRE-41] adversarial dream cycle result
	EventSuggestCommit  EventType = "suggest_commit"    // [SRE-56.1] all Kanban tasks DONE — prompt memory + commit
	EventThermalRollback  EventType = "thermal_rollback"  // [SRE-57.2] critical RAPL sustained — git stash triggered
	EventOOMGuard         EventType = "oom_guard"         // [SRE-57.3] heap exceeded threshold — forced GC + FreeOSMemory
	EventSuggestCompress  EventType = "suggest_compress"  // [SRE-58.1] session IO > threshold — proactive context compression
	EventOracleAlert      EventType = "oracle_alert"      // [SRE-61.3] failure probability > AlertThreshold
	EventWorkspaceSwitched      EventType = "workspace_switched"       // [SRE-68.4.3] active workspace changed via nexus
	EventCrossWorkspaceImpact  EventType = "cross_workspace_impact"  // [SRE-87.B.3] scatter-gather detected cross-workspace blast radius
	EventCachePulse             EventType = "cache_pulse"              // [PILAR-XXV/186] periodic RAG cache stats + search-path counters
	EventCacheThrash            EventType = "cache_thrash"             // [PILAR-XXV/193] evict_rate > 30% — operator should bump query_cache_capacity
	EventContractDrift          EventType = "contract_drift"           // [PILAR-XXXVIII/292.B] certify detected breaking HTTP contract change
	EventNexusDebtWarning       EventType = "nexus_debt_warning"       // [PILAR LXVI / 353.A] Nexus reports P0+ debt affecting this workspace at boot
	EventDaemonBudgetWarning    EventType = "daemon_budget_warning"    // [132.B] daemon session token usage reached 90% of configured limit
	EventDaemonProgress         EventType = "daemon_progress"           // [132.C] daemon task queue state update for HUD DaemonQueuePanel
)

// Event is the typed envelope published by any NeoAnvil subsystem.
type Event struct {
	Type    EventType `json:"type"`
	At      time.Time `json:"at"`
	Payload any       `json:"payload"`
}

// Bus is a non-blocking fan-out broadcast bus. Multiple goroutines may safely
// call Publish and Subscribe concurrently.
type Bus struct {
	mu   sync.RWMutex
	subs []chan Event
}

// NewBus allocates an empty Bus.
func NewBus() *Bus { return &Bus{} }

// Subscribe returns a buffered channel that receives future events and an
// unsubscribe closure. The caller MUST invoke unsubscribe when done (e.g. on
// SSE connection close) to avoid leaking channels.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, s := range b.subs {
			if s == ch {
				b.subs = append(b.subs[:i], b.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}
}

// Publish broadcasts e to all active subscribers. Slow subscribers are
// silently dropped (select default) — never blocks the publisher.
func (b *Bus) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // subscriber fell behind — drop to preserve publisher latency
		}
	}
}
