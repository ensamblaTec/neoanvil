package rag

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/memx"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// constEmbedder returns a fixed vector — used to make flashback tests deterministic.
type constEmbedder struct {
	vec []float32
}

func (e *constEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	out := make([]float32, len(e.vec))
	copy(out, e.vec)
	return out, nil
}

func (e *constEmbedder) Dimension() int { return len(e.vec) }

func newTestCPU() tensorx.ComputeDevice {
	pool := memx.NewObservablePool(
		func() *memx.F32Slab { return &memx.F32Slab{Data: make([]float32, 0, 64)} },
		func(s *memx.F32Slab) { s.Data = s.Data[:0] },
		64,
	)
	return tensorx.NewCPUDevice(pool)
}

// [SRE-31.1.2] Flashback Accuracy & Distance — validates dist < 0.25 triggers flashback.
func TestSearchFlashback_HitsBelowThreshold(t *testing.T) {
	const dim = 3
	cpu := newTestCPU()

	// Build graph with a single known vector (identical to our query → dist = 0)
	g := NewGraph(1, 4, dim)
	g.AddNode(Node{DocID: 999, EdgesOffset: 0, EdgesLength: 0, Layer: 0})
	g.Vectors = []float32{0.577, 0.577, 0.577} // normalised diagonal

	// WAL with doc meta for that node
	dir := t.TempDir()
	wal, err := OpenWAL(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()

	if err := wal.SaveDocMeta(999, "backend/user.go", "Always use 5s context timeout", 0); err != nil {
		t.Fatalf("SaveDocMeta: %v", err)
	}

	// Embedder returns identical vector → cosine distance = 0 → below threshold
	embedder := &constEmbedder{vec: []float32{0.577, 0.577, 0.577}}

	result, err := SearchFlashback(context.Background(), g, wal, cpu, embedder,
		"undefined: createUser", "backend/user.go", nil, nil)
	if err != nil {
		t.Fatalf("SearchFlashback error: %v", err)
	}
	if result == nil {
		t.Fatal("expected a flashback result (dist=0 < 0.25), got nil")
	}
	if result.Distance >= 0.25 {
		t.Errorf("expected dist < 0.25, got %.4f", result.Distance)
	}
	if result.Content != "Always use 5s context timeout" {
		t.Errorf("unexpected content: %q", result.Content)
	}
}

// TestSearchFlashback_MissesAboveThreshold validates no flashback when dist >= 0.25.
func TestSearchFlashback_MissesAboveThreshold(t *testing.T) {
	const dim = 3
	cpu := newTestCPU()

	g := NewGraph(1, 4, dim)
	g.AddNode(Node{DocID: 1, EdgesOffset: 0, EdgesLength: 0, Layer: 0})
	g.Vectors = []float32{1, 0, 0} // axis X

	dir := t.TempDir()
	wal, _ := OpenWAL(filepath.Join(dir, "test.db"))
	defer wal.Close()
	_ = wal.SaveDocMeta(1, "pkg/foo.go", "some old lesson", 0)

	// Embedder returns orthogonal vector → cosine distance = 1.0 → above threshold
	embedder := &constEmbedder{vec: []float32{0, 1, 0}} // axis Y

	result, err := SearchFlashback(context.Background(), g, wal, cpu, embedder,
		"some error", "pkg/foo.go", nil, nil)
	if err != nil {
		t.Fatalf("SearchFlashback error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil flashback (dist >= 0.25), got dist=%.4f", result.Distance)
	}
}

// TestSearchFlashback_EmptyGraph returns nil without error.
func TestSearchFlashback_EmptyGraph(t *testing.T) {
	g := NewGraph(0, 0, 3)
	dir := t.TempDir()
	wal, _ := OpenWAL(filepath.Join(dir, "test.db"))
	defer wal.Close()
	cpu := newTestCPU()
	embedder := &constEmbedder{vec: []float32{1, 0, 0}}

	result, err := SearchFlashback(context.Background(), g, wal, cpu, embedder, "error", "file.go", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil on empty graph")
	}
}
