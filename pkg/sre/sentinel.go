// Package sre — Sentinel Constitution: policy engine for autonomy governance. [SRE-40]
//
// A lightweight rule engine that validates whether the system is allowed to
// perform autonomous actions (daemon mode, auto-approve commands, self-healing).
// Rules are declarative and composable. Each rule receives the current system
// context and returns Allow/Deny with a reason.
//
// Architecture:
//   PolicyEngine.Evaluate(action, context) → Decision{Allow|Deny, Reason}
//   Rules are registered at boot and can be hot-reloaded from neo.yaml.
package sre

import (
	"fmt"
	"log"
	"sort"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// Decision is the result of a policy evaluation. [SRE-40.1]
type Decision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	Rule    string `json:"rule"` // which rule made the decision
}

// PolicyContext carries the current system state for policy evaluation. [SRE-40.1]
type PolicyContext struct {
	Action      string            `json:"action"`       // e.g. "auto_approve", "daemon_exec", "phoenix_protocol"
	ServerMode  string            `json:"server_mode"`  // "pair", "fast", "daemon"
	HeapMB      float64           `json:"heap_mb"`
	GCRuns      uint32            `json:"gc_runs"`
	RAPLWatts   float64           `json:"rapl_watts"`
	Goroutines  int               `json:"goroutines"`
	Uptime      time.Duration     `json:"uptime"`
	Labels      map[string]string `json:"labels"` // arbitrary key-value for rule matching
}

// PolicyRule is a single governance rule. [SRE-40.1]
type PolicyRule struct {
	Name        string                                 `json:"name"`
	Description string                                 `json:"description"`
	Priority    int                                    `json:"priority"` // higher = evaluated first
	EvalFn      func(ctx PolicyContext) *Decision       // nil Decision = rule doesn't apply
}

// PolicyEngine is the governance engine that evaluates actions against rules. [SRE-40.1]
type PolicyEngine struct {
	mu       sync.RWMutex
	rules    []PolicyRule
	auditLog []AuditEntry
	bootTime time.Time
	cfg      config.SentinelConfig
}

// AuditEntry records a policy decision for formal verification. [SRE-40.2]
type AuditEntry struct {
	Timestamp  time.Time      `json:"timestamp"`
	Action     string         `json:"action"`
	Decision   Decision       `json:"decision"`
	Context    PolicyContext   `json:"context"`
}

// NewPolicyEngine creates a new engine with the default constitution rules. [SRE-40.1]
func NewPolicyEngine(cfg config.SentinelConfig) *PolicyEngine {
	pe := &PolicyEngine{
		bootTime: time.Now(),
		cfg:      cfg,
	}
	pe.RegisterDefaultRules()
	return pe
}

// RegisterRule adds a policy rule to the engine. [SRE-40.1]
func (pe *PolicyEngine) RegisterRule(rule PolicyRule) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	pe.rules = append(pe.rules, rule)
	// Sort by priority descending
	sort.Slice(pe.rules, func(i, j int) bool {
		return pe.rules[i].Priority > pe.rules[j].Priority
	})
}

// Evaluate checks an action against all registered rules. [SRE-40.1]
// Returns the first Deny, or Allow if all rules pass.
func (pe *PolicyEngine) Evaluate(action string, labels map[string]string) Decision {
	ctx := pe.captureContext(action, labels)

	pe.mu.RLock()
	rules := make([]PolicyRule, len(pe.rules))
	copy(rules, pe.rules)
	pe.mu.RUnlock()

	for _, rule := range rules {
		if d := rule.EvalFn(ctx); d != nil {
			pe.recordAudit(ctx, *d)
			if !d.Allowed {
				log.Printf("[SENTINEL] DENY action=%s rule=%s reason=%s", action, d.Rule, d.Reason)
				return *d
			}
		}
	}

	allowed := Decision{Allowed: true, Reason: "all rules passed", Rule: "default_allow"}
	pe.recordAudit(ctx, allowed)
	return allowed
}

// captureContext snapshots current system state for policy evaluation.
func (pe *PolicyEngine) captureContext(action string, labels map[string]string) PolicyContext {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return PolicyContext{
		Action:     action,
		ServerMode: os.Getenv("NEO_SERVER_MODE"),
		HeapMB:     float64(mem.HeapAlloc) / (1024 * 1024),
		GCRuns:     mem.NumGC,
		Goroutines: runtime.NumGoroutine(),
		Uptime:     time.Since(pe.bootTime),
		Labels:     labels,
	}
}

