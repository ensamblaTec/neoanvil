package cpg

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/tools/go/ssa"
)

// ErrNotReady is returned by Graph when the background build is still running.
var ErrNotReady = errors.New("cpg: graph not ready yet")

// ManagerConfig holds runtime parameters for the Manager.
// Populate from cfg.CPG in neo.yaml — never hardcode these values.
//
// [Épica 229.4] MaxHeapMB is the legacy static limit. For hot-reloadable
// behaviour pass MaxHeapMBFn (a closure over LiveConfig) — when non-nil it
// overrides MaxHeapMB and is re-evaluated on every Graph() call. Raising
// the limit after an OOM trip immediately restores serving without a rebuild.
type ManagerConfig struct {
	PageRankIters   int
	PageRankDamping float64
	ActivationAlpha float64
	MaxHeapMB       int
	MaxHeapMBFn     func() int
}

// resolveMaxHeapMB returns the live limit, preferring the callback. [229.4]
func (c ManagerConfig) resolveMaxHeapMB() int {
	if c.MaxHeapMBFn != nil {
		return c.MaxHeapMBFn()
	}
	return c.MaxHeapMB
}

// Manager builds and serves the CPG for a workspace package.
// The build runs in a background goroutine; callers receive ErrNotReady while
// it is in progress and should degrade gracefully (e.g. fallback to grep).
type Manager struct {
	mu        sync.RWMutex
	g         *Graph
	ssaFuncs  []*ssa.Function // available after ready is closed
	buildErr  error
	cfg       ManagerConfig // [229.4] stored so Graph() can read live limit
	ready     chan struct{}  // closed when build finishes (success or failure)
	started   bool
	bootedFast bool // [263.D] true when started via LoadSnapshot, false for SSA rebuild
}

// NewManager returns an uninitialised Manager. Call Start to begin building.
func NewManager() *Manager {
	return &Manager{ready: make(chan struct{})}
}

// Start launches the CPG build in a background goroutine.
//
//   - pkgPattern:    go/packages pattern, e.g. "./cmd/neo-mcp"
//   - workspaceRoot: absolute path to the workspace root (where go.mod lives)
//   - pkgDir:        absolute path to the package directory (for mtime scanning)
//   - db:            BoltDB handle shared with neo-mcp
//   - cfg:           runtime parameters from neo.yaml
//
// Start is idempotent — subsequent calls are no-ops.
func (m *Manager) Start(ctx context.Context, pkgPattern, workspaceRoot, pkgDir string, db *bolt.DB, cfg ManagerConfig) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.cfg = cfg // [229.4] retained so Graph() can re-check the live limit.
	m.mu.Unlock()

	go m.run(ctx, pkgPattern, workspaceRoot, pkgDir, db, cfg)
}

func (m *Manager) run(ctx context.Context, pkgPattern, workspaceRoot, pkgDir string, db *bolt.DB, cfg ManagerConfig) {
	defer close(m.ready)

	log.Printf("[CPG] building workspace graph for %q…", pkgPattern)

	cache, cacheErr := NewGraphCache(db)
	if cacheErr != nil {
		log.Printf("[CPG] cache init failed: %v — building without cache", cacheErr)
	}

	var (
		g       *Graph
		err     error
		elapsed time.Duration
	)

	builder := NewBuilder(BuildConfig{Dir: workspaceRoot})

	if cache != nil {
		g, elapsed, err = builder.BuildCached(pkgPattern, pkgDir, cache)
		if err == nil && elapsed == 0 {
			log.Printf("[CPG] cache hit — %d nodes, %d edges", len(g.Nodes), len(g.Edges))
		}
	} else {
		start := time.Now()
		g, err = builder.Build(ctx, pkgPattern)
		elapsed = time.Since(start)
	}

	if err != nil {
		log.Printf("[CPG] build failed: %v", err)
		m.mu.Lock()
		m.buildErr = err
		m.mu.Unlock()
		return
	}

	// [Épica 229.4] Always store the built graph. The OOM guard is now a
	// serving-time policy gate (see Graph()) rather than a build-time
	// discard — so raising MaxHeapMB via hot-reload immediately unblocks
	// a graph that was previously over-limit, without a rebuild.
	log.Printf("[CPG] ready: %d nodes, %d edges (%v)", len(g.Nodes), len(g.Edges), elapsed)
	m.mu.Lock()
	m.g = g
	m.ssaFuncs = builder.SSAFunctions()
	m.mu.Unlock()

	// Log once at build completion if we would have tripped the legacy
	// static guard — operator visibility without the hard discard.
	if limit := cfg.resolveMaxHeapMB(); limit > 0 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		heapMB := int(ms.HeapAlloc / (1024 * 1024))
		if heapMB > limit {
			log.Printf("[CPG] heap=%dMB > limit=%dMB — graph kept in memory but serving gated until heap fits or limit raised", heapMB, limit)
		}
	}
}

