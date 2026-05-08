// Package state — Causal Conscience engine for intent reconstruction. [SRE-39]
//
// Tracks the causal chain of decisions: why a mutation happened, what hardware
// state preceded it, and what prior errors led to the current action. Enables
// "situational flashbacks" that include reasoning context, not just code matches.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// ─── Causal Context Capture [SRE-39.1] ─────────────────────────────────────

// errorRing is a bounded ring buffer of recent error messages.
// Used to populate CausalContext.PriorErrors without allocation pressure.
type errorRing struct {
	mu    sync.Mutex
	buf   [8]string
	head  int
	count int
}

var globalErrorRing errorRing

// RecordError appends an error message to the global ring buffer. [SRE-39.1]
// Call this from error-handling paths (certify failures, build errors, etc.)
func RecordError(msg string) {
	globalErrorRing.mu.Lock()
	globalErrorRing.buf[globalErrorRing.head%8] = msg
	globalErrorRing.head++
	if globalErrorRing.count < 8 {
		globalErrorRing.count++
	}
	globalErrorRing.mu.Unlock()
}

// recentErrors returns the last N error messages (max 5).
func recentErrors(n int) []string {
	globalErrorRing.mu.Lock()
	defer globalErrorRing.mu.Unlock()

	if n > 5 {
		n = 5
	}
	if n > globalErrorRing.count {
		n = globalErrorRing.count
	}
	if n == 0 {
		return nil
	}

	result := make([]string, n)
	start := (globalErrorRing.head - n + 8) % 8
	for i := 0; i < n; i++ {
		result[i] = globalErrorRing.buf[(start+i)%8]
	}
	return result
}

// CaptureCausalContext snapshots the current system state for embedding in a MemexEntry. [SRE-39.1]
func CaptureCausalContext(triggerEvent string, parentID string) *CausalContext {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return &CausalContext{
		HeapMB:       float64(mem.HeapAlloc) / (1024 * 1024),
		GCRuns:       mem.NumGC,
		CPULoad:      estimateCPULoad(),
		RAPLWatts:    readRAPLWatts(),
		ServerMode:   os.Getenv("NEO_SERVER_MODE"),
		PriorErrors:  recentErrors(5),
		TriggerEvent: triggerEvent,
		ParentID:     parentID,
	}
}

// estimateCPULoad returns a rough CPU utilization estimate via NumGoroutine / GOMAXPROCS.
func estimateCPULoad() float64 {
	goroutines := runtime.NumGoroutine()
	procs := runtime.GOMAXPROCS(0)
	if procs == 0 {
		procs = 1
	}
	load := float64(goroutines) / float64(procs*10) // normalized heuristic
	if load > 1.0 {
		load = 1.0
	}
	return load
}

// readRAPLWatts reads Intel RAPL power consumption from sysfs. Returns 0 if unavailable.
func readRAPLWatts() float64 {
	data, err := os.ReadFile("/sys/class/powercap/intel-rapl:0/energy_uj")
	if err != nil {
		return 0
	}
	_ = data // Would need two reads with a time delta for actual watts calculation
	return 0 // Placeholder — actual implementation requires periodic sampling
}

// ─── Intent Reconstruction [SRE-39.2] ──────────────────────────────────────

// CausalChainEntry represents one link in a causal chain. [SRE-39.2]
type CausalChainEntry struct {
	EntryID      string         `json:"entry_id"`
	Topic        string         `json:"topic"`
	Content      string         `json:"content"`
	Timestamp    int64          `json:"timestamp"`
	Causal       *CausalContext `json:"causal,omitempty"`
	Depth        int            `json:"depth"` // 0 = target entry, 1 = parent, 2 = grandparent, ...
}

