package cpg_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

func TestManagerReady(t *testing.T) {
	root := repoRoot(t)
	db := openTestDB(t)

	m := cpg.NewManager()
	cfg := cpg.ManagerConfig{MaxHeapMB: 500} // generous limit for test
	m.Start(context.Background(), "./cmd/neo-mcp", root, filepath.Join(root, "cmd", "neo-mcp"), db, cfg)

	// Status should be "building" immediately after Start.
	status := m.Status()
	if status != "building" && status != "ready" {
		t.Errorf("unexpected initial status: %q", status)
	}

	// Graph(5s) blocks until ready or timeout.
	g, err := m.Graph(5 * time.Second)
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if len(g.Nodes) == 0 {
		t.Error("expected non-empty graph")
	}
	t.Logf("Manager ready: %d nodes, %d edges", len(g.Nodes), len(g.Edges))

	// After ready, Status must report "ready".
	if s := m.Status(); s != "ready" {
		t.Errorf("expected status=ready after build, got %q", s)
	}

	// Subsequent Graph calls are instant (no blocking).
	start := time.Now()
	_, _ = m.Graph(100 * time.Millisecond)
	if time.Since(start) > 5*time.Millisecond {
		t.Errorf("second Graph() call took too long: %v", time.Since(start))
	}
}

func TestManagerIdempotentStart(t *testing.T) {
	root := repoRoot(t)
	db := openTestDB(t)
	m := cpg.NewManager()
	cfg := cpg.ManagerConfig{MaxHeapMB: 500}
	pkgDir := filepath.Join(root, "cmd", "neo-mcp")

	// Call Start twice — second call must be a no-op (no panic, no double build).
	m.Start(context.Background(), "./cmd/neo-mcp", root, pkgDir, db, cfg)
	m.Start(context.Background(), "./cmd/neo-mcp", root, pkgDir, db, cfg)

	g, err := m.Graph(5 * time.Second)
	if err != nil {
		t.Fatalf("Graph after double Start: %v", err)
	}
	if len(g.Nodes) == 0 {
		t.Error("expected non-empty graph")
	}
}

func TestManagerTimeout(t *testing.T) {
	// Use a cancelled context so Build will fail immediately, then check ErrNotReady
	// via a very short timeout on Graph().
	root := repoRoot(t)
	db := openTestDB(t)
	m := cpg.NewManager()
	cfg := cpg.ManagerConfig{MaxHeapMB: 500}

	// Use a context that cancels after we call Start but before build finishes.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	m.Start(ctx, "./cmd/neo-mcp", root, filepath.Join(root, "cmd", "neo-mcp"), db, cfg)

	// Graph with 0 timeout must return ErrNotReady or build error immediately.
	_, err := m.Graph(0)
	// Either ErrNotReady (build still running) or context error (build aborted).
	if err == nil {
		t.Error("expected an error with 0-timeout Graph call on cancelled context")
	}
	t.Logf("Got expected error: %v", err)
}
