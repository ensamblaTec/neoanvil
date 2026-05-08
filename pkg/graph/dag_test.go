package graph

import (
	"testing"
)

func TestTarjanSCC_CycleDetection(t *testing.T) {
	dag := NewDAG()
	// A -> B -> C -> A
	dag.AddEdge("A", "B")
	dag.AddEdge("B", "C")
	dag.AddEdge("C", "A")

	// D -> E
	dag.AddEdge("D", "E")

	sccs := dag.TarjanSCC()

	hasCycle := false
	for _, scc := range sccs {
		if len(scc) > 1 {
			hasCycle = true
			if len(scc) != 3 {
				t.Errorf("Expected cycle of length 3, got %d", len(scc))
			}
		}
	}

	if !hasCycle {
		t.Error("TarjanSCC failed to detect the cycle: A -> B -> C -> A")
	}
}

func TestTarjanSCC_NoCycle(t *testing.T) {
	dag := NewDAG()
	dag.AddEdge("A", "B")
	dag.AddEdge("B", "C")
	dag.AddEdge("D", "C")

	sccs := dag.TarjanSCC()
	for _, scc := range sccs {
		if len(scc) > 1 {
			t.Errorf("Detected false cycle in acyclic graph: %v", scc)
		}
	}
}