// heapOOMGuard checks the live heap against the live MaxHeapMB setting.
// Returns an error when the guard trips so Graph() can deny serving without
// discarding the in-memory graph. [Épica 229.4]
func (m *Manager) heapOOMGuard() error {
	limit := m.cfg.resolveMaxHeapMB()
	if limit <= 0 {
		return nil
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	heapMB := int(ms.HeapAlloc / (1024 * 1024))
	if heapMB > limit {
		return fmt.Errorf("cpg: OOM guard — heap %dMB exceeds limit %dMB", heapMB, limit)
	}
	return nil
}

// SSAFunctions returns the SSA functions collected during the most recent build.
// Returns nil if the build is still running or failed. Used by McCabeCC callers.
func (m *Manager) SSAFunctions() []*ssa.Function {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ssaFuncs
}

// ProcessHeapMB returns the live PROCESS-WIDE HeapAlloc in MB — NOT
// CPG-specific allocation. CPG never tracked its own arena; the OOM
// guard at line 154 trips on process pressure (across HNSW + memex +
// SharedMem + dep-graph) compared against cpg.max_heap_mb. Pair with
// ProcessOOMLimitMB to reason about remaining headroom.
// [PILAR-XXVII/243.C] Renamed from CurrentHeapMB 2026-05-15 — old
// name implied CPG-only allocation which was misleading. The wire
// format (snapshot CPGHeapMB / json:"cpg_heap_mb") is kept for back-
// compat with TUI + persisted observability snapshots.
func (m *Manager) ProcessHeapMB() int {
	if m == nil {
		return 0
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int(ms.HeapAlloc / (1024 * 1024))
}

// CurrentHeapMB is the deprecated pre-2026-05-15 name. Kept as a thin
// wrapper for any out-of-tree consumer compiling against this package.
// New code should call ProcessHeapMB.
//
// Deprecated: use ProcessHeapMB. Will be removed after 2026-08-15.
func (m *Manager) CurrentHeapMB() int { return m.ProcessHeapMB() }

// HeapLimitMB returns the resolved CPG heap ceiling (static or via the
// MaxHeapMBFn closure). 0 when no limit is configured.
// ProcessOOMLimitMB returns the resolved process-wide OOM guard ceiling
// (static cfg.CPG.MaxHeapMB or the live MaxHeapMBFn closure value). 0
// when no limit is configured. Named cpg.max_heap_mb in neo.yaml for
// historic reasons — see ProcessHeapMB doc — but the threshold trips
// on TOTAL process HeapAlloc, not CPG subsystem allocation alone.
func (m *Manager) ProcessOOMLimitMB() int {
	if m == nil {
		return 0
	}
	return m.cfg.resolveMaxHeapMB()
}

// HeapLimitMB is the deprecated pre-2026-05-15 name. Kept as a thin
// wrapper for any out-of-tree consumer. New code should call
// ProcessOOMLimitMB.
//
// Deprecated: use ProcessOOMLimitMB. Will be removed after 2026-08-15.
func (m *Manager) HeapLimitMB() int { return m.ProcessOOMLimitMB() }

// Graph returns the built CPG, blocking up to timeout for the build to finish.
// Returns ErrNotReady if the build is still running after timeout.
// [Épica 229.4] The OOM guard is evaluated HERE against the live MaxHeapMB —
// so a hot-reloaded limit change takes effect immediately.
func (m *Manager) Graph(timeout time.Duration) (*Graph, error) {
	m.mu.RLock()
	g, err := m.g, m.buildErr
	m.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if g != nil {
		if guardErr := m.heapOOMGuard(); guardErr != nil {
			return nil, guardErr
		}
		return g, nil
	}

	select {
	case <-m.ready:
		m.mu.RLock()
		g, err = m.g, m.buildErr
		m.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		if g != nil {
			if guardErr := m.heapOOMGuard(); guardErr != nil {
				return nil, guardErr
			}
		}
		return g, nil
	case <-time.After(timeout):
		return nil, ErrNotReady
	}
}

// BootedFast returns true when the Manager was populated via LoadSnapshot rather
// than a full SSA rebuild. Used by BRIEFING to show cpg_boot mode. [Épica 263.D]
func (m *Manager) BootedFast() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bootedFast
}

// LoadSnapshot injects a pre-loaded Graph into the Manager, marking it as ready.
// Used by the fast-boot path to skip SSA rebuild when a fresh cpg.bin exists. [Épica 263.A]
func (m *Manager) LoadSnapshot(g *Graph) {
	m.mu.Lock()
	m.g = g
	m.started = true
	m.bootedFast = true
	m.mu.Unlock()
	// Signal ready without running the builder goroutine.
	select {
	case <-m.ready:
	default:
		close(m.ready)
	}
	log.Printf("[CPG] fast-boot: %d nodes restored from snapshot", len(g.Nodes))
}

// SaveSnapshot serializes the current Graph to path if the build is ready. [Épica 263.B]
func (m *Manager) SaveSnapshot(path string) {
	m.mu.RLock()
	g := m.g
	m.mu.RUnlock()
	if g == nil {
		return
	}
	if err := SaveCPG(g, path); err != nil {
		log.Printf("[CPG] snapshot save failed: %v", err)
		return
	}
	log.Printf("[CPG] snapshot saved: %d nodes to %s", len(g.Nodes), path)
}

// Status returns "building", "ready", or "error".
func (m *Manager) Status() string {
	select {
	case <-m.ready:
		m.mu.RLock()
		defer m.mu.RUnlock()
		if m.buildErr != nil {
			return "error"
		}
		return "ready"
	default:
		return "building"
	}
}
