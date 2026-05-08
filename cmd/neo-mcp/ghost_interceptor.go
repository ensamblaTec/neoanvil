package main

// ghost_interceptor.go — Ghost Mode: autonomous execution of safe commands. [SRE-50]
//
// When governance.ghost_mode=true, the GhostInterceptor auto-approves MCP tool
// calls whose names match the safe_commands whitelist, increments a cycle counter,
// and forces a human checkpoint when GhostModeMaxCycles is reached.
//
// Divergence Guard: if a command attempts to write outside the active workspace,
// Ghost Mode is suspended and an alert is emitted.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// GhostInterceptor manages autonomous command execution in Ghost Mode. [SRE-50.1]
type GhostInterceptor struct {
	cfg       *config.NeoConfig
	workspace string
	cycles    atomic.Int32 // autonomous cycles since last human checkpoint
	suspended atomic.Bool  // true after divergence or cycle cap reached
}

// NewGhostInterceptor creates a GhostInterceptor bound to the given workspace.
func NewGhostInterceptor(cfg *config.NeoConfig, workspace string) *GhostInterceptor {
	return &GhostInterceptor{cfg: cfg, workspace: workspace}
}

// GhostDecision is the outcome of ShouldAutoApprove.
type GhostDecision struct {
	AutoApproved bool
	Reason       string
	CycleCount   int32
	Suspended    bool
}

// ShouldAutoApprove decides whether a command can be auto-approved in Ghost Mode. [SRE-50.1/50.3]
// Returns (true, reason) if ghost_mode is enabled, command is safe, no divergence, and under cycle cap.
// Uses LiveConfig() so governance.ghost_mode changes in neo.yaml take effect without restart.
func (g *GhostInterceptor) ShouldAutoApprove(_ context.Context, toolName, commandText string) GhostDecision {
	cfg := LiveConfig(g.cfg) // hot-reload aware
	if cfg == nil || !cfg.Governance.GhostMode {
		return GhostDecision{Reason: "ghost_mode disabled"}
	}

	if g.suspended.Load() {
		return GhostDecision{
			Suspended: true,
			Reason:    "Ghost Mode suspended — human checkpoint required before resuming",
		}
	}

	// [SRE-50.3] Cycle cap: force human checkpoint.
	maxCycles := int32(cfg.Governance.GhostModeMaxCycles)
	if maxCycles <= 0 {
		maxCycles = 50
	}
	current := g.cycles.Load()
	if current >= maxCycles {
		g.suspended.Store(true)
		log.Printf("[GHOST] Cycle cap reached (%d/%d). Suspending Ghost Mode — checkpoint required.", current, maxCycles)
		return GhostDecision{
			Suspended:  true,
			CycleCount: current,
			Reason:     fmt.Sprintf("Ghost Mode suspended: cycle cap %d reached — human checkpoint required", maxCycles),
		}
	}

	// [SRE-50.2] Divergence Guard: block writes outside workspace.
	if isDivergent(commandText, g.workspace) {
		g.suspended.Store(true)
		log.Printf("[GHOST] Divergence detected: command targets path outside workspace. Ghost Mode suspended.")
		return GhostDecision{
			Suspended: true,
			Reason:    "Ghost Mode suspended: divergence guard triggered (write outside workspace)",
		}
	}

	// Check safe_commands whitelist (via Watchdog logic).
	if !g.isSafeCommand(toolName, commandText) {
		return GhostDecision{Reason: fmt.Sprintf("command '%s' not in safe_commands whitelist", toolName)}
	}

	newCycle := g.cycles.Add(1)
	log.Printf("[GHOST] Auto-approved cycle %d/%d: %s", newCycle, maxCycles, toolName)
	return GhostDecision{
		AutoApproved: true,
		CycleCount:   newCycle,
		Reason:       fmt.Sprintf("[AUTO-APPROVED] Ghost Mode cycle %d/%d", newCycle, maxCycles),
	}
}

// Reset clears the cycle counter after a human checkpoint. [SRE-50.3]
func (g *GhostInterceptor) Reset() {
	g.cycles.Store(0)
	g.suspended.Store(false)
	log.Printf("[GHOST] Cycle counter reset — Ghost Mode resumed")
}

// CycleCount returns the current autonomous cycle count.
func (g *GhostInterceptor) CycleCount() int32 { return g.cycles.Load() }

// isSafeCommand checks if toolName or commandText prefix-matches safe_commands. [SRE-50.1]
// Uses LiveConfig() so safe_commands list changes in neo.yaml take effect without restart.
func (g *GhostInterceptor) isSafeCommand(toolName, commandText string) bool {
	safeTools := map[string]bool{
		"neo_radar":            true,
		"neo_memory_commit":    true,
		"neo_compress_context": true,
	}
	if safeTools[toolName] {
		return true
	}
	cfg := LiveConfig(g.cfg)
	lowerCmd := strings.ToLower(commandText)
	for _, safe := range cfg.SRE.SafeCommands {
		if strings.HasPrefix(lowerCmd, strings.ToLower(safe)) {
			return true
		}
	}
	return false
}

// isDivergent returns true if commandText references a path outside workspace. [SRE-50.2]
func isDivergent(commandText, workspace string) bool {
	if workspace == "" {
		return false
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return false
	}
	// Look for absolute paths in the command that are outside the workspace.
	for _, token := range strings.Fields(commandText) {
		if !filepath.IsAbs(token) {
			continue
		}
		absToken, pathErr := filepath.Abs(token)
		if pathErr != nil {
			continue
		}
		if !strings.HasPrefix(absToken, absWorkspace) {
			return true
		}
	}
	return false
}

// GhostGC removes ephemeral artifacts created during a Ghost Mode session. [SRE-58.3]
// Called on clean shutdown when ghost_mode was active.
// Removes: stale cert seals, .neo/sandbox/, .neo/tmp/ temp files.
func GhostGC(workspace string) {
	targets := []string{
		filepath.Join(workspace, ".neo", "db", "certified_state.lock"),
		filepath.Join(workspace, ".neo", "sandbox"),
		filepath.Join(workspace, ".neo", "tmp"),
	}
	for _, t := range targets {
		if err := os.RemoveAll(t); err == nil {
			log.Printf("[GHOST-GC] Removed: %s", t)
		}
	}
	log.Printf("[GHOST-GC] Post-session cleanup complete.")
}
