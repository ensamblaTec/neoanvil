package rag

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/memx"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

func TestGraph_Search_GreedyFindsClosest(t *testing.T) {
	const dim = 3
	g := NewGraph(4, 8, dim)

	g.AddNode(Node{DocID: 100, EdgesOffset: 0, EdgesLength: 2, Layer: 0})

	g.AddNode(Node{DocID: 101, EdgesOffset: 2, EdgesLength: 2, Layer: 0})

	g.AddNode(Node{DocID: 102, EdgesOffset: 4, EdgesLength: 2, Layer: 0})

	g.AddNode(Node{DocID: 103, EdgesOffset: 6, EdgesLength: 2, Layer: 0})

	g.Vectors = []float32{
		1, 0, 0,
		0.5, 0.5, 0,
		0, 0.5, 0.5,
		0.57, 0.57, 0.57,
	}

	g.Edges = []uint32{
		1, 2,
		0, 3,
		0, 3,
		1, 2,
	}

	pool := memx.NewObservablePool(
		func() *memx.F32Slab { return &memx.F32Slab{Data: make([]float32, 0, 64)} },
		func(s *memx.F32Slab) { s.Data = s.Data[:0] },
		64,
	)
	cpu := tensorx.NewCPUDevice(pool)

	query := []float32{0.6, 0.6, 0.6}

	results, err := g.Search(context.Background(), query, 1, cpu)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if results[0] != 3 {
		t.Errorf("expected node 3 (closest to diagonal query), got node %d", results[0])
	}
}

func TestGraph_Search_TopK(t *testing.T) {
	const dim = 2
	g := NewGraph(3, 4, dim)

	g.AddNode(Node{DocID: 0, EdgesOffset: 0, EdgesLength: 1, Layer: 0})
	g.AddNode(Node{DocID: 1, EdgesOffset: 1, EdgesLength: 2, Layer: 0})
	g.AddNode(Node{DocID: 2, EdgesOffset: 3, EdgesLength: 1, Layer: 0})

	g.Vectors = []float32{
		1, 0,
		0.7, 0.7,
		0, 1,
	}

	g.Edges = []uint32{
		1,
		0, 2,
		1,
	}

	pool := memx.NewObservablePool(
		func() *memx.F32Slab { return &memx.F32Slab{Data: make([]float32, 0, 64)} },
		func(s *memx.F32Slab) { s.Data = s.Data[:0] },
		64,
	)
	cpu := tensorx.NewCPUDevice(pool)

	query := []float32{0.65, 0.75}
	results, err := g.Search(context.Background(), query, 3, cpu)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results for topK=3, got %d", len(results))
	}
}

func TestGraph_Search_EmptyGraph(t *testing.T) {
	g := NewGraph(0, 0, 3)

	pool := memx.NewObservablePool(
		func() *memx.F32Slab { return &memx.F32Slab{Data: make([]float32, 0, 64)} },
		func(s *memx.F32Slab) { s.Data = s.Data[:0] },
		64,
	)
	cpu := tensorx.NewCPUDevice(pool)

	results, err := g.Search(context.Background(), []float32{1, 0, 0}, 5, cpu)
	if err != nil {
		t.Fatalf("Search on empty graph should not error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results for empty graph, got %v", results)
	}
}
