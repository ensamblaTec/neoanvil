// pkg/rag/distance_arm64.go — ARM64 NEON cosine kernel stub + dispatch. [Épica 305.C]
//
// cosineNEON is implemented in distance_arm64.s using 128-bit NEON registers
// (V0-V31, 4×float32 per lane). Processing 4 floats per cycle vs 1 in scalar.
// For 768-dim vectors: 192 NEON iterations vs 192 scalar (but with FMA pipelining).
// Dispatch init() runs after distance.go init() (alphabetical file order) and
// overrides cosineF32 = cosineNEON when cpu.ARM64.HasASIMD is true.
//
//go:build arm64

package rag

import "golang.org/x/sys/cpu"

// cosineNEON computes cosine similarity using ARM64 NEON FMLA instructions.
// Requires ASIMDHP feature (standard on all ARMv8.0-A+ cores including Apple M-series).
func cosineNEON(a, b []float32) float32

func init() {
	if cpu.ARM64.HasASIMD {
		cosineF32 = cosineNEON
	}
}
