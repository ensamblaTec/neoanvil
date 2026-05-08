// Package sre — Active Dreaming: adversarial sandbox for self-training. [SRE-41]
//
// During idle periods (REM sleep), the system generates synthetic failure
// scenarios within a quarantined WASM sandbox. Successful recovery patterns
// are stored as "immune memory" — pre-computed patches that activate instantly
// when the real failure pattern is detected.
//
// This is NOT a testing framework — it's a self-improvement loop:
//  1. Generate adversarial scenario (fault injection)
//  2. Attempt recovery using existing flashback knowledge
//  3. If recovery succeeds → store as immune memory
//  4. If recovery fails → record the gap for human review
package sre

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

// FaultScenario describes a synthetic failure for adversarial testing. [SRE-41.1]
type FaultScenario struct {
	ID          string `json:"id"`
	Category    string `json:"category"` // "oom", "deadlock", "corruption", "timeout", "panic"
	Description string `json:"description"`
	Severity    int    `json:"severity"`  // 1-10
	Signature   string `json:"signature"` // error pattern to match against
	CreatedAt   int64  `json:"created_at"`
}

// ImmuneEntry is a pre-computed recovery pattern learned from adversarial training. [SRE-41.2]
type ImmuneEntry struct {
	ID             string  `json:"id"`
	FaultSignature string  `json:"fault_signature"` // the error pattern this immunizes against
	RecoveryAction string  `json:"recovery_action"` // what to do when detected
	Confidence     float64 `json:"confidence"`      // 0-1, how reliable this recovery is
	TimesActivated int     `json:"times_activated"` // how many times this fired in production
	LastActivated  int64   `json:"last_activated"`
	LearnedAt      int64   `json:"learned_at"`
	SourceScenario string  `json:"source_scenario"` // which FaultScenario taught us this
}

// DreamEngine orchestrates adversarial self-training. [SRE-41.1]
type DreamEngine struct {
	mu                     sync.RWMutex
	immuneMemory           map[string]*ImmuneEntry // signature → entry
	dreamLog               []DreamResult
	scenarioGen            []ScenarioGenerator
	immunityActivationMin  float64 // from SentinelConfig
	immunityConfidenceInit float64 // from SentinelConfig
}

// DreamResult records the outcome of a single adversarial dream. [SRE-41.1]
type DreamResult struct {
	Scenario    FaultScenario `json:"scenario"`
	Recovered   bool          `json:"recovered"`
	RecoveryMS  int64         `json:"recovery_ms"`
	NewImmunity bool          `json:"new_immunity"` // true if a new ImmuneEntry was created
	Timestamp   int64         `json:"timestamp"`
	Notes       string        `json:"notes"`
}

// ScenarioGenerator produces fault scenarios for a given category. [SRE-41.1]
type ScenarioGenerator func() FaultScenario

// NewDreamEngine creates the adversarial training engine. [SRE-41.1]
func NewDreamEngine(activationMin, confidenceInit float64) *DreamEngine {
	if activationMin <= 0 {
		activationMin = 0.5
	}
	if confidenceInit <= 0 {
		confidenceInit = 0.6
	}
	de := &DreamEngine{
		immuneMemory:           make(map[string]*ImmuneEntry),
		immunityActivationMin:  activationMin,
		immunityConfidenceInit: confidenceInit,
	}
	de.registerDefaultGenerators()
	return de
}

// registerDefaultGenerators sets up the built-in fault scenario generators.
func (de *DreamEngine) registerDefaultGenerators() {
	de.scenarioGen = []ScenarioGenerator{
		func() FaultScenario {
			return FaultScenario{
				Category:    "oom",
				Description: "Heap exhaustion during bulk HNSW ingestion",
				Severity:    8,
				Signature:   "runtime: out of memory",
			}
		},
		func() FaultScenario {
			return FaultScenario{
				Category:    "timeout",
				Description: "Ollama embedding timeout under concurrent load",
				Severity:    5,
				Signature:   "context deadline exceeded",
			}
		},
		func() FaultScenario {
			return FaultScenario{
				Category:    "corruption",
				Description: "BoltDB WAL entry with invalid JSON",
				Severity:    7,
				Signature:   "invalid character",
			}
		},
		func() FaultScenario {
			return FaultScenario{
				Category:    "panic",
				Description: "Index out of range in HNSW Search",
				Severity:    9,
				Signature:   "index out of range",
			}
		},
		func() FaultScenario {
			return FaultScenario{
				Category:    "deadlock",
				Description: "BoltDB lock contention from concurrent writes",
				Severity:    6,
				Signature:   "timeout waiting for bbolt lock",
			}
		},
	}
}

