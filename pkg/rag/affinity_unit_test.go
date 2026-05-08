// pkg/rag/affinity_unit_test.go — unit tests for CPU-affinity configuration. [367.A]
package rag

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// TestSetAffinityConfig_Disabled verifies that SetAffinityConfig(false, nil) is
// a safe no-op that does not affect Search correctness.
func TestSetAffinityConfig_Disabled(t *testing.T) {
	g := synthesizeGraph(100, 32)
	g.SetAffinityConfig(false, nil)

	pool := newTensorxPool(32)
	cpu := tensorx.NewCPUDevice(pool)
	query := make([]float32, 32)
	for i := range query {
		query[i] = 0.1
	}
	ids, err := g.Search(context.Background(), query, 3, cpu)
	if err != nil {
		t.Fatalf("Search after SetAffinityConfig(false) error: %v", err)
	}
	if len(ids) == 0 {
		t.Error("Search after SetAffinityConfig(false) returned 0 results")
	}
}

// TestSetAffinityConfig_EmptyCoreList verifies that setting an empty core list
// with enabled=true does not panic — it should degrade safely to no pinning.
func TestSetAffinityConfig_EmptyCoreList(t *testing.T) {
	g := synthesizeGraph(100, 32)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SetAffinityConfig(true, []int{}) panicked: %v", r)
		}
	}()
	g.SetAffinityConfig(true, []int{})
}

// TestSetAffinityConfig_ValidCores verifies that configuring valid cores does
// not break Search — at least one result is returned.
func TestSetAffinityConfig_ValidCores(t *testing.T) {
	g := synthesizeGraph(200, 64)
	// Use core 0 only — safe on any machine (single-threaded pinning).
	g.SetAffinityConfig(true, []int{0})

	pool := newTensorxPool(64)
	cpu := tensorx.NewCPUDevice(pool)
	query := make([]float32, 64)
	for i := range query {
		query[i] = float32(i) * 0.01
	}
	ids, err := g.Search(context.Background(), query, 5, cpu)
	if err != nil {
		t.Fatalf("Search with affinity core=[0] error: %v", err)
	}
	if len(ids) == 0 {
		t.Error("Search with affinity core=[0] returned 0 results")
	}
}

// TestSetAffinityConfig_DisableAfterEnable verifies that re-disabling affinity
// after enabling it works correctly and Search still returns results.
func TestSetAffinityConfig_DisableAfterEnable(t *testing.T) {
	g := synthesizeGraph(100, 32)
	g.SetAffinityConfig(true, []int{0})
	g.SetAffinityConfig(false, nil)

	pool := newTensorxPool(32)
	cpu := tensorx.NewCPUDevice(pool)
	query := make([]float32, 32)
	ids, err := g.Search(context.Background(), query, 3, cpu)
	if err != nil {
		t.Fatalf("Search after re-disable error: %v", err)
	}
	if len(ids) == 0 {
		t.Error("Search after re-disable returned 0 results")
	}
}