// ReconstructIntentChain walks backward through the ParentID links to reconstruct
// the causal chain that led to a specific memex entry. [SRE-39.2]
// Returns the chain from the target entry back to the root cause (max depth 10).
func ReconstructIntentChain(targetID string) ([]CausalChainEntry, error) {
	if plannerDB == nil {
		return nil, fmt.Errorf("planner DB not initialized")
	}

	var chain []CausalChainEntry
	currentID := targetID
	visited := make(map[string]bool)

	for depth := 0; depth < 10 && currentID != ""; depth++ {
		if visited[currentID] {
			break // prevent cycles
		}
		visited[currentID] = true

		entry, err := getMemexByID(currentID)
		if err != nil {
			break
		}

		chain = append(chain, CausalChainEntry{
			EntryID:   entry.ID,
			Topic:     entry.Topic,
			Content:   entry.Content,
			Timestamp: entry.Timestamp,
			Causal:    entry.Causal,
			Depth:     depth,
		})

		if entry.Causal == nil || entry.Causal.ParentID == "" {
			break
		}
		currentID = entry.Causal.ParentID
	}

	return chain, nil
}

// getMemexByID retrieves a single memex entry by ID from the active buffer or history.
func getMemexByID(id string) (*MemexEntry, error) {
	if plannerDB == nil {
		return nil, fmt.Errorf("planner DB not initialized")
	}

	var entry MemexEntry
	var found bool

	err := plannerDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(memexBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if found {
				return nil
			}
			var e MemexEntry
			if jsonErr := json.Unmarshal(v, &e); jsonErr == nil && e.ID == id {
				entry = e
				found = true
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("memex entry %s not found", id)
	}
	return &entry, nil
}

// ─── Situational Flashback [SRE-39.3] ──────────────────────────────────────

// SituationalFlashback enriches a flashback result with causal reasoning. [SRE-39.3]
// Given a matching memex entry, it reconstructs the intent chain and formats
// a human-readable explanation of why the previous decision was made.
type SituationalFlashback struct {
	OriginalContent string             `json:"original_content"`
	IntentChain     []CausalChainEntry `json:"intent_chain"`
	HardwareContext string             `json:"hardware_context"` // human-readable summary
	Reasoning       string             `json:"reasoning"`        // reconstructed reasoning
}

// BuildSituationalFlashback constructs a full situational flashback for a memex entry. [SRE-39.3]
func BuildSituationalFlashback(entryID string) (*SituationalFlashback, error) {
	chain, err := ReconstructIntentChain(entryID)
	if err != nil || len(chain) == 0 {
		return nil, fmt.Errorf("no causal chain found for %s: %w", entryID, err)
	}

	root := chain[0]
	fb := &SituationalFlashback{
		OriginalContent: root.Content,
		IntentChain:     chain,
	}

	// Build hardware context summary
	if root.Causal != nil {
		fb.HardwareContext = fmt.Sprintf(
			"Heap: %.1fMB | GC: %d | CPU: %.0f%% | Mode: %s | RAPL: %.1fW",
			root.Causal.HeapMB, root.Causal.GCRuns, root.Causal.CPULoad*100,
			root.Causal.ServerMode, root.Causal.RAPLWatts,
		)
	}

	// Reconstruct reasoning from the chain
	if len(chain) > 1 {
		fb.Reasoning = fmt.Sprintf(
			"This lesson was recorded because of '%s' (at %s). "+
				"The chain traces back %d steps to root cause: '%s'.",
			chain[0].Topic,
			time.Unix(chain[0].Timestamp, 0).Format("2006-01-02 15:04"),
			len(chain)-1,
			chain[len(chain)-1].Topic,
		)
	} else {
		fb.Reasoning = fmt.Sprintf(
			"Direct observation recorded at %s. Trigger: %s.",
			time.Unix(root.Timestamp, 0).Format("2006-01-02 15:04"),
			safeGetTrigger(root.Causal),
		)
	}

	// Add prior error context if available
	if root.Causal != nil && len(root.Causal.PriorErrors) > 0 {
		fb.Reasoning += fmt.Sprintf(" Prior errors: %v.", root.Causal.PriorErrors)
	}

	return fb, nil
}

func safeGetTrigger(c *CausalContext) string {
	if c == nil || c.TriggerEvent == "" {
		return "unknown"
	}
	return c.TriggerEvent
}
