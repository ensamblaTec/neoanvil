package mctx

import (
	"testing"
)

func TestDoubleBuffer_AtomicHybridSwap(t *testing.T) {
	eng := NewFederatedEngine(128)

	theta := make([]float32, 128)
	for i := range theta {
		theta[i] = 1.0
	}

	eng.AggregateBackground([][]float32{theta})

	p1 := eng.ReadProd()
	if p1[0] == 1.0 {
		t.Fatalf("Violación Aislamiento: Lectores asimilaron el dev prematuramente")
	}

	eng.SwapEpoch(0.0)

	p2 := eng.ReadProd()
	if p2[0] != 1.0 {
		t.Fatalf("Shadow Epoching falló promoviendo tensor Sellado. P2: %f", p2[0])
	}
}

func BenchmarkFederated_HybridConcurrency(b *testing.B) {
	eng := NewFederatedEngine(1024)

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Workers disparan cientos de Lecturas O(1) concurrentes (HOT CACHE M-Zero)
			prod := eng.ReadProd()
			_ = prod[10]
		}
	})
}

// Background Network Latency (AES-GCM Costly Bound)
func BenchmarkFederated_BackgroundAggregate(b *testing.B) {
	eng := NewFederatedEngine(1024)
	theta := make([]float32, 1024)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		eng.AggregateBackground([][]float32{theta})
	}
}

func TestMultiKrum_ByzantineVenom_3Sigma(t *testing.T) {
	eng := NewFederatedEngine(10)
	thetaSano1 := make([]float32, 10)
	thetaSano2 := make([]float32, 10)
	thetaSano3 := make([]float32, 10)
	for i := range thetaSano1 {
		thetaSano1[i] = 1.0
		thetaSano2[i] = 1.1
		thetaSano3[i] = 0.9
	}

	veneno := make([]float32, 10)
	for i := range veneno {
		veneno[i] = 50.0
	}

	eng.AggregateBackground([][]float32{thetaSano1, thetaSano2, thetaSano3, veneno})
}
