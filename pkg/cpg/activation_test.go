package cpg_test

import (
	"math"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/cpg"
)

// TestActivateBasic verifies that energy propagates from seeds along EdgeCall arcs.
func TestActivateBasic(t *testing.T) {
	// Topology: A→B, A→C, B→D, C→D, D→A (cycle). Seed = A.
	g := syntheticCallGraph()
	ids := resolveIDs(t, g)

	energy := cpg.Activate(g, []cpg.NodeID{ids["A"]}, 0.5, 2)
	if energy == nil {
		t.Fatal("expected non-nil energy map")
	}
	// Seed A should have energy 1.0.
	if energy[ids["A"]] != 1.0 {
		t.Errorf("seed A energy: want 1.0, got %.4f", energy[ids["A"]])
	}
	// Direct callees (B, C) should have energy 0.5 (alpha^1).
	for _, name := range []string{"B", "C"} {
		got := energy[ids[name]]
		if math.Abs(got-0.5) > 1e-9 {
			t.Errorf("node %s energy: want 0.5, got %.4f", name, got)
		}
	}
	// D (depth 2) reachable via B or C.
	// energy[D] = energy[B] × alpha^2 = 0.5 × 0.25 = 0.125
	if math.Abs(energy[ids["D"]]-0.125) > 1e-9 {
		t.Errorf("node D energy: want 0.125, got %.4f", energy[ids["D"]])
	}
	// E has no incoming call edges → energy 0.
	if energy[ids["E"]] != 0 {
		t.Errorf("node E should have 0 energy, got %.4f", energy[ids["E"]])
	}
}

// TestActivateEmptySeeds verifies nil return on empty seed list.
func TestActivateEmptySeeds(t *testing.T) {
	g := syntheticCallGraph()
	if cpg.Activate(g, nil, 0.5, 3) != nil {
		t.Error("expected nil for empty seeds")
	}
}

// TestActivateNilGraph verifies nil return on nil graph.
func TestActivateNilGraph(t *testing.T) {
	ids := []cpg.NodeID{0}
	if cpg.Activate(nil, ids, 0.5, 3) != nil {
		t.Error("expected nil for nil graph")
	}
}

// TestNormalizeEnergy verifies max-normalization to [0,1].
func TestNormalizeEnergy(t *testing.T) {
	energy := map[cpg.NodeID]float64{0: 0.8, 1: 0.4, 2: 0.0}
	norm := cpg.NormalizeEnergy(energy)
	if norm == nil {
		t.Fatal("NormalizeEnergy returned nil")
	}
	if math.Abs(norm[0]-1.0) > 1e-9 {
		t.Errorf("max node: want 1.0, got %.4f", norm[0])
	}
	if math.Abs(norm[1]-0.5) > 1e-9 {
		t.Errorf("mid node: want 0.5, got %.4f", norm[1])
	}
	if norm[2] != 0 {
		t.Errorf("zero node: want 0.0, got %.4f", norm[2])
	}
}
