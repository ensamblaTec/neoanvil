package cpg_test

import (
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// TestCallersOf verifies BFS inverse over EdgeCall arcs.
func TestCallersOf(t *testing.T) {
	g := syntheticCallGraph() // A→B, A→C, B→D, C→D, D→A

	// Topology: A=0, B=1, C=2, D=3, E=4
	// Callers of D (id=3): B(1) and C(2)
	ids := resolveIDs(t, g)

	callers := g.CallersOf(ids["D"])
	if len(callers) != 2 {
		t.Fatalf("expected 2 callers of D, got %d: %v", len(callers), callers)
	}
	callerSet := map[string]bool{}
	for _, cid := range callers {
		if int(cid) < len(g.Nodes) {
			callerSet[g.Nodes[cid].Name] = true
		}
	}
	for _, want := range []string{"B", "C"} {
		if !callerSet[want] {
			t.Errorf("expected caller %q in callers of D, got %v", want, callerSet)
		}
	}
}

// TestReachableFrom verifies BFS forward within maxDepth hops.
func TestReachableFrom(t *testing.T) {
	g := syntheticCallGraph() // A→B, A→C, B→D, C→D, D→A
	ids := resolveIDs(t, g)

	// From A (depth=1): B, C
	r1 := g.ReachableFrom(ids["A"], 1)
	if len(r1) != 2 {
		t.Fatalf("depth=1 from A: expected 2 reachable, got %d", len(r1))
	}

	// From A (depth=2): B, C, D (B→D, C→D)
	r2 := g.ReachableFrom(ids["A"], 2)
	if len(r2) != 3 {
		t.Fatalf("depth=2 from A: expected 3 reachable, got %d", len(r2))
	}

	// From E (no outgoing edges) → 0 reachable
	r0 := g.ReachableFrom(ids["E"], 2)
	if len(r0) != 0 {
		t.Errorf("E has no callees, expected 0 reachable, got %d", len(r0))
	}
}

// TestNodeByName verifies O(1) lookup.
func TestNodeByName(t *testing.T) {
	g := syntheticCallGraph()

	id, ok := g.NodeByName("pkg", "A")
	if !ok {
		t.Fatal("NodeByName(pkg, A) returned not-found")
	}
	if int(id) >= len(g.Nodes) || g.Nodes[id].Name != "A" {
		t.Errorf("NodeByName returned wrong node: %v", g.Nodes[id])
	}

	_, ok = g.NodeByName("pkg", "Z")
	if ok {
		t.Error("expected not-found for unknown node Z")
	}
}

// resolveIDs is a helper that maps node names to NodeIDs via NodeByName.
func resolveIDs(t *testing.T, g *cpg.Graph) map[string]cpg.NodeID {
	t.Helper()
	names := []string{"A", "B", "C", "D", "E"}
	ids := make(map[string]cpg.NodeID, len(names))
	for _, name := range names {
		id, ok := g.NodeByName("pkg", name)
		if !ok {
			t.Fatalf("NodeByName(pkg, %s) not found", name)
		}
		ids[name] = id
	}
	return ids
}
