// pkg/rag/bench_batcher_test.go — burst-throughput benchmarks for Épica 367.C.
// Gate: BenchmarkHNSWBurst_WithBatch throughput >= 2× BenchmarkHNSWBurst_NoBatch
// for 100 concurrent queries (measured as total wall-time for the burst).
package rag

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

const (
	burstNodes = 1000
	burstDim   = 768
	burstN     = 100 // concurrent queries per burst
)

// burstQueries launches burstN goroutines, each performing one graph.Search,
// and waits for all to complete. Returns total elapsed nanoseconds.
func burstQueries(g *Graph, cpu tensorx.ComputeDevice, queries [][]float32) {
	var wg sync.WaitGroup
	wg.Add(burstN)
	for i := range burstN {
		go func(qi int) {
			defer wg.Done()
			_, _ = g.Search(context.Background(), queries[qi], 5, cpu)
		}(i)
	}
	wg.Wait()
}

// BenchmarkHNSWBurst_NoBatch is the baseline: burstN concurrent goroutines,
// each calling Graph.Search independently. [367.C]
func BenchmarkHNSWBurst_NoBatch(b *testing.B) {
	g := synthesizeGraph(burstNodes, burstDim)
	pool := newTensorxPool(burstDim)
	cpu := tensorx.NewCPUDevice(pool)
	r := rand.New(rand.NewPCG(11, 17))
	queries := make([][]float32, burstN)
	for i := range burstN {
		queries[i] = make([]float32, burstDim)
		for j := range burstDim {
			queries[i][j] = r.Float32()*2 - 1
		}
	}
	b.ResetTimer()
	for b.Loop() {
		burstQueries(g, cpu, queries)
	}
}

// BenchmarkHNSWBurst_WithBatch runs the same burst through the QueryBatcher
// (2ms window, maxSize 32). The batcher's pinned goroutine amortizes
// LockOSThread + dispatch overhead across queries in the window. [367.C]
func BenchmarkHNSWBurst_WithBatch(b *testing.B) {
	g := synthesizeGraph(burstNodes, burstDim)
	pool := newTensorxPool(burstDim)
	cpu := tensorx.NewCPUDevice(pool)
	g.EnableBatcher(cpu, 2, 32)
	defer g.DisableBatcher()

	r := rand.New(rand.NewPCG(11, 17))
	queries := make([][]float32, burstN)
	for i := range burstN {
		queries[i] = make([]float32, burstDim)
		for j := range burstDim {
			queries[i][j] = r.Float32()*2 - 1
		}
	}
	b.ResetTimer()
	for b.Loop() {
		burstQueries(g, cpu, queries)
	}
}