func (pe *PolicyEngine) recordAudit(ctx PolicyContext, d Decision) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	pe.auditLog = append(pe.auditLog, AuditEntry{
		Timestamp: time.Now(),
		Action:    ctx.Action,
		Decision:  d,
		Context:   ctx,
	})

	// Keep audit log bounded
	if len(pe.auditLog) > pe.cfg.AuditLogMaxSize {
		pe.auditLog = pe.auditLog[len(pe.auditLog)-pe.cfg.AuditLogMaxSize/2:]
	}
}

// AuditLog returns the recent policy decisions for formal verification. [SRE-40.2]
func (pe *PolicyEngine) AuditLog() []AuditEntry {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	result := make([]AuditEntry, len(pe.auditLog))
	copy(result, pe.auditLog)
	return result
}

// VerifyInvariant checks a formal safety property against the audit log. [SRE-40.2]
// Returns true if the invariant holds for all entries, false with counter-example if violated.
type InvariantCheck struct {
	Name    string `json:"name"`
	Holds   bool   `json:"holds"`
	Violated int   `json:"violated"` // count of violations
	Example string `json:"example"`  // first violation description
}

// VerifyInvariants runs all formal safety checks against the audit log. [SRE-40.2]
func (pe *PolicyEngine) VerifyInvariants() []InvariantCheck {
	log := pe.AuditLog()

	checks := []InvariantCheck{
		pe.checkNoDaemonInPairMode(log),
		pe.checkNoPhoenixWithoutArm(log),
		pe.checkHeapBoundary(log),
		pe.checkNoAutoApproveHighRisk(log),
	}
	return checks
}

// checkNoDaemonInPairMode: daemon actions must never execute in pair/fast mode.
func (pe *PolicyEngine) checkNoDaemonInPairMode(entries []AuditEntry) InvariantCheck {
	ic := InvariantCheck{Name: "no_daemon_in_pair_mode", Holds: true}
	for _, e := range entries {
		if strings.HasPrefix(e.Action, "daemon_") && e.Decision.Allowed &&
			(e.Context.ServerMode == "pair" || e.Context.ServerMode == "fast") {
			ic.Holds = false
			ic.Violated++
			if ic.Example == "" {
				ic.Example = fmt.Sprintf("daemon action %q allowed in %s mode at %s",
					e.Action, e.Context.ServerMode, e.Timestamp.Format(time.RFC3339))
			}
		}
	}
	return ic
}

// checkNoPhoenixWithoutArm: phoenix protocol must never fire without SRE_PHOENIX_ARMED.
func (pe *PolicyEngine) checkNoPhoenixWithoutArm(entries []AuditEntry) InvariantCheck {
	ic := InvariantCheck{Name: "no_phoenix_without_armed", Holds: true}
	for _, e := range entries {
		if e.Action == "phoenix_protocol" && e.Decision.Allowed {
			if e.Context.Labels["phoenix_armed"] != "true" {
				ic.Holds = false
				ic.Violated++
				if ic.Example == "" {
					ic.Example = fmt.Sprintf("phoenix allowed without armed flag at %s",
						e.Timestamp.Format(time.RFC3339))
				}
			}
		}
	}
	return ic
}

// checkHeapBoundary: no auto-approve when heap > 500MB.
func (pe *PolicyEngine) checkHeapBoundary(entries []AuditEntry) InvariantCheck {
	ic := InvariantCheck{Name: "heap_boundary_500mb", Holds: true}
	for _, e := range entries {
		if e.Action == "auto_approve" && e.Decision.Allowed && e.Context.HeapMB > 500 {
			ic.Holds = false
			ic.Violated++
			if ic.Example == "" {
				ic.Example = fmt.Sprintf("auto_approve at %.0fMB heap at %s",
					e.Context.HeapMB, e.Timestamp.Format(time.RFC3339))
			}
		}
	}
	return ic
}

// checkNoAutoApproveHighRisk: destructive actions must never be auto-approved.
func (pe *PolicyEngine) checkNoAutoApproveHighRisk(entries []AuditEntry) InvariantCheck {
	ic := InvariantCheck{Name: "no_auto_approve_destructive", Holds: true}
	destructive := map[string]bool{"rm_rf": true, "drop_table": true, "force_push": true, "phoenix_protocol": true}
	for _, e := range entries {
		if destructive[e.Action] && e.Decision.Allowed {
			if e.Context.Labels["explicit_approval"] != "true" {
				ic.Holds = false
				ic.Violated++
				if ic.Example == "" {
					ic.Example = fmt.Sprintf("destructive %q auto-approved at %s",
						e.Action, e.Timestamp.Format(time.RFC3339))
				}
			}
		}
	}
	return ic
}

