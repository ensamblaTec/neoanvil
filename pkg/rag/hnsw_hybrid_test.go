package rag

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

func TestSearchHybridBinary_RequiresBothStorages(t *testing.T) {
	// No binary populated → must error out cleanly.
	g := synthesizeGraph(10, 64)
	pool := newTensorxPool(64)
	cpu := tensorx.NewCPUDevice(pool)
	q := make([]float32, 64)
	if _, err := g.SearchHybridBinary(context.Background(), q, 3, cpu); err == nil {
		t.Fatal("expected error when binary stage not populated")
	}
}

func TestSearchHybridBinary_RecoverRecall(t *testing.T) {
	// The two-stage re-rank should recover most of what pure-binary misses.
	// Quality bar: hybrid top-5 overlap with float32 top-5 must be ≥ the
	// overlap achieved by pure binary. On a 200-node synthetic graph the
	// re-rank over 50 candidates typically reaches 4-5/5 because the true
	// neighbours live well within the expanded candidate pool.
	const nNodes, dim = 200, 768
	g := synthesizeGraph(nNodes, dim)
	g.PopulateBinary()

	pool := newTensorxPool(dim)
	cpu := tensorx.NewCPUDevice(pool)
	ctx := context.Background()

	r := rand.New(rand.NewPCG(31, 37))
	q := make([]float32, dim)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}

	floatTop, err := g.Search(ctx, q, 5, cpu)
	if err != nil {
		t.Fatalf("float32 search failed: %v", err)
	}
	binTop, err := g.SearchBinary(ctx, q, 5)
	if err != nil {
		t.Fatalf("binary search failed: %v", err)
	}
	hybTop, err := g.SearchHybridBinary(ctx, q, 5, cpu)
	if err != nil {
		t.Fatalf("hybrid search failed: %v", err)
	}

	floatSet := make(map[uint32]bool)
	for _, id := range floatTop {
		floatSet[id] = true
	}
	binOverlap := 0
	for _, id := range binTop {
		if floatSet[id] {
			binOverlap++
		}
	}
	hybOverlap := 0
	for _, id := range hybTop {
		if floatSet[id] {
			hybOverlap++
		}
	}
	t.Logf("overlap with float32 top-5: binary=%d/5 hybrid=%d/5", binOverlap, hybOverlap)

	if hybOverlap < binOverlap {
		t.Errorf("hybrid re-rank should not do worse than binary alone (bin=%d hyb=%d)",
			binOverlap, hybOverlap)
	}
}

// BenchmarkSearch_Hybrid completes the 4-way race. Expectation: slower
// per query than pure-binary (because stage 2 adds 50 × CosineDistance
// ≈ 28 µs) but with full-precision recall.
func BenchmarkSearch_Hybrid(b *testing.B) {
	g := synthesizeGraph(1000, 768)
	g.PopulateBinary()
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
		_, err := g.SearchHybridBinary(ctx, q, 5, cpu)
		if err != nil {
			b.Fatal(err)
		}
	}
}
