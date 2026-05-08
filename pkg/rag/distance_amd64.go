//go:build amd64

// distance_amd64.go — AMD64 cosine dispatch override. [Épicas 305.A/B]
//
// init() runs after distance.go init (alphabetical order) and rewires
// cosineF32 to cosineAVX512 or cosineAVX2 depending on CPU feature flags.
// No .s file: pure Go + GOAMD64=v3/v4 emits packed VFMADD231PS.
//
// 4-way (AVX2, GOAMD64=v3): 16 XMM register budget — 12 persistent + 2 temps = 14 OK.
// 8-way (AVX-512, GOAMD64=v4): 32 ZMM registers — 24 persistent float32
// accumulators fit without spill. For ZMM emission: GOAMD64=v4 make build-mcp.
// On hardware without AVX-512 cosineAVX512 falls back to scalar 8-way (correct, no ZMM). [305.D]

package rag

import (
	"math"

	"golang.org/x/sys/cpu"
)

func init() {
	switch {
	case cpu.X86.HasAVX512F:
		cosineF32 = cosineAVX512 // 305.B: 8-way; ZMM via GOAMD64=v4, safe scalar 8-way on v3
	case cpu.X86.HasAVX2:
		cosineF32 = cosineAVX2 // 305.A: 4-way, VFMADD231PS via GOAMD64=v3
	}
	// No-SIMD CPUs keep cosineScalar set by distance.go init.
}

// cosineAVX2 computes float32 cosine similarity with 4-way unrolled accumulation.
// Unlike cosineScalar→cosineSim, this is a self-contained implementation:
// the single hop via the dispatch pointer avoids one extra function call.
// With GOAMD64=v3 the compiler emits packed VFMADD231PS instructions for each
// of the 3 accumulation groups (dot, na, nb). 4-way is the maximum unroll
// factor that stays within the 16 XMM register budget (12 persistent + 2 temps).
// [Épica 305.A/D]
func cosineAVX2(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	n := len(a)

	var d0, d1, d2, d3 float32
	var na0, na1, na2, na3 float32
	var nb0, nb1, nb2, nb3 float32

	end4 := n - (n % 4)
	for i := 0; i < end4; i += 4 {
		x0, y0 := a[i], b[i]
		x1, y1 := a[i+1], b[i+1]
		x2, y2 := a[i+2], b[i+2]
		x3, y3 := a[i+3], b[i+3]
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

	// Scalar tail: n % 4 remaining elements.
	var dotT, naT, nbT float32
	for i := end4; i < n; i++ {
		x, y := a[i], b[i]
		dotT += x * y
		naT += x * x
		nbT += y * y
	}

	dot := d0 + d1 + d2 + d3 + dotT
	na := na0 + na1 + na2 + na3 + naT
	nb := nb0 + nb1 + nb2 + nb3 + nbT
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(float64(dot) / (math.Sqrt(float64(na)) * math.Sqrt(float64(nb))))
}

// cosineAVX512 computes float32 cosine similarity with 8-way unrolled accumulation.
// 24 independent float32 chains fit within the 32 ZMM register file without spill
// when compiled with GOAMD64=v4; on GOAMD64=v3 the compiler falls back to scalar
// 8-way (correct, faster than cosineScalar due to ILP, no ZMM emission). [Épica 305.B]
func cosineAVX512(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	n := len(a)

	var d0, d1, d2, d3, d4, d5, d6, d7 float32
	var na0, na1, na2, na3, na4, na5, na6, na7 float32
	var nb0, nb1, nb2, nb3, nb4, nb5, nb6, nb7 float32

	end8 := n - (n % 8)
	for i := 0; i < end8; i += 8 {
		x0, y0 := a[i], b[i]
		x1, y1 := a[i+1], b[i+1]
		x2, y2 := a[i+2], b[i+2]
		x3, y3 := a[i+3], b[i+3]
		x4, y4 := a[i+4], b[i+4]
		x5, y5 := a[i+5], b[i+5]
		x6, y6 := a[i+6], b[i+6]
		x7, y7 := a[i+7], b[i+7]
		d0 += x0 * y0
		d1 += x1 * y1
		d2 += x2 * y2
		d3 += x3 * y3
		d4 += x4 * y4
		d5 += x5 * y5
		d6 += x6 * y6
		d7 += x7 * y7
		na0 += x0 * x0
		na1 += x1 * x1
		na2 += x2 * x2
		na3 += x3 * x3
		na4 += x4 * x4
		na5 += x5 * x5
		na6 += x6 * x6
		na7 += x7 * x7
		nb0 += y0 * y0
		nb1 += y1 * y1
		nb2 += y2 * y2
		nb3 += y3 * y3
		nb4 += y4 * y4
		nb5 += y5 * y5
		nb6 += y6 * y6
		nb7 += y7 * y7
	}

	var dotT, naT, nbT float32
	for i := end8; i < n; i++ {
		x, y := a[i], b[i]
		dotT += x * y
		naT += x * x
		nbT += y * y
	}

	dot := d0 + d1 + d2 + d3 + d4 + d5 + d6 + d7 + dotT
	na := na0 + na1 + na2 + na3 + na4 + na5 + na6 + na7 + naT
	nb := nb0 + nb1 + nb2 + nb3 + nb4 + nb5 + nb6 + nb7 + nbT
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(float64(dot) / (math.Sqrt(float64(na)) * math.Sqrt(float64(nb))))
}
