// distance_test.go — Correctness tests for cosine distance dispatch. [Épica 305.C]
//go:build arm64

package rag

import (
	"math"
	"testing"
)

func TestCosineNEON_Correctness(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []float32
		wantNear float32 // expected cosine similarity (±1e-5 tolerance)
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
			name:     "768-dim matches scalar",
			a:        make768(),
			b:        make768rev(),
			wantNear: cosineScalar(make768(), make768rev()),
		},
		{
			name:     "5-element tail check (n=5, SIMD+1 tail)",
			a:        []float32{1, 0, 0, 0, 1},
			b:        []float32{0, 0, 0, 0, 1},
			wantNear: float32(1.0 / math.Sqrt2),
		},
	}

	const tol = 1e-5
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineNEON(tc.a, tc.b)
			if diff := float32(math.Abs(float64(got - tc.wantNear))); diff > tol {
				t.Errorf("cosineNEON = %v, want %v (diff %v > tol %v)", got, tc.wantNear, diff, tol)
			}
		})
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
