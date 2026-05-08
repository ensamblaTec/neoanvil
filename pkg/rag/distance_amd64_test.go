//go:build amd64

// distance_amd64_test.go — Correctness + benchmarks for cosineAVX2/cosineAVX512. [Épicas 305.A/B/D]
package rag

import (
	"math"
	"testing"
)

func TestCosineAVX2_Correctness(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []float32
		wantNear float32
	}{
		{
			name:     "identical unit vectors",
			a:        []float32{1, 0, 0, 0},
			b:        []float32{1, 0, 0, 0},
			wantNear: 1.0,
		},
		{
			name:     "orthogonal vectors",
			a:        []float32{1, 0, 0, 0},
			b:        []float32{0, 1, 0, 0},
			wantNear: 0.0,
		},
		{
			name:     "opposite direction",
			a:        []float32{1, 0, 0, 0},
			b:        []float32{-1, 0, 0, 0},
			wantNear: -1.0,
		},
		{
			name:     "45-degree angle",
			a:        []float32{1, 0, 0, 0},
			b:        []float32{1, 1, 0, 0},
			wantNear: float32(1.0 / math.Sqrt2),
		},
		{
			name:     "tail: 5 elements (4+1)",
			a:        []float32{1, 0, 0, 0, 1},
			b:        []float32{0, 0, 0, 0, 1},
			wantNear: float32(1.0 / math.Sqrt2),
		},
		{
			name:     "768-dim matches scalar",
			a:        make768(),
			b:        make768rev(),
			wantNear: cosineScalar(make768(), make768rev()),
		},
	}

	const tol = 1e-5
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineAVX2(tc.a, tc.b)
			if diff := float32(math.Abs(float64(got - tc.wantNear))); diff > tol {
				t.Errorf("cosineAVX2 = %v, want %v (diff %v > tol %v)", got, tc.wantNear, diff, tol)
			}
		})
	}
}

// BenchmarkCosineScalar768 — baseline (4-way unrolled, GOAMD64=v3 auto-vec).
func BenchmarkCosineScalar768(b *testing.B) {
	a, v := make768(), make768rev()
	b.ReportAllocs()
	for b.Loop() {
		_ = cosineScalar(a, v)
	}
}

// BenchmarkCosineAVX2768 — 4-way direct, GOAMD64=v3 VFMADD231PS, one hop via dispatch. [Épica 305.A]
func BenchmarkCosineAVX2768(b *testing.B) {
	a, v := make768(), make768rev()
	b.ReportAllocs()
	for b.Loop() {
		_ = cosineAVX2(a, v)
	}
}

// BenchmarkCosineDispatch768 — exercises cosineF32 (the live dispatch pointer).
func BenchmarkCosineDispatch768(b *testing.B) {
	a, v := make768(), make768rev()
	b.ReportAllocs()
	for b.Loop() {
		_ = cosineF32(a, v)
	}
}

// TestCosineAVX512_Correctness verifies cosineAVX512 matches cosineScalar
// within float32 tolerance. Runs on all hardware (no AVX-512 required for
// correctness — the function is pure Go). [Épica 305.B]
func TestCosineAVX512_Correctness(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []float32
		wantNear float32
	}{
		{
			name:     "identical unit vectors",
			a:        []float32{1, 0, 0, 0, 0, 0, 0, 0},
			b:        []float32{1, 0, 0, 0, 0, 0, 0, 0},
			wantNear: 1.0,
		},
		{
			name:     "orthogonal 8-elem",
			a:        []float32{1, 0, 0, 0, 0, 0, 0, 0},
			b:        []float32{0, 1, 0, 0, 0, 0, 0, 0},
			wantNear: 0.0,
		},
		{
			name:     "tail: 9 elements (8+1)",
			a:        []float32{1, 0, 0, 0, 0, 0, 0, 0, 1},
			b:        []float32{0, 0, 0, 0, 0, 0, 0, 0, 1},
			wantNear: float32(1.0 / math.Sqrt2),
		},
		{
			name:     "768-dim matches scalar",
			a:        make768(),
			b:        make768rev(),
			wantNear: cosineScalar(make768(), make768rev()),
		},
	}

	const tol = 1e-5
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineAVX512(tc.a, tc.b)
			if diff := float32(math.Abs(float64(got - tc.wantNear))); diff > tol {
				t.Errorf("cosineAVX512 = %v, want %v (diff %v > tol %v)", got, tc.wantNear, diff, tol)
			}
		})
	}
}

// BenchmarkCosineAVX512_768 — 8-way ILP; ZMM via GOAMD64=v4, scalar 8-way on v3. [305.B]
func BenchmarkCosineAVX512_768(b *testing.B) {
	a, v := make768(), make768rev()
	b.ReportAllocs()
	for b.Loop() {
		_ = cosineAVX512(a, v)
	}
}

func make768() []float32 {
	v := make([]float32, 768)
	for i := range v {
		v[i] = float32(i+1) * 0.001
	}
	return v
}

func make768rev() []float32 {
	v := make([]float32, 768)
	for i := range v {
		v[i] = float32(768-i) * 0.001
	}
	return v
}