// ─── Default Constitution Rules [SRE-40.1] ─────────────────────────────────

// RegisterDefaultRules installs the constitutional rules that define NeoAnvil's autonomy boundaries.
func (pe *PolicyEngine) RegisterDefaultRules() {
	// Rule 1: Daemon actions are forbidden in pair/fast mode
	pe.RegisterRule(PolicyRule{
		Name:        "mode_isolation",
		Description: "Daemon actions forbidden in pair/fast mode",
		Priority:    100,
		EvalFn: func(ctx PolicyContext) *Decision {
			if !strings.HasPrefix(ctx.Action, "daemon_") {
				return nil
			}
			if ctx.ServerMode == "pair" || ctx.ServerMode == "fast" {
				return &Decision{Allowed: false, Reason: "daemon actions prohibited in " + ctx.ServerMode + " mode", Rule: "mode_isolation"}
			}
			return nil
		},
	})

	// Rule 2: Phoenix Protocol requires explicit arming
	pe.RegisterRule(PolicyRule{
		Name:        "phoenix_safety",
		Description: "Phoenix protocol requires SRE_PHOENIX_ARMED=true",
		Priority:    99,
		EvalFn: func(ctx PolicyContext) *Decision {
			if ctx.Action != "phoenix_protocol" {
				return nil
			}
			if ctx.Labels["phoenix_armed"] != "true" {
				return &Decision{Allowed: false, Reason: "phoenix protocol requires SRE_PHOENIX_ARMED=true", Rule: "phoenix_safety"}
			}
			return nil
		},
	})

	// Rule 3: Thermal throttle — deny resource-intensive actions when heap exceeds threshold
	heapThreshold := float64(pe.cfg.HeapThresholdMB)
	pe.RegisterRule(PolicyRule{
		Name:        "thermal_throttle",
		Description: fmt.Sprintf("Deny intensive actions when heap exceeds %dMB", pe.cfg.HeapThresholdMB),
		Priority:    90,
		EvalFn: func(ctx PolicyContext) *Decision {
			if ctx.HeapMB > heapThreshold && (ctx.Action == "auto_approve" || ctx.Action == "bulk_ingest" || ctx.Action == "chaos_drill") {
				return &Decision{
					Allowed: false,
					Reason:  fmt.Sprintf("heap pressure %.0fMB exceeds %dMB threshold", ctx.HeapMB, pe.cfg.HeapThresholdMB),
					Rule:    "thermal_throttle",
				}
			}
			return nil
		},
	})

	// Rule 4: Goroutine explosion guard
	goroutineLimit := pe.cfg.GoroutineExplosionLimit
	pe.RegisterRule(PolicyRule{
		Name:        "goroutine_guard",
		Description: fmt.Sprintf("Deny new work when goroutine count exceeds %d", goroutineLimit),
		Priority:    85,
		EvalFn: func(ctx PolicyContext) *Decision {
			if ctx.Goroutines > goroutineLimit && ctx.Action != "shutdown" {
				return &Decision{
					Allowed: false,
					Reason:  fmt.Sprintf("goroutine explosion: %d goroutines active", ctx.Goroutines),
					Rule:    "goroutine_guard",
				}
			}
			return nil
		},
	})

	// Rule 5: Destructive actions require explicit approval label
	pe.RegisterRule(PolicyRule{
		Name:        "destructive_guard",
		Description: "Destructive actions require explicit_approval label",
		Priority:    95,
		EvalFn: func(ctx PolicyContext) *Decision {
			destructive := map[string]bool{"rm_rf": true, "drop_table": true, "force_push": true}
			if !destructive[ctx.Action] {
				return nil
			}
			if ctx.Labels["explicit_approval"] != "true" {
				return &Decision{Allowed: false, Reason: "destructive action requires explicit approval", Rule: "destructive_guard"}
			}
			return nil
		},
	})

	// Rule 6: Uptime minimum — no autonomous actions during cold-start grace period
	coldStartGrace := time.Duration(pe.cfg.ColdStartGraceSec) * time.Second
	pe.RegisterRule(PolicyRule{
		Name:        "cold_start_guard",
		Description: fmt.Sprintf("No autonomous actions during first %ds of boot", pe.cfg.ColdStartGraceSec),
		Priority:    80,
		EvalFn: func(ctx PolicyContext) *Decision {
			if ctx.Uptime < coldStartGrace && ctx.Action == "auto_approve" {
				return &Decision{Allowed: false, Reason: "system still in cold-start phase", Rule: "cold_start_guard"}
			}
			return nil
		},
	})
}
