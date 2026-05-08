package rag

import (
	"math"
	"testing"
)

func TestQuantizeDequantize_RoundTrip(t *testing.T) {
	// A typical embedding with values roughly in [-0.8, 0.8]; reconstruction
	// error must stay bounded by scale/2 per-element.
	v := []float32{0.5, -0.25, 0.7, -0.8, 0.1, 0.0, 0.33, -0.33}
	q, scale := QuantizeInt8(v)
	if len(q) != len(v) {
		t.Fatalf("quantized length mismatch: got %d want %d", len(q), len(v))
	}
	if scale <= 0 {
		t.Fatalf("expected positive scale, got %v", scale)
	}
	recon := DequantizeInt8(q, scale)
	for i, orig := range v {
		diff := math.Abs(float64(recon[i] - orig))
		if diff > float64(scale) {
			t.Errorf("element %d: |recon-orig| = %v exceeds scale %v (orig=%v recon=%v q=%d)",
				i, diff, scale, orig, recon[i], q[i])
		}
	}
}

func TestQuantizeInt8_ZeroVector(t *testing.T) {
	v := make([]float32, 16)
	q, scale := QuantizeInt8(v)
	if scale != 1.0 {
		t.Errorf("zero vector should return scale=1.0 (divisor-safe), got %v", scale)
	}
	for _, x := range q {
		if x != 0 {
			t.Errorf("zero input produced non-zero quantized value %d", x)
		}
	}
}

func TestDotProductInt8_MatchesFloat(t *testing.T) {
	// Deterministic seed-like pattern, 768 dim (same as nomic-embed output).
	const dim = 768
	a := make([]float32, dim)
	b := make([]float32, dim)
	for i := range a {
		a[i] = float32(math.Sin(float64(i) * 0.1))
		b[i] = float32(math.Cos(float64(i) * 0.1))
	}

	// Ground truth dot product in float64 to minimize accumulator error.
	var truth float64
	for i := range a {
		truth += float64(a[i]) * float64(b[i])
	}

	qa, sa := QuantizeInt8(a)
	qb, sb := QuantizeInt8(b)
	got := float64(DotProductInt8(qa, qb, sa, sb))

	// 5% relative error is the realistic bound for symmetric int8 on
	// 768-dim embeddings with values spanning the full [-1, 1] range.
	// FAISS docs quote 1-5% typical for this configuration; tighter bounds
	// require asymmetric quantization or product quantization (PQ8/PQ4).
	rel := math.Abs(got-truth) / math.Abs(truth)
	if rel > 0.05 {
		t.Errorf("int8 dot product deviates too far: got=%v truth=%v rel=%v", got, truth, rel)
	}
}

func TestCosineDistanceInt8_MatchesFloat(t *testing.T) {
	const dim = 768
	a := make([]float32, dim)
	b := make([]float32, dim)
	for i := range a {
		a[i] = float32(math.Sin(float64(i) * 0.1))
		b[i] = float32(math.Cos(float64(i) * 0.1))
	}
	// Float reference.
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	refDist := 1.0 - dot/math.Sqrt(na*nb)

	qa, _ := QuantizeInt8(a)
	qb, _ := QuantizeInt8(b)
	gotDist := float64(CosineDistanceInt8(qa, qb))

	diff := math.Abs(gotDist - refDist)
	if diff > 0.01 {
		t.Errorf("cosine distance int8 off by %v (ref=%v got=%v)", diff, refDist, gotDist)
	}
}

// BenchmarkDotProductFloat32_768 is the baseline for comparison against
// the int8 variant. Uses the same 4-way unroll pattern so the comparison
// measures only the data-type effect.
func BenchmarkDotProductFloat32_768(b *testing.B) {
	const dim = 768
	a := make([]float32, dim)
	c := make([]float32, dim)
	for i := range a {
		a[i] = float32(i%127) / 127.0
		c[i] = float32((i*17)%127) / 127.0
	}
	b.ResetTimer()
	var sink float32
	for i := 0; i < b.N; i++ {
		// Same 4-way unroll pattern as CosineDistance, so the comparison
		// isolates data-type, not loop structure.
		var s0, s1, s2, s3 float32
		n := len(a)
		end4 := n - (n % 4)
		for j := 0; j < end4; j += 4 {
			s0 += a[j] * c[j]
			s1 += a[j+1] * c[j+1]
			s2 += a[j+2] * c[j+2]
			s3 += a[j+3] * c[j+3]
		}
		sink += s0 + s1 + s2 + s3
	}
	if sink == 0 {
		b.Fatal("unexpected zero sink")
	}
}

func BenchmarkDotProductInt8_768(b *testing.B) {
	const dim = 768
	af := make([]float32, dim)
	cf := make([]float32, dim)
	for i := range af {
		af[i] = float32(i%127) / 127.0
		cf[i] = float32((i*17)%127) / 127.0
	}
	a, sa := QuantizeInt8(af)
	c, sc := QuantizeInt8(cf)
	b.ResetTimer()
	var sink float32
	for i := 0; i < b.N; i++ {
		sink += DotProductInt8(a, c, sa, sc)
	}
	if sink == 0 {
		b.Fatal("unexpected zero sink")
	}
}

func BenchmarkCosineDistanceInt8_768(b *testing.B) {
	const dim = 768
	af := make([]float32, dim)
	cf := make([]float32, dim)
	for i := range af {
		af[i] = float32(i%127) / 127.0
		cf[i] = float32((i*17)%127) / 127.0
	}
	a, _ := QuantizeInt8(af)
	c, _ := QuantizeInt8(cf)
	b.ResetTimer()
	var sink float32
	for i := 0; i < b.N; i++ {
		sink += CosineDistanceInt8(a, c)
	}
	if sink == 0 {
		b.Fatal("unexpected zero sink")
	}
}

func BenchmarkCosineSim_768(b *testing.B) {
	const dim = 768
	af := make([]float32, dim)
	bf := make([]float32, dim)
	for i := range af {
		af[i] = float32(math.Sin(float64(i) * 0.1))
		bf[i] = float32(math.Cos(float64(i) * 0.1))
	}
	var sink float32
	for b.Loop() {
		sink += cosineSim(af, bf)
	}
	if sink == 0 {
		b.Fatal("unexpected zero sink")
	}
}

func BenchmarkQuantizeInt8_768(b *testing.B) {
	const dim = 768
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(math.Sin(float64(i) * 0.1))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = QuantizeInt8(v)
	}
}
