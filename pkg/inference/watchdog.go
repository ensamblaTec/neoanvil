// Watchdog implements the Safe-Command Filter and Autonomous Loop. [SRE-34.3.1/34.3.2]
//
// In UNSUPERVISED mode the Watchdog automatically approves commands whose prefix
// matches the safe_commands whitelist (from neo.yaml). Every auto-approval emits
// an EventAutoApprove event to the Dashboard with the [AUTO-APPROVED] tag. [SRE-34 note #3]
//
// Isolation guarantee: the Watchdog is goroutine-safe. Multiple workspace goroutines
// may call IsSafe and AutoApprove concurrently without cross-workspace interference.
package inference

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/pubsub"
)

// Watchdog guards command execution in UNSUPERVISED mode.
type Watchdog struct {
	safeCommands  []string
	bus           *pubsub.Bus
	maxCycles     int
	cycles        atomic.Int32
	unsupervised  atomic.Bool
	workspaceID   string
}

// NewWatchdog creates a Watchdog bound to a pubsub.Bus.
// safeCommands is the whitelist from neo.yaml sre.safe_commands.
// maxCycles is neo.yaml sre.unsupervised_max_cycles.
func NewWatchdog(safeCommands []string, maxCycles int, bus *pubsub.Bus, workspaceID string) *Watchdog {
	return &Watchdog{
		safeCommands: safeCommands,
		bus:          bus,
		maxCycles:    maxCycles,
		workspaceID:  workspaceID,
	}
}

// EnableUnsupervised activates UNSUPERVISED mode and resets the cycle counter.
func (w *Watchdog) EnableUnsupervised() {
	w.cycles.Store(0)
	w.unsupervised.Store(true)
	log.Printf("[WATCHDOG][%s] UNSUPERVISED mode activated (max_cycles=%d)", w.workspaceID, w.maxCycles)
}

// DisableUnsupervised reverts to supervised mode immediately.
func (w *Watchdog) DisableUnsupervised() {
	w.unsupervised.Store(false)
	log.Printf("[WATCHDOG][%s] UNSUPERVISED mode deactivated after %d cycles", w.workspaceID, w.cycles.Load())
}

// IsUnsupervised reports whether the Watchdog is in autonomous mode.
func (w *Watchdog) IsUnsupervised() bool { return w.unsupervised.Load() }

// CyclesUsed returns the number of auto-approved commands in the current session.
func (w *Watchdog) CyclesUsed() int { return int(w.cycles.Load()) }

// IsSafe reports whether cmd is safe to auto-approve based on the whitelist.
// Matching is prefix-based and case-insensitive.
func (w *Watchdog) IsSafe(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	lower := strings.ToLower(trimmed)
	for _, safe := range w.safeCommands {
		if strings.HasPrefix(lower, strings.ToLower(safe)) {
			return true
		}
	}
	return false
}

// AutoApproveResult carries the outcome of an AutoApprove call.
type AutoApproveResult struct {
	Approved    bool
	Reason      string
	CycleNumber int
}

// AutoApprove decides whether to auto-approve cmd in UNSUPERVISED mode.
//
// Returns Approved=true only when:
//   - UNSUPERVISED mode is active, AND
//   - cycle limit has not been exhausted, AND
//   - cmd prefix is in the safe_commands whitelist
//
// Every approval emits an EventAutoApprove to the Dashboard bus. [SRE-34 note #3]
func (w *Watchdog) AutoApprove(cmd string) AutoApproveResult {
	if !w.unsupervised.Load() {
		return AutoApproveResult{Reason: "supervised mode active"}
	}

	cycle := int(w.cycles.Load()) + 1
	if cycle > w.maxCycles {
		w.unsupervised.Store(false)
		log.Printf("[WATCHDOG][%s] Max cycles (%d) reached — reverting to supervised", w.workspaceID, w.maxCycles)
		return AutoApproveResult{
			Reason: fmt.Sprintf("max UNSUPERVISED cycles (%d) exhausted — reverted to supervised", w.maxCycles),
		}
	}

	if !w.IsSafe(cmd) {
		return AutoApproveResult{
			Reason: "command not in safe_commands whitelist — requires WASM hypervisor validation",
		}
	}

	w.cycles.Add(1)

	payload := map[string]any{
		"tag":          "[AUTO-APPROVED]",
		"command":      cmd,
		"cycle":        cycle,
		"workspace_id": w.workspaceID,
		"at":           time.Now().UTC().Format(time.RFC3339),
	}

	if w.bus != nil {
		w.bus.Publish(pubsub.Event{
			Type:    pubsub.EventAutoApprove,
			Payload: payload,
		})
	}

	log.Printf("[WATCHDOG][%s] [AUTO-APPROVED] cycle=%d cmd=%q", w.workspaceID, cycle, cmd)

	return AutoApproveResult{
		Approved:    true,
		Reason:      "[AUTO-APPROVED] safe command",
		CycleNumber: cycle,
	}
}
