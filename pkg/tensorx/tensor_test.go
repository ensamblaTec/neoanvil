package tensorx

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/memx"
)

func TestMatMulF32_Identity(t *testing.T) {
	const dim = 3
	pool := newI8Pool(dim * dim)
	cpu := NewCPUDevice(pool)

	aData := []float32{1, 2, 3, 4, 5, 6, 7, 8, 9}

	bData := []float32{1, 0, 0, 0, 1, 0, 0, 0, 1}
	cData := make([]float32, dim*dim)

	a, _ := NewTensor(aData, Shape{dim, dim})
	b, _ := NewTensor(bData, Shape{dim, dim})
	c, _ := NewTensor(cData, Shape{dim, dim})

	if err := cpu.MatMulF32(context.Background(), a, b, c); err != nil {
		t.Fatalf("MatMulF32 failed: %v", err)
	}

	expected := []float32{1, 2, 3, 4, 5, 6, 7, 8, 9}
	for i, v := range expected {
		if c.Data[i] != v {
			t.Errorf("c.Data[%d] = %f, want %f", i, c.Data[i], v)
		}
	}
}

func TestMatMulF32_KnownProduct(t *testing.T) {
	pool := newI8Pool(64)
	cpu := NewCPUDevice(pool)

	aData := []float32{1, 2, 3, 4}
	bData := []float32{5, 6, 7, 8}
	cData := make([]float32, 4)

	a, _ := NewTensor(aData, Shape{2, 2})
	b, _ := NewTensor(bData, Shape{2, 2})
	c, _ := NewTensor(cData, Shape{2, 2})

	if err := cpu.MatMulF32(context.Background(), a, b, c); err != nil {
		t.Fatalf("MatMulF32 failed: %v", err)
	}

	expected := []float32{19, 22, 43, 50}
	for i, v := range expected {
		if c.Data[i] != v {
			t.Errorf("c.Data[%d] = %f, want %f", i, c.Data[i], v)
		}
	}
}

func BenchmarkMatMulF32(b *testing.B) {
	const dim = 256
	pool := newI8Pool(dim * dim)
	cpu := NewCPUDevice(pool)

	aData := make([]float32, dim*dim)
	bData := make([]float32, dim*dim)
	cData := make([]float32, dim*dim)

	for i := range aData {
		aData[i] = float32(i % 127)
	}
	for i := range bData {
		bData[i] = float32(i % 127)
	}

	a, _ := NewTensor(aData, Shape{dim, dim})
	bb, _ := NewTensor(bData, Shape{dim, dim})
	c, _ := NewTensor(cData, Shape{dim, dim})

	ctx := context.Background()

	warmSlab := pool.Acquire()
	warmSlab.Data = warmSlab.Data[:dim*dim]
	pool.Release(warmSlab, cap(warmSlab.Data))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cpu.MatMulF32(ctx, a, bb, c)
	}
}

func newI8Pool(capacity int) *memx.ObservablePool[memx.F32Slab] {
	return memx.NewObservablePool(
		func() *memx.F32Slab { return &memx.F32Slab{Data: make([]float32, 0, capacity)} },
		func(s *memx.F32Slab) { s.Data = s.Data[:0] },
		capacity,
	)
}

func TestMatTransposeF32(t *testing.T) {
	pool := newI8Pool(64)
	cpu := NewCPUDevice(pool)

	aData := []float32{1, 2, 3, 4}
	cData := make([]float32, 4)
	a, _ := NewTensor(aData, Shape{2, 2})
	c, _ := NewTensor(cData, Shape{2, 2})

	if err := cpu.MatTransposeF32(a, c); err != nil {
		t.Fatalf("MatTransposeF32 failed: %v", err)
	}
	expected := []float32{1, 3, 2, 4}
	for i, v := range expected {
		if c.Data[i] != v {
			t.Errorf("c.Data[%d] = %f, want %f", i, c.Data[i], v)
		}
	}
}

// BenchmarkCosineDistance_768 stresses the HNSW hot path (768-dim vectors
// = nomic-embed-text output size). Quantifies the speedup from GOAMD64=v3
// auto-vectorization. [Épica 167.C]
func BenchmarkCosineDistance_768(b *testing.B) {
	const dim = 768
	pool := newI8Pool(dim)
	cpu := NewCPUDevice(pool)

	aData := make([]float32, dim)
	bData := make([]float32, dim)
	for i := range aData {
		aData[i] = float32(i%127) / 127.0
		bData[i] = float32((i*17)%127) / 127.0
	}
	a, _ := NewTensor(aData, Shape{dim})
	bb, _ := NewTensor(bData, Shape{dim})

	b.ResetTimer()
	var sink float32
	for i := 0; i < b.N; i++ {
		d, _ := cpu.CosineDistance(a, bb)
		sink += d
	}
	if sink == 0 {
		b.Fatal("unexpected zero sink")
	}
}

func BenchmarkEKF_CovarianceUpdate(b *testing.B) {
	pool := newI8Pool(256)
	cpu := NewCPUDevice(pool)

	F_data := []float32{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	P_data := []float32{0.5, 0, 0, 0, 0, 0.5, 0, 0, 0, 0, 0.5, 0, 0, 0, 0, 0.5}
	Q_data := []float32{0.1, 0, 0, 0, 0, 0.1, 0, 0, 0, 0, 0.1, 0, 0, 0, 0, 0.1}

	F, _ := NewTensor(F_data, Shape{4, 4})
	P, _ := NewTensor(P_data, Shape{4, 4})
	Q, _ := NewTensor(Q_data, Shape{4, 4})

	F_T_data := make([]float32, 16)
	F_T, _ := NewTensor(F_T_data, Shape{4, 4})

	Temp_data := make([]float32, 16)
	Temp, _ := NewTensor(Temp_data, Shape{4, 4})

	P_next_data := make([]float32, 16)
	P_next, _ := NewTensor(P_next_data, Shape{4, 4})

	ctx := context.Background()

	// Pre-warm the pool to ensure 0 Allocs during benchmark
	warmSlab := pool.Acquire()
	warmSlab.Data = warmSlab.Data[:16]
	pool.Release(warmSlab, cap(warmSlab.Data))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// P = F * P * F^T + Q
		_ = cpu.MatTransposeF32(F, F_T)
		_ = cpu.MatMulF32(ctx, F, P, Temp)        // Temp = F * P
		_ = cpu.MatMulF32(ctx, Temp, F_T, P_next) // P_next = Temp * F_T
		_ = cpu.MatAddF32(P_next, Q, P_next)      // P_next = P_next + Q
	}
}
