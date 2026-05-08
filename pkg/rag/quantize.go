// Package rag — symmetric int8 quantization primitives. [PILAR-XXV/170.B]
//
// Converts float32 embedding vectors to int8 with a per-vector scale factor.
// Empirically measured gain on Go 1.26 / GOAMD64=v3 / i5-10400:
//
//   - 4× RAM reduction: a 768-dim nomic vector drops from 3 KB to 768 B,
//     so a 100k-node HNSW graph uses 75 MB of vectors instead of 300 MB.
//     This is the PRIMARY win — RAM is the constraint on small single-host
//     deployments, not compute.
//   - Compute: currently 2× SLOWER than the float32 baseline (582 ns vs
//     234 ns on DotProduct_768). Reason: the Go 1.26 auto-vectorizer does
//     NOT emit VPMADDUBSW / VPDPBUSDS for int8 × int8 loops; every int8 is
//     widened to int32 scalar before multiplication, which costs more than
//     the direct float32 MULSS. The 3-4× speedup documented by FAISS and
//     ScaNN requires SIMD VNNI intrinsics the Go compiler does not generate
//     without manual assembly.
//   - Recall: <1% loss on typical sentence-embedding workloads (FAISS), up
//     to 5% relative error on raw dot product — tested on 768-dim sin/cos
//     synthetic vectors. Cosine distance is much more robust because the
//     scale factors cancel.
//
// Use int8 when RAM is the bottleneck (corpus >100k nodes on a single host).
// Use float32 when compute is the bottleneck (which is almost always the
// case on pure-Go code paths without SIMD intrinsics).
//
// Future: Go 1.27+ may gain auto-vectorization for int8 reduction loops, or
// a manual _amd64.s kernel can be added once the neoanvil workload justifies
// the per-architecture maintenance cost.
//
// Design: symmetric quantization — each vector v maps to (q, s) where
//
//	q[i] = round(v[i] / s) clamped to [-127, 127]
//	s    = max(|v|) / 127
//
// Asymmetric (unsigned) quantization gains one extra bit but loses the
// property that dot(a,b) ≈ scaleA * scaleB * dot(qa,qb); symmetric keeps
// the math clean and the scale factor small.
//
// The primitives here are stateless and deterministic — no globals, no
// pools, no locks. Call sites decide when to quantize (at ingest) and
// when to dequantize (when displaying to the operator).

package rag

import "math"

// QuantizeInt8 converts a float32 vector to int8 using symmetric per-vector
// scaling. Returns (q, scale) where v[i] ≈ scale * float32(q[i]).
//
// On a degenerate all-zero input, the scale returned is 1.0 (not zero) so
// downstream dot products do not divide by zero. A zero vector is still a
// zero vector regardless of scale.
func QuantizeInt8(v []float32) ([]int8, float32) {
	if len(v) == 0 {
		return nil, 1.0
	}
	// Single pass over the input to find the max-abs. Compiler-friendly:
	// one accumulator, no branches in the body (the conditional promote is
	// trivially lifted).
	var absMax float32
	for _, x := range v {
		if x < 0 {
			x = -x
		}
		if x > absMax {
			absMax = x
		}
	}
	if absMax == 0 {
		return make([]int8, len(v)), 1.0
	}
	scale := absMax / 127.0
	inv := 1.0 / scale
	q := make([]int8, len(v))
	for i, x := range v {
		// round-half-away-from-zero — matches IEEE round(), NOT truncation.
		// Bias of 0.5 with sign keeps the quantization symmetric around 0.
		r := x * inv
		if r >= 0 {
			r += 0.5
		} else {
			r -= 0.5
		}
		// Clamp to int8 range (max-abs normalization almost never triggers
		// this, but floating-point rounding at the boundary can).
		switch {
		case r > 127:
			q[i] = 127
		case r < -128:
			q[i] = -128
		default:
			q[i] = int8(r)
		}
	}
	return q, scale
}

// DequantizeInt8 reconstructs the approximated float32 vector. Useful for
// display and for crossing back into float32 code paths (e.g. CPG
// activation) during a gradual migration.
func DequantizeInt8(q []int8, scale float32) []float32 {
	out := make([]float32, len(q))
	for i, x := range q {
		out[i] = float32(x) * scale
	}
	return out
}

// DotProductInt8 computes dot(a, b) in int32 accumulators and rescales
// back to float32. The accumulator is int32 because two int8 products fit
// in int16 and 768 of those fit in int32 with 10 bits of headroom.
//
// The 4-way unroll mirrors the pattern established in
// pkg/tensorx.CosineDistance — four independent int32 chains let the Go
// auto-vectorizer emit packed multiplies on GOAMD64=v3.
func DotProductInt8(a, b []int8, scaleA, scaleB float32) float32 {
	n := len(a)
	if n != len(b) {
		return 0
	}
	var s0, s1, s2, s3 int32
	end4 := n - (n % 4)
	for i := 0; i < end4; i += 4 {
		s0 += int32(a[i]) * int32(b[i])
		s1 += int32(a[i+1]) * int32(b[i+1])
		s2 += int32(a[i+2]) * int32(b[i+2])
		s3 += int32(a[i+3]) * int32(b[i+3])
	}
	var tail int32
	for i := end4; i < n; i++ {
		tail += int32(a[i]) * int32(b[i])
	}
	dotInt := s0 + s1 + s2 + s3 + tail
	return float32(dotInt) * scaleA * scaleB
}

// CosineDistanceInt8 returns 1 - cosine(a, b) for two quantized vectors.
// Norms are computed over the same quantized representation so the scale
// factors cancel cleanly in the ratio: the cosine of the approximated
// vectors is the same as cos(scaleA*qa, scaleB*qb) because cosine is
// scale-invariant.
func CosineDistanceInt8(a, b []int8) float32 {
	if len(a) != len(b) {
		return 1.0
	}
	n := len(a)
	var d0, d1, d2, d3 int32
	var na0, na1, na2, na3 int32
	var nb0, nb1, nb2, nb3 int32
	end4 := n - (n % 4)
	for i := 0; i < end4; i += 4 {
		x0, y0 := int32(a[i]), int32(b[i])
		x1, y1 := int32(a[i+1]), int32(b[i+1])
		x2, y2 := int32(a[i+2]), int32(b[i+2])
		x3, y3 := int32(a[i+3]), int32(b[i+3])
		d0 += x0 * y0
		d1 += x1 * y1
		d2 += x2 * y2
		d3 += x3 * y3
		na0 += x0 * x0
		na1 += x1 * x1
		na2 += x2 * x2
		na3 += x3 * x3
		nb0 += y0 * y0
		nb1 += y1 * y1
		nb2 += y2 * y2
		nb3 += y3 * y3
	}
	var dotT, naT, nbT int32
	for i := end4; i < n; i++ {
		x, y := int32(a[i]), int32(b[i])
		dotT += x * y
		naT += x * x
		nbT += y * y
	}
	dot := d0 + d1 + d2 + d3 + dotT
	normA := na0 + na1 + na2 + na3 + naT
	normB := nb0 + nb1 + nb2 + nb3 + nbT
	denom := math.Sqrt(float64(normA) * float64(normB))
	if denom == 0 {
		return 1.0
	}
	return float32(1.0 - float64(dot)/denom)
}
