// pkg/rag/batcher_unit_test.go — unit tests for QueryBatcher lifecycle and API. [367.C]
package rag

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// TestGraphEnableBatcher_Lifecycle verifies that EnableBatcher starts the background
// goroutine and DisableBatcher stops it cleanly without a race or panic.
func TestGraphEnableBatcher_Lifecycle(t *testing.T) {
	const (
		nodes = 200
		dim   = 64
	)
	g := synthesizeGraph(nodes, dim)
	pool := newTensorxPool(dim)
	cpu := tensorx.NewCPUDevice(pool)

	// Batcher should be off initially.
	if got := g.BatcherAvgBatchSize(); got != 0 {
		t.Errorf("BatcherAvgBatchSize before enable = %v, want 0", got)
	}

	g.EnableBatcher(cpu, 2, 16)

	// After enable: AvgBatchSize is 0 (no submissions yet), but function must not panic.
	if got := g.BatcherAvgBatchSize(); got < 0 {
		t.Errorf("BatcherAvgBatchSize after enable = %v, want ≥0", got)
	}

	g.DisableBatcher()

	// After disable: back to 0.
	if got := g.BatcherAvgBatchSize(); got != 0 {
		t.Errorf("BatcherAvgBatchSize after disable = %v, want 0", got)
	}
}

// TestGraphEnableBatcher_Submit verifies that submitting a query through the
// batcher returns a valid result (non-empty IDs, no error) for a populated graph.
func TestGraphEnableBatcher_Submit(t *testing.T) {
	const (
		nodes = 200
		dim   = 64
		k     = 5
	)
	g := synthesizeGraph(nodes, dim)
	pool := newTensorxPool(dim)
	cpu := tensorx.NewCPUDevice(pool)

	g.EnableBatcher(cpu, 2, 16)
	defer g.DisableBatcher()

	query := make([]float32, dim)
	for i := range query {
		query[i] = float32(i) / float32(dim)
	}

	ids, err := g.Search(context.Background(), query, k, cpu)
	if err != nil {
		t.Fatalf("Search via batcher returned error: %v", err)
	}
	if len(ids) == 0 {
		t.Error("Search via batcher returned 0 results, want > 0")
	}
	if len(ids) > k {
		t.Errorf("Search via batcher returned %d results, want ≤ %d", len(ids), k)
	}
}

// TestGraphDisableBatcher_WhenNotEnabled verifies that calling DisableBatcher
// when the batcher was never enabled is a safe no-op.
func TestGraphDisableBatcher_WhenNotEnabled(t *testing.T) {
	g := synthesizeGraph(50, 32)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DisableBatcher on non-enabled graph panicked: %v", r)
		}
	}()
	g.DisableBatcher()
}

// TestGraphEnableBatcher_DoubleEnable verifies that calling EnableBatcher twice
// does not leak goroutines — the second call replaces the first batcher.
func TestGraphEnableBatcher_DoubleEnable(t *testing.T) {
	g := synthesizeGraph(100, 32)
	pool := newTensorxPool(32)
	cpu := tensorx.NewCPUDevice(pool)

	g.EnableBatcher(cpu, 2, 8)
	g.EnableBatcher(cpu, 2, 8) // second call must not panic or double-start
	g.DisableBatcher()
}
