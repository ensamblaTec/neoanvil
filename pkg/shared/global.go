// Package shared — Nexus-global shared store. [PILAR LXVII refinement via 330.L user request]
//
// Two-tier shared memory:
//
//   1. Project-shared (existing): .neo-project/db/shared.db
//      Scope: workspaces WITHIN one project (strategos + strategosia_frontend).
//      Created per-project, filesystem-anchored.
//
//   2. Nexus-global (this package): ~/.neo/shared/db/global.db
//      Scope: ALL workspaces managed by THIS Nexus installation, regardless
//      of project. Process-anchored — one Nexus = one global store.
//      Use cases: Nexus improvement notes, cross-project lessons, operator
//      preferences, meta-patterns observed across projects.
//
// The two tiers COEXIST — they're not alternatives. A workspace can write:
//   scope:"workspace" → .neo/db/knowledge.db          (local)
//   scope:"project"   → .neo-project/db/shared.db     (intra-project)
//   scope:"nexus"     → ~/.neo/shared/db/global.db    (THIS package)
//
// Reserved namespaces for Nexus-global scope:
//   improvements  — ideas/feature requests for Nexus itself
//   lessons       — cross-project patterns (seen in ≥2 projects)
//   operator      — operator preferences/shortcuts
//   upgrades      — Nexus version migration notes
//   patterns      — meta-patterns worth reusing across all managed projects
package shared

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ensamblatec/neoanvil/pkg/knowledge"
)

// Reserved namespaces for the Nexus-global tier. Created with .gitkeep at
// first boot so operators see the layout.
const (
	NSImprovements = "improvements"
	NSLessons      = "lessons"
	NSOperator     = "operator"
	NSUpgrades     = "upgrades"
	NSPatterns     = "patterns"
)

// ReservedGlobalNamespaces is the list of subdirs seeded at boot. [LXVII-refined]
func ReservedGlobalNamespaces() []string {
	return []string{NSImprovements, NSLessons, NSOperator, NSUpgrades, NSPatterns}
}

// GlobalStorePath returns the canonical path to the Nexus-global BoltDB.
// Honors NEO_SHARED_DIR env override — useful for tests. Defaults to
// ~/.neo/shared/db/global.db.
func GlobalStorePath() string {
	if override := os.Getenv("NEO_SHARED_DIR"); override != "" {
		return filepath.Join(override, "db", "global.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Fall back to /tmp if HOME is unreadable — rare, but don't crash boot.
		return "/tmp/.neo-shared/db/global.db"
	}
	return filepath.Join(home, ".neo", "shared", "db", "global.db")
}

// GlobalKnowledgeDir returns the markdown sync directory for the Nexus-global
// store, parallel to the project-level .neo-project/knowledge/. Honors the
// same NEO_SHARED_DIR override.
func GlobalKnowledgeDir() string {
	if override := os.Getenv("NEO_SHARED_DIR"); override != "" {
		return filepath.Join(override, "knowledge")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.neo-shared/knowledge"
	}
	return filepath.Join(home, ".neo", "shared", "knowledge")
}

// ErrLeaderBusy signals that another process already holds the RW flock on
// the Nexus-global store. Post-redesign, the intended owner is the Nexus
// dispatcher itself (singleton per installation) — neo-mcp children no
// longer call OpenGlobalStore. This sentinel is retained as a safety net
// so a second Nexus instance (or a misconfigured child) gets a clear error
// instead of a confusing bbolt message.
var ErrLeaderBusy = errors.New("shared: nexus-global flock held by another process")

// OpenGlobalStore opens (or creates) the Nexus-global KnowledgeStore at
// ~/.neo/shared/db/global.db and wires the markdown sync dir. Creates the
// reserved namespace subdirs with .gitkeep on first call (parallels 330.J
// for project-level). Idempotent.
//
// [354.Z-redesign] The Nexus dispatcher is the sole caller in production.
// neo-mcp children proxy tier:"nexus" operations via HTTP to Nexus
// /api/v1/shared/nexus/*. Boot order is irrelevant.
//
// Returns (nil, ErrLeaderBusy) when the flock is held (defensive — should
// never happen under single-Nexus install). Returns a non-nil error only on
// real failures (disk, permissions, corruption).
func OpenGlobalStore() (*knowledge.KnowledgeStore, error) {
	dbPath := GlobalStorePath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, fmt.Errorf("shared: mkdir db: %w", err)
	}
	ks, err := knowledge.Open(dbPath)
	if err != nil {
		if errors.Is(err, knowledge.ErrLockBusy) {
			return nil, ErrLeaderBusy
		}
		return nil, fmt.Errorf("shared: open: %w", err)
	}
	knowledgeDir := GlobalKnowledgeDir()
	if err := knowledge.EnsureSyncDir(knowledgeDir); err != nil {
		ks.Close()
		return nil, fmt.Errorf("shared: ensure sync dir: %w", err)
	}
	for _, ns := range ReservedGlobalNamespaces() {
		nsDir := filepath.Join(knowledgeDir, ns)
		if mkErr := os.MkdirAll(nsDir, 0o750); mkErr != nil {
			ks.Close()
			return nil, fmt.Errorf("shared: ensure ns %s: %w", ns, mkErr)
		}
		keep := filepath.Join(nsDir, ".gitkeep")
		if _, statErr := os.Stat(keep); os.IsNotExist(statErr) {
			if wErr := os.WriteFile(keep, nil, 0o640); wErr != nil { //nolint:gosec // G304: fixed filename under controlled path
				ks.Close()
				return nil, fmt.Errorf("shared: seed gitkeep %s: %w", ns, wErr)
			}
		}
	}
	ks.SetSyncDir(knowledgeDir)
	return ks, nil
}
