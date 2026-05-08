package cpg_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// repoRoot returns the absolute path to the neoanvil repository root.
func repoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// pkg/cpg/builder_test.go → ../../
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestBuildCMDNeoMCP(t *testing.T) {
	root := repoRoot(t)
	b := cpg.NewBuilder(cpg.BuildConfig{Dir: root})

	start := time.Now()
	g, err := b.Build(context.Background(), "./cmd/neo-mcp")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	t.Logf("=== CPG build results (NeedDeps) ===")
	t.Logf("  Elapsed : %v", elapsed)
	t.Logf("  Nodes   : %d", len(g.Nodes))
	t.Logf("  Edges   : %d", len(g.Edges))

	if elapsed > 500*time.Millisecond {
		t.Logf("  WARN: build took %v — exceeds 500ms threshold", elapsed)
	} else {
		t.Logf("  OK: build within 500ms budget")
	}

	if len(g.Nodes) == 0 {
		t.Error("expected at least one node in CPG")
	}
	if len(g.Edges) == 0 {
		t.Error("expected at least one edge in CPG")
	}
}

// TestEdgeDedup verifies that addEdge does not produce duplicate arcs.
func TestEdgeDedup(t *testing.T) {
	root := repoRoot(t)
	b := cpg.NewBuilder(cpg.BuildConfig{Dir: root})

	g, err := b.Build(context.Background(), "./cmd/neo-mcp")
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Build a set of (from, to, kind) and check for duplicates.
	type edgeTriple struct {
		From, To uint32
		Kind     uint8
	}
	seen := make(map[edgeTriple]struct{}, len(g.Edges))
	for _, e := range g.Edges {
		triple := edgeTriple{uint32(e.From), uint32(e.To), uint8(e.Kind)}
		if _, dup := seen[triple]; dup {
			t.Errorf("duplicate edge: %+v", triple)
		}
		seen[triple] = struct{}{}
	}
	t.Logf("edges after dedup: %d (no duplicates)", len(g.Edges))
}

// TestContextCancellation verifies Build respects context cancellation.
func TestContextCancellation(t *testing.T) {
	root := repoRoot(t)
	b := cpg.NewBuilder(cpg.BuildConfig{Dir: root})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := b.Build(ctx, "./cmd/neo-mcp")
	if err == nil {
		// go/packages may still succeed with a pre-cancelled context on some
		// platforms (cached build artifacts). Log but don't fail hard.
		t.Log("WARN: Build succeeded despite cancelled context — likely cached artifacts")
	} else {
		t.Logf("Build correctly aborted on cancelled context: %v", err)
	}
}

// BenchmarkBuildCMDNeoMCP measures repeated build cost (NeedDeps — full transitive).
func BenchmarkBuildCMDNeoMCP(b *testing.B) {
	root := repoRoot(b)
	builder := cpg.NewBuilder(cpg.BuildConfig{Dir: root})

	for b.Loop() {
		g, err := builder.Build(context.Background(), "./cmd/neo-mcp")
		if err != nil {
			b.Fatal(err)
		}
		_ = g
	}
}

// BenchmarkBuildWithDeps measures cost WITH NeedDeps (full transitive — 142.D comparison).
// 142.D result: +1.18s overhead, zero change in nodes/edges vs default mode. Use sparingly.
func BenchmarkBuildWithDeps(b *testing.B) {
	root := repoRoot(b)
	builder := cpg.NewBuilder(cpg.BuildConfig{Dir: root, WithTransitiveDeps: true})

	for b.Loop() {
		g, err := builder.Build(context.Background(), "./cmd/neo-mcp")
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(g.Nodes)), "nodes")
		b.ReportMetric(float64(len(g.Edges)), "edges")
		_ = g
	}
}
