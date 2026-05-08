package rag

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestQuantizeBinary_Length(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{0, 0},
		{1, 1},    // 1 bit packs into 1 uint64
		{64, 1},   // exactly one word
		{65, 2},   // overflow into second word
		{768, 12}, // nomic-embed dim → 12 words = 96 bytes (32× compression)
	}
	for _, tt := range tests {
		v := make([]float32, tt.in)
		got := len(QuantizeBinary(v))
		if got != tt.want {
			t.Errorf("QuantizeBinary(dim=%d) length = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestQuantizeBinary_SignEncoding(t *testing.T) {
	// Alternating sign: the bits should also alternate.
	v := []float32{1, -1, 1, -1, 1, -1, 1, -1}
	q := QuantizeBinary(v)
	if len(q) != 1 {
		t.Fatalf("want 1 word, got %d", len(q))
	}
	// Bit i is set iff v[i] >= 0 → expected pattern 10101010 in LSB order.
	want := uint64(0b01010101)
	if q[0] != want {
		t.Errorf("encoding mismatch: got %b, want %b", q[0], want)
	}
}

func TestHammingDistance_SelfIsZero(t *testing.T) {
	v := []float32{0.5, -0.2, 0.1, -0.8, 0.3}
	q := QuantizeBinary(v)
	if d := HammingDistance(q, q); d != 0 {
		t.Errorf("self distance = %d, want 0", d)
	}
}

func TestHammingDistance_Opposite(t *testing.T) {
	// Two perfectly opposite vectors → distance equals dim.
	a := []float32{1, 1, 1, 1, 1, 1, 1, 1}
	b := []float32{-1, -1, -1, -1, -1, -1, -1, -1}
	qa := QuantizeBinary(a)
	qb := QuantizeBinary(b)
	d := HammingDistance(qa, qb)
	if d != 8 {
		t.Errorf("opposite distance = %d, want 8", d)
	}
}

func TestHammingDistance_LengthMismatch(t *testing.T) {
	a := []uint64{0x1234}
	b := []uint64{0x1234, 0x5678}
	if d := HammingDistance(a, b); d != -1 {
		t.Errorf("length mismatch should return -1, got %d", d)
	}
}

func TestHammingSimilarity_RangeClamped(t *testing.T) {
	v := make([]float32, 768)
	for i := range v {
		v[i] = float32(math.Sin(float64(i) * 0.1))
	}
	q := QuantizeBinary(v)
	if s := HammingSimilarity(q, q, 768); s != 1.0 {
		t.Errorf("self similarity = %v, want 1.0", s)
	}
}

func TestBinaryVsCosine_Agreement(t *testing.T) {
	// On 768-d random vectors the binary Hamming-derived similarity should
	// correlate with cosine similarity. We do not expect exact agreement,
	// just that "most similar" identified by cosine is also "most similar"
	// identified by Hamming — a basic sanity check for first-stage filter
	// quality.
	const dim = 768
	r := rand.New(rand.NewPCG(11, 13))
	query := make([]float32, dim)
	for i := range query {
		query[i] = r.Float32()*2 - 1
	}
	// A nearby vector (perturbed query) and a far one (independent random).
	near := make([]float32, dim)
	far := make([]float32, dim)
	for i := range near {
		near[i] = query[i] + (r.Float32()-0.5)*0.1 // small perturbation
		far[i] = r.Float32()*2 - 1                 // fresh random
	}
	qb := QuantizeBinary(query)
	nb := QuantizeBinary(near)
	fb := QuantizeBinary(far)

	nearSim := HammingSimilarity(qb, nb, dim)
	farSim := HammingSimilarity(qb, fb, dim)
	if nearSim <= farSim {
		t.Errorf("binary similarity ordering wrong: near=%v far=%v", nearSim, farSim)
	}
}

// Benchmarks — quantify the "extreme RAM + extreme speed" claim.

func BenchmarkQuantizeBinary_768(b *testing.B) {
	v := make([]float32, 768)
	for i := range v {
		v[i] = float32(math.Sin(float64(i) * 0.1))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = QuantizeBinary(v)
	}
}

func BenchmarkHammingDistance_768(b *testing.B) {
	va := make([]float32, 768)
	vb := make([]float32, 768)
	for i := range va {
		va[i] = float32(math.Sin(float64(i) * 0.1))
		vb[i] = float32(math.Cos(float64(i) * 0.1))
	}
	qa := QuantizeBinary(va)
	qb := QuantizeBinary(vb)
	b.ResetTimer()
	var sink int
	for i := 0; i < b.N; i++ {
		sink += HammingDistance(qa, qb)
	}
	if sink == 0 {
		b.Fatal("unexpected zero sink")
	}
}

func BenchmarkHammingSimilarity_768(b *testing.B) {
	va := make([]float32, 768)
	vb := make([]float32, 768)
	for i := range va {
		va[i] = float32(math.Sin(float64(i) * 0.1))
		vb[i] = float32(math.Cos(float64(i) * 0.1))
	}
	qa := QuantizeBinary(va)
	qb := QuantizeBinary(vb)
	b.ResetTimer()
	var sink float32
	for i := 0; i < b.N; i++ {
		sink += HammingSimilarity(qa, qb, 768)
	}
	if sink == 0 {
		b.Fatal("unexpected zero sink")
	}
}
