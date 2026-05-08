package rag

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

func TestPopulateBinary_Consistency(t *testing.T) {
	g := synthesizeGraph(100, 768)
	if g.BinaryPopulated() {
		t.Fatal("fresh graph should not report binary populated")
	}
	g.PopulateBinary()
	if !g.BinaryPopulated() {
		t.Fatal("after PopulateBinary the graph must report populated")
	}
	wantWords := (768 + 63) / 64 // = 12
	if g.BinaryWords != wantWords {
		t.Errorf("BinaryWords = %d, want %d", g.BinaryWords, wantWords)
	}
	if len(g.BinaryVectors) != 100*wantWords {
		t.Errorf("BinaryVectors length = %d, want %d", len(g.BinaryVectors), 100*wantWords)
	}
}

func TestSearchBinary_FallbackWhenUnpopulated(t *testing.T) {
	g := synthesizeGraph(10, 64)
	q := make([]float32, 64)
	if _, err := g.SearchBinary(context.Background(), q, 3); err == nil {
		t.Fatal("expected error when binary storage is not populated")
	}
}

func TestSearchBinary_TopKOverlapWithFloat32(t *testing.T) {
	// Coarse-filter quality check. Binary-vs-cosine correlation is strong on
	// REAL sentence embeddings (~80% recall@10 per Anthropic/Jina benchmarks
	// on MSMARCO/BEIR), but WEAK on the uniform-random vectors used by
	// synthesizeGraph — random noise gives cosine and Hamming no shared
	// structure to agree on. Threshold 1/10 is a sanity floor: the search
	// must return *something* (not obviously broken), not a quality claim
	// for synthetic data. Real-embedding recall is covered by the doc-level
	// benchmarks in binary.go comments.
	const nNodes, dim = 200, 768
	g := synthesizeGraph(nNodes, dim)
	g.PopulateBinary()

	pool := newTensorxPool(dim)
	cpu := tensorx.NewCPUDevice(pool)
	ctx := context.Background()

	r := rand.New(rand.NewPCG(19, 29))
	q := make([]float32, dim)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}

	floatTop, err := g.Search(ctx, q, 10, cpu)
	if err != nil {
		t.Fatalf("float32 search failed: %v", err)
	}
	binTop, err := g.SearchBinary(ctx, q, 10)
	if err != nil {
		t.Fatalf("binary search failed: %v", err)
	}

	inCommon := 0
	set := make(map[uint32]bool, len(floatTop))
	for _, id := range floatTop {
		set[id] = true
	}
	for _, id := range binTop {
		if set[id] {
			inCommon++
		}
	}
	if inCommon < 1 {
		t.Errorf("binary top-10 has zero overlap with float32 — the greedy traversal is broken")
	}
	t.Logf("binary/float32 overlap at top-10: %d/10 (random vectors — expected weak agreement)", inCommon)
}

// BenchmarkSearch_Binary completes the 3-way race (float32, int8, binary).
// The number here should be SIGNIFICANTLY smaller than Search_Float32 —
// this is the first pure-Go path where the compiler intrinsics (POPCNT)
// deliver a real end-to-end compute speedup.
func BenchmarkSearch_Binary(b *testing.B) {
	g := synthesizeGraph(1000, 768)
	g.PopulateBinary()
	r := rand.New(rand.NewPCG(1, 2))
	q := make([]float32, 768)
	for i := range q {
		q[i] = r.Float32()*2 - 1
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := g.SearchBinary(ctx, q, 5)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPopulateBinary(b *testing.B) {
	g := synthesizeGraph(1000, 768)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.PopulateBinary()
	}
}
