// pkg/rag/perf_bench_test.go — Benchmarks for Épicas 305.D + 306.A.3 + 306.B.2 + 366.A
package rag

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

func randomVec(n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = rand.Float32()*2 - 1
	}
	return v
}

func BenchmarkCosineScalar(b *testing.B) {
	a, v := randomVec(768), randomVec(768)
	b.ResetTimer()
	for b.Loop() {
		_ = cosineScalar(a, v)
	}
}

func BenchmarkCosineDispatch(b *testing.B) {
	a, v := randomVec(768), randomVec(768)
	b.ResetTimer()
	for b.Loop() {
		_ = cosineF32(a, v)
	}
}

func BenchmarkAlignedAlloc(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = alignedFloat32Slice(768)
	}
}

// BenchmarkVectorAccessUnaligned measures sequential cosine reads from a standard (unaligned)
// float32 buffer — simulates Graph.Vectors BEFORE 306.A.2 fix. [306.A.3]
func BenchmarkVectorAccessUnaligned(b *testing.B) {
	const dim, count = 768, 1000
	buf := make([]float32, dim*count) // unaligned (Go default allocator)
	for idx := range buf {
		buf[idx] = float32(idx) * 0.001
	}
	query := randomVec(dim)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		var acc float32
		for i := range count {
			acc += cosineScalar(query, buf[i*dim:i*dim+dim])
		}
		_ = acc
	}
}

// BenchmarkGraphConcurrentGen measures contention on Graph.Gen under mixed
// reader+writer load. [366.A] With //go:align 64 on Graph the struct starts at a
// cache-line boundary so Gen (at its natural offset) does not share a line with
// unrelated data from adjacent heap objects. Run with -cpu=1,2,4,8 to verify
// scalability does not degrade.
func BenchmarkGraphConcurrentGen(b *testing.B) {
	g := NewGraph(1024, 8192, 768)
	var readers atomic.Int64
	readers.Store(0)

	b.RunParallel(func(pb *testing.PB) {
		r := readers.Add(1)
		// Odd workers simulate InsertBatch (Gen.Add); even workers simulate QueryCache (Gen.Load).
		if r%2 == 0 {
			for pb.Next() {
				_ = g.Gen.Load()
			}
		} else {
			for pb.Next() {
				g.Gen.Add(1)
			}
		}
	})
}

// BenchmarkSearchStatePool measures throughput of the searchStatePool under
// concurrent Acquire+Release. [366.A] //go:align 64 on SearchState prevents
// adjacent pool slots from sharing cache lines across goroutines.
func BenchmarkSearchStatePool(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := searchStatePool.Get().(*SearchState)
			clear(s.Visited)
			s.Results = s.Results[:0]
			s.IDs = s.IDs[:0]
			searchStatePool.Put(s)
		}
	})
}

// BenchmarkSyncPoolCacheLine measures throughput of sync.Pool get/put under
// parallel goroutines to stress atomic counter cache lines. [366.A]
func BenchmarkSyncPoolCacheLine(b *testing.B) {
	type buf struct{ data [256]byte }
	pool := &sync.Pool{New: func() any { return new(buf) }}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			v := pool.Get().(*buf)
			pool.Put(v)
		}
	})
}

// BenchmarkVectorAccessAligned measures sequential cosine reads from a 64-byte-aligned buffer
// — simulates Graph.Vectors AFTER 306.A.2 fix. Expect lower ns/op when dim*4 ≡ 0 (mod 64). [306.A.3]
func BenchmarkVectorAccessAligned(b *testing.B) {
	const dim, count = 768, 1000
	buf := alignedFloat32Slice(dim * count) // 64-byte aligned (306.A)
	for idx := range buf {
		buf[idx] = float32(idx) * 0.001
	}
	query := randomVec(dim)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		var acc float32
		for i := range count {
			acc += cosineScalar(query, buf[i*dim:i*dim+dim])
		}
		_ = acc
	}
}
