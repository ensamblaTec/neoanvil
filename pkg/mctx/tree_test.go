package mctx

import (
	"sync"
	"testing"
)

func TestTree_ApplyVirtualLoss_ConcurrentCAS(t *testing.T) {
	const workers = 100
	arena := make([]Node, 2)
	tree := NewTree(arena)

	tree.nextNodeIdx.Store(2)
	tree.Nodes[1].Parent = 0

	initialVisits, initialScore := unpackState(tree.Nodes[1].state.Load())
	if initialVisits != 0 || initialScore != 0.0 {
		t.Fatalf("initial state mismatch: visits=%d, score=%.2f", initialVisits, initialScore)
	}

	var wg sync.WaitGroup
	wg.Add(workers)

	for i := range workers {
		go func(id int) {
			defer wg.Done()
			tree.ApplyVirtualLoss(1)
		}(i)
	}

	wg.Wait()

	finalVisits, finalScore := unpackState(tree.Nodes[1].state.Load())

	if finalVisits != workers {
		t.Errorf("CAS data loss detected: visits=%d, want %d", finalVisits, workers)
	}

	expectedScore := -float32(workers) * VirtualLoss
	if finalScore != expectedScore {
		t.Errorf("CAS data loss detected: score=%.2f, want %.2f", finalScore, expectedScore)
	}
}

func TestTree_AddChild(t *testing.T) {
	arena := make([]Node, 10)
	tree := NewTree(arena)

	childIdx, err := tree.AddChild(0)
	if err != nil {
		t.Fatalf("addChild failed: %v", err)
	}
	if childIdx != 1 {
		t.Errorf("expected child index 1, got %d", childIdx)
	}
	if tree.Nodes[childIdx].Parent != 0 {
		t.Errorf("child parent should be 0, got %d", tree.Nodes[childIdx].Parent)
	}
}

func TestTree_AddChild_OOM(t *testing.T) {
	arena := make([]Node, 2)
	tree := NewTree(arena)

	_, err := tree.AddChild(0)
	if err != nil {
		t.Fatalf("first addChild should succeed: %v", err)
	}

	_, err = tree.AddChild(0)
	if err == nil {
		t.Fatal("expected OOM error on arena exhaustion")
	}
}

func TestPackUnpackState_Roundtrip(t *testing.T) {
	tests := []struct {
		visits uint32
		score  float32
	}{
		{0, 0.0},
		{1, 1.0},
		{100, -100.0},
		{1000000, 3.14159},
		{0xFFFFFFFF, -0.001},
	}

	for _, tt := range tests {
		packed := packState(tt.visits, tt.score)
		gotVisits, gotScore := unpackState(packed)

		if gotVisits != tt.visits {
			t.Errorf("visits roundtrip failed: packed(%d, %.5f) → %d", tt.visits, tt.score, gotVisits)
		}
		if gotScore != tt.score {
			t.Errorf("score roundtrip failed: packed(%d, %.5f) → %.5f", tt.visits, tt.score, gotScore)
		}
	}
}
