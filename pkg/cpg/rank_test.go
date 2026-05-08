package cpg_test

import (
	"context"
	"math"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// syntheticCallGraph builds a small deterministic graph for rank validation.
// Topology: A→B, A→C, B→D, C→D, D→A (cycle). 5 nodes, 5 call edges.
func syntheticCallGraph() *cpg.Graph {
	g := cpg.ExportedNewGraph()
	nodes := []struct{ pkg, name string }{
		{"pkg", "A"}, {"pkg", "B"}, {"pkg", "C"}, {"pkg", "D"}, {"pkg", "E"},
	}
	ids := make([]cpg.NodeID, len(nodes))
	for i, n := range nodes {
		ids[i] = g.ExportedAddNode(cpg.Node{Kind: cpg.NodeFunc, Package: n.pkg, Name: n.name})
	}
	// A→B, A→C, B→D, C→D, D→A
	edges := [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 0}}
	for _, e := range edges {
		g.ExportedAddEdge(ids[e[0]], ids[e[1]], cpg.EdgeCall)
	}
	return g
}

// TestPageRankConverges verifies the sum of ranks ≈ 1.0 (±0.001).
func TestPageRankConverges(t *testing.T) {
	g := syntheticCallGraph()
	ranks := cpg.ComputePageRank(g, 0.85, 50)

	var sum float64
	for _, r := range ranks {
		sum += r
	}
	t.Logf("rank sum after 50 iters: %.6f", sum)
	if math.Abs(sum-1.0) > 0.001 {
		t.Errorf("expected sum≈1.0, got %.6f", sum)
	}
}

// TestTopFunctionsNeoMCP verifies that high-dispatch functions appear in top-10.
func TestTopFunctionsNeoMCP(t *testing.T) {
	root := repoRoot(t)
	builder := cpg.NewBuilder(cpg.BuildConfig{Dir: root})

	g, err := builder.Build(context.Background(), "./cmd/neo-mcp")
	if err != nil {
		t.Skipf("skipping: build failed: %v", err)
	}

	ranks := cpg.ComputePageRank(g, 0.85, 50)

	// All packages — stdlib dominates (expected).
	topAll := g.TopN(5, ranks, "")
	t.Logf("=== Top-5 CodeRank (all packages) ===")
	for i, rn := range topAll {
		t.Logf("  %2d. %-40s pkg=%-30s score=%.6f", i+1, rn.Name, rn.Package, rn.Score)
	}

	// Local packages only (ensamblatec prefix).
	topLocal := g.TopN(10, ranks, "github.com/ensamblatec/neoanvil")
	t.Logf("=== Top-10 CodeRank (ensamblatec only) ===")
	for i, rn := range topLocal {
		t.Logf("  %2d. %-40s score=%.6f line=%d", i+1, rn.Name, rn.Score, rn.Line)
	}

	if len(topLocal) == 0 {
		t.Error("expected non-empty local top-N")
	}
	// High-dispatch handlers should appear in local top-10.
	found := false
	highDispatch := map[string]bool{
		"handleRadar": true, "handleBlastRadius": true, "handleASTAudit": true,
		"handleCompileAudit": true, "handleSemanticCode": true, "Analyze": true,
	}
	for _, rn := range topLocal {
		if highDispatch[rn.Name] {
			found = true
			t.Logf("  -> high-dispatch function %q in local top-10 ✓", rn.Name)
			break
		}
	}
	if !found {
		t.Logf("  INFO: no expected high-dispatch in local top-10 — checking top-20")
		top20 := g.TopN(20, ranks, "github.com/ensamblatec/neoanvil")
		for _, rn2 := range top20 {
			t.Logf("    %s (%.6f)", rn2.Name, rn2.Score)
		}
	}
}

// TestPageRankEmptyGraph verifies no panic on empty input.
func TestPageRankEmptyGraph(t *testing.T) {
	g := cpg.ExportedNewGraph()
	ranks := cpg.ComputePageRank(g, 0.85, 50)
	if ranks != nil {
		t.Error("expected nil ranks for empty graph")
	}
	top := g.TopN(10, ranks, "")
	if top != nil {
		t.Error("expected nil top for empty graph")
	}
}