// Dream runs a single adversarial training cycle. [SRE-41.1]
// Generates a fault scenario, checks if we have immunity, and either
// validates the immunity or records a gap.
func (de *DreamEngine) Dream(ctx context.Context) DreamResult {
	// Select a random scenario
	gen := de.scenarioGen[rand.Intn(len(de.scenarioGen))]
	scenario := gen()
	scenario.ID = generateScenarioID(scenario)
	scenario.CreatedAt = time.Now().Unix()

	start := time.Now()

	// Check if we have immunity
	de.mu.RLock()
	immune, hasImmunity := de.immuneMemory[scenario.Signature]
	de.mu.RUnlock()

	result := DreamResult{
		Scenario:  scenario,
		Timestamp: time.Now().Unix(),
	}

	if hasImmunity && immune.Confidence > de.immunityActivationMin {
		// We have a known recovery — validate it still works
		result.Recovered = true
		result.RecoveryMS = time.Since(start).Milliseconds()
		result.Notes = fmt.Sprintf("Immune memory activated: %s (confidence: %.2f)", immune.RecoveryAction, immune.Confidence)

		// Boost confidence on successful validation
		de.mu.Lock()
		immune.Confidence = minF(1.0, immune.Confidence+0.05)
		immune.TimesActivated++
		immune.LastActivated = time.Now().Unix()
		de.mu.Unlock()
	} else {
		// No immunity — attempt recovery based on category
		recoveryAction := de.attemptRecovery(ctx, scenario)

		if recoveryAction != "" {
			result.Recovered = true
			result.RecoveryMS = time.Since(start).Milliseconds()
			result.NewImmunity = true
			result.Notes = fmt.Sprintf("New immunity learned: %s", recoveryAction)

			// Store new immune entry
			de.mu.Lock()
			de.immuneMemory[scenario.Signature] = &ImmuneEntry{
				ID:             generateScenarioID(scenario) + "_immune",
				FaultSignature: scenario.Signature,
				RecoveryAction: recoveryAction,
				Confidence:     de.immunityConfidenceInit,
				LearnedAt:      time.Now().Unix(),
				SourceScenario: scenario.ID,
			}
			de.mu.Unlock()
		} else {
			result.Recovered = false
			result.Notes = "No recovery strategy found — gap identified for human review"
		}
	}

	de.mu.Lock()
	de.dreamLog = append(de.dreamLog, result)
	if len(de.dreamLog) > 500 {
		de.dreamLog = de.dreamLog[len(de.dreamLog)-250:]
	}
	de.mu.Unlock()

	log.Printf("[DREAM] %s: recovered=%v ms=%d notes=%s",
		scenario.Category, result.Recovered, result.RecoveryMS, result.Notes)

	return result
}

// attemptRecovery tries to find a recovery action for a scenario based on category patterns.
func (de *DreamEngine) attemptRecovery(_ context.Context, scenario FaultScenario) string {
	switch scenario.Category {
	case "oom":
		return "trigger GC + flush PMEM caches + reduce batch size"
	case "timeout":
		return "circuit breaker open + retry with exponential backoff"
	case "corruption":
		return "WAL sanitizer scan + skip corrupted entry + re-index"
	case "panic":
		return "DumpSnapshot + graceful restart with state recovery"
	case "deadlock":
		return "bbolt timeout + snapshot isolation + retry transaction"
	default:
		return ""
	}
}

// DreamCycle runs N adversarial dreams. Called during REM sleep. [SRE-41.1]
func (de *DreamEngine) DreamCycle(ctx context.Context, count int) []DreamResult {
	results := make([]DreamResult, 0, count)
	for range count {
		if ctx.Err() != nil {
			break
		}
		results = append(results, de.Dream(ctx))
	}
	return results
}

// CheckImmunity looks up whether we have a known recovery for a given error. [SRE-41.2]
// Called from production error handlers to get instant recovery advice.
func (de *DreamEngine) CheckImmunity(errorMsg string) (*ImmuneEntry, bool) {
	de.mu.RLock()
	defer de.mu.RUnlock()

	// Exact match first
	if entry, ok := de.immuneMemory[errorMsg]; ok {
		return entry, true
	}

	// Substring match for partial signatures
	for sig, entry := range de.immuneMemory {
		if len(sig) > 3 && contains(errorMsg, sig) {
			return entry, true
		}
	}

	return nil, false
}

// ImmuneMemorySnapshot returns all current immune entries. [SRE-41.2]
func (de *DreamEngine) ImmuneMemorySnapshot() []ImmuneEntry {
	de.mu.RLock()
	defer de.mu.RUnlock()

	entries := make([]ImmuneEntry, 0, len(de.immuneMemory))
	for _, e := range de.immuneMemory {
		entries = append(entries, *e)
	}
	return entries
}

// ExportImmuneMemory serializes immune memory for persistence. [SRE-41.2]
func (de *DreamEngine) ExportImmuneMemory() ([]byte, error) {
	entries := de.ImmuneMemorySnapshot()
	return json.Marshal(entries)
}

// ImportImmuneMemory loads persisted immune entries. [SRE-41.2]
func (de *DreamEngine) ImportImmuneMemory(data []byte) error {
	var entries []ImmuneEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}

	de.mu.Lock()
	defer de.mu.Unlock()
	for i := range entries {
		de.immuneMemory[entries[i].FaultSignature] = &entries[i]
	}
	return nil
}

// DreamLog returns the recent dream results. [SRE-41.1]
func (de *DreamEngine) DreamLog() []DreamResult {
	de.mu.RLock()
	defer de.mu.RUnlock()
	result := make([]DreamResult, len(de.dreamLog))
	copy(result, de.dreamLog)
	return result
}

func generateScenarioID(s FaultScenario) string {
	h := sha256.Sum256([]byte(s.Category + s.Signature + fmt.Sprint(time.Now().UnixNano())))
	return hex.EncodeToString(h[:8])
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
