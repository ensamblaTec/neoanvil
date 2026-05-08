// pkg/rag/benchmarks_affinity_test.go — CPU affinity benchmarks for Épica 367.A.
// Gate: FullAffinity.p99 <= LockOSThreadOnly.p99 * 0.97 AND ops_sec >= LockOSThreadOnly * 1.02
package rag

import (
	"context"
	"math/rand/v2"
	"os"
	"runtime"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// BenchmarkHNSWSearch_NoAffinity measures baseline Search with no thread pinning. [367.A]
func BenchmarkHNSWSearch_NoAffinity(b *testing.B) {
	g := synthesizeGraph(1000, 768)
	pool := newTensorxPool(768)
	cpu := tensorx.NewCPUDevice(pool)
	r := rand.New(rand.NewPCG(7, 13))
	q := make([]float32, 768)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = g.Search(ctx, q, 5, cpu)
		}
	})
}

// BenchmarkHNSWSearch_LockOSThreadOnly measures Search with runtime.LockOSThread
// but no SchedSetaffinity — isolates the overhead of OS-thread locking alone. [367.A]
func BenchmarkHNSWSearch_LockOSThreadOnly(b *testing.B) {
	g := synthesizeGraph(1000, 768)
	pool := newTensorxPool(768)
	cpu := tensorx.NewCPUDevice(pool)
	r := rand.New(rand.NewPCG(7, 13))
	q := make([]float32, 768)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		for pb.Next() {
			_, _ = g.Search(ctx, q, 5, cpu)
		}
	})
}

// BenchmarkHNSWSearch_FullAffinity measures Search with full CPU affinity enabled. [367.A]
// On Linux calls SchedSetaffinity per-search; on Darwin falls back to LockOSThread.
// Skip with NEO_CPU_AFFINITY=off.
func BenchmarkHNSWSearch_FullAffinity(b *testing.B) {
	if os.Getenv("NEO_CPU_AFFINITY") == "off" {
		b.Skip("NEO_CPU_AFFINITY=off")
	}
	g := synthesizeGraph(1000, 768)
	g.SetAffinityConfig(true, []int{0, 1, 2, 3})
	pool := newTensorxPool(768)
	cpu := tensorx.NewCPUDevice(pool)
	r := rand.New(rand.NewPCG(7, 13))
	q := make([]float32, 768)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = g.Search(ctx, q, 5, cpu)
		}
	})
}
