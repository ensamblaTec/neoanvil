package rag

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/memx"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// synthesizeGraph builds a toy HNSW graph with nNodes nodes of dim=dim,
// each fully connected to the previous 4 neighbors (ring-with-skip topology
// keeps the traversal non-trivial without needing the real HNSW insert logic).
// Deterministic seed so benchmarks are reproducible.
func synthesizeGraph(nNodes, dim int) *Graph {
	g := NewGraph(nNodes, nNodes*4, dim)
	r := rand.New(rand.NewPCG(42, 99))
	g.Vectors = make([]float32, nNodes*dim)
	for i := range g.Vectors {
		g.Vectors[i] = r.Float32()*2 - 1 // range [-1, 1]
	}
	g.Nodes = make([]Node, nNodes)
	g.Edges = make([]uint32, 0, nNodes*4)
	for i := range nNodes {
		g.Nodes[i] = Node{
			EdgesOffset: uint32(len(g.Edges)),
			EdgesLength: 4,
		}
		// Connect to 4 neighbors (wrap-around).
		for d := 1; d <= 4; d++ {
			g.Edges = append(g.Edges, uint32((i+d)%nNodes))
		}
	}
	return g
}

func TestPopulateInt8_Consistency(t *testing.T) {
	g := synthesizeGraph(100, 768)
	if g.Int8Populated() {
		t.Fatal("fresh graph should not report int8 populated")
	}
	g.PopulateInt8()
	if !g.Int8Populated() {
		t.Fatal("after PopulateInt8 the graph must report populated")
	}
	if len(g.Int8Vectors) != 100*768 {
		t.Errorf("Int8Vectors length = %d, want %d", len(g.Int8Vectors), 100*768)
	}
	if len(g.Int8Scales) != 100 {
		t.Errorf("Int8Scales length = %d, want %d", len(g.Int8Scales), 100)
	}
	// Every node with non-zero float32 content must have a positive scale.
	for i := range g.Int8Scales {
		if g.Int8Scales[i] <= 0 {
			t.Errorf("node %d: scale must be positive, got %v", i, g.Int8Scales[i])
		}
	}
}

func TestSearchInt8_FallbackWhenUnpopulated(t *testing.T) {
	g := synthesizeGraph(10, 64)
	q := make([]float32, 64)
	_, err := g.SearchInt8(context.Background(), q, 3)
	if err == nil {
		t.Fatal("expected error when int8 storage is not populated")
	}
}

func TestSearchInt8_RanksOverlapWithFloat32(t *testing.T) {
	// Correctness check: on a small synthetic graph, the int8 top-5 should
	// overlap substantially with the float32 top-5. Exact match is not
	// required (int8 rounding alters distances), but any result in common
	// indicates the ranking mechanism is intact.
	const nNodes, dim = 200, 768
	g := synthesizeGraph(nNodes, dim)
	g.PopulateInt8()

	pool := newTensorxPool(dim)
	cpu := tensorx.NewCPUDevice(pool)
	ctx := context.Background()

	r := rand.New(rand.NewPCG(7, 13))
	q := make([]float32, dim)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}

	floatTop, err := g.Search(ctx, q, 5, cpu)
	if err != nil {
		t.Fatalf("float32 search failed: %v", err)
	}
	int8Top, err := g.SearchInt8(ctx, q, 5)
	if err != nil {
		t.Fatalf("int8 search failed: %v", err)
	}

	inCommon := 0
	set := make(map[uint32]bool, len(floatTop))
	for _, id := range floatTop {
		set[id] = true
	}
	for _, id := range int8Top {
		if set[id] {
			inCommon++
		}
	}
	// Allow some drift — HNSW traversal can diverge when distance values
	// are tied after quantization. 2 of 5 is a conservative lower bound.
	if inCommon < 2 {
		t.Errorf("int8 top-5 only overlaps %d of 5 with float32 (too divergent): float=%v int8=%v",
			inCommon, floatTop, int8Top)
	}
}

// newTensorxPool constructs a minimal float32 slab pool for the tensorx
// CPU device. Mirrors the internal helper used by pkg/tensorx tests —
// duplicated locally so pkg/rag does not depend on tensorx test internals.
func newTensorxPool(capacity int) *memx.ObservablePool[memx.F32Slab] {
	return memx.NewObservablePool(
		func() *memx.F32Slab { return &memx.F32Slab{Data: make([]float32, 0, capacity)} },
		func(s *memx.F32Slab) { s.Data = s.Data[:0] },
		capacity,
	)
}

// BenchmarkSearch_Float32 measures the baseline HNSW search latency on a
// 1000-node synthetic graph, dim=768. [171.C]
func BenchmarkSearch_Float32(b *testing.B) {
	g := synthesizeGraph(1000, 768)
	pool := newTensorxPool(768)
	cpu := tensorx.NewCPUDevice(pool)
	r := rand.New(rand.NewPCG(1, 2))
	q := make([]float32, 768)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := g.Search(ctx, q, 5, cpu)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSearch_Int8 is the int8 analog — same graph, same query, int8 path.
func BenchmarkSearch_Int8(b *testing.B) {
	g := synthesizeGraph(1000, 768)
	g.PopulateInt8()
	r := rand.New(rand.NewPCG(1, 2))
	q := make([]float32, 768)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := g.SearchInt8(ctx, q, 5)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPopulateInt8 measures the cost of converting the full float32
// vector store to int8 — typically a one-time cost at boot.
func BenchmarkPopulateInt8(b *testing.B) {
	g := synthesizeGraph(1000, 768)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.PopulateInt8()
	}
}

// BenchmarkRecallParity measures recall@10 of int8 vs float32 HNSW search on
// a 1000-node synthetic graph (dim=768, 100 fixed queries). Reports recall@10
// via b.ReportMetric and fails if below 0.95 — the production-readiness gate
// for int8 (Épica 327.A). Run: go test -bench=BenchmarkRecallParity -benchtime=1x ./pkg/rag/
func BenchmarkRecallParity(b *testing.B) {
	const (
		nNodes   = 1000
		dim      = 768
		k        = 10
		nQueries = 100
		target   = 0.95
	)
	g := synthesizeGraph(nNodes, dim)
	g.PopulateInt8()
	pool := newTensorxPool(dim)
	cpu := tensorx.NewCPUDevice(pool)
	rng := rand.New(rand.NewPCG(42, 99))
	queries := make([][]float32, nQueries)
	for i := range queries {
		q := make([]float32, dim)
		for j := range q {
			q[j] = rng.Float32()*2 - 1
		}
		queries[i] = q
	}
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		hits := 0
		for _, q := range queries {
			f32, err := g.Search(ctx, q, k, cpu)
			if err != nil {
				b.Fatal(err)
			}
			i8, err := g.SearchInt8(ctx, q, k)
			if err != nil {
				b.Fatal(err)
			}
			f32set := make(map[uint32]bool, k)
			for _, id := range f32 {
				f32set[id] = true
			}
			for _, id := range i8 {
				if f32set[id] {
					hits++
				}
			}
		}
		recall := float64(hits) / float64(nQueries*k)
		b.ReportMetric(recall, "recall@10")
		if recall < target {
			b.Errorf("recall@10=%.4f below %.2f threshold — int8 not production candidate; skip 327.B/C", recall, target)
		}
	}
}
