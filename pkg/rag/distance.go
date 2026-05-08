// pkg/rag/distance.go — CPU-dispatch table for cosine similarity. [Épica 305.0]
//
// At init time the best available implementation is selected based on CPU
// feature flags from golang.org/x/sys/cpu. Arch-specific files override in
// their own init() (alphabetical order guarantees they run after this file):
//   distance_amd64.go → cosineAVX2 for AVX2/AVX-512 (305.A)
//   distance_arm64.go → cosineNEON for ASIMD       (305.C)
package rag

import "golang.org/x/sys/cpu"

// cosineF32 is the active cosine similarity implementation, selected at init
// time based on CPU capabilities. Defaults to the scalar 4-way unrolled
// implementation.
var cosineF32 func(a, b []float32) float32

func init() {
	switch {
	case cpu.X86.HasAVX512F:
		cosineF32 = cosineScalar // overridden by distance_amd64.go init() → cosineAVX2 (305.A); ZMM upgrade in 305.B
	case cpu.X86.HasAVX2:
		cosineF32 = cosineScalar // overridden by distance_amd64.go init() → cosineAVX2 (305.A)
	case cpu.ARM64.HasASIMD:
		cosineF32 = cosineScalar // overridden by distance_arm64.go init() → cosineNEON (305.C)
	default:
		cosineF32 = cosineScalar // portable: 4-way unrolled, GOAMD64=v3 auto-vectorizes
	}
}

// cosineScalar is the portable fallback: 4-way unrolled float32 loop.
// Delegates to cosineSim in shared_graph.go which owns the authoritative
// scalar implementation.
func cosineScalar(a, b []float32) float32 {
	return cosineSim(a, b)
}
