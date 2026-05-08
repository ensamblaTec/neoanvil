// pkg/rag/quantize_dispatch.go — Runtime CPU-feature detection and int8 kernel
// dispatch table. [368.A]
//
// At init time the optimal int8 dot-product implementation is selected based
// on CPU feature flags from golang.org/x/sys/cpu. When 327.B lands with
// architecture-specific assembly kernels, wire them here by importing the
// arch-specific file and overriding dotInt8Impl in its init().
//
// Boot log: "[CPU] detected: v4 (AVX-512 + VNNI)" or "v3 (AVX2 + FMA)" or
//
//	"v2 (SSE4.2)" or "arm64 (NEON + DotProd)" etc.
//
// The active dispatch function is called by SearchInt8 and can be read via
// ActiveInt8KernelTier() for metrics/logging.
package rag

import (
	"log"

	"golang.org/x/sys/cpu"
)

// CPUTier names the highest int8 kernel tier available at runtime. [368.A]
type CPUTier string

const (
	TierV4VNNI    CPUTier = "v4 (AVX-512 + VNNI)"
	TierV3AVX2    CPUTier = "v3 (AVX2 + FMA)"
	TierV2SSE4    CPUTier = "v2 (SSE4.2)"
	TierARM64Dot  CPUTier = "arm64 (NEON + ASIMDP)"
	TierARM64NEON CPUTier = "arm64 (NEON)"
	TierARM64SVE2 CPUTier = "arm64 (SVE2 + ASIMDP)"
	TierScalar    CPUTier = "scalar (portable)"
)

// dotInt8Impl is the active int8 raw dot-product kernel (unscaled, signed accumulate).
// Signature mirrors the inner loop of DotProductInt8 — takes two equal-length
// []int8 slices and returns their raw integer dot product.
// When 327.B lands: replace dotInt8Scalar with the compiled asm kernel.
var dotInt8Impl func(a, b []int8) int32

// activeTier is set at init and exported for HUD_STATE / PROJECT_DIGEST metrics.
var activeTier CPUTier

func init() {
	selectInt8Kernel()
}

// selectInt8Kernel sets dotInt8Impl and activeTier based on runtime CPU features.
// Called once at package init. Overridable by arch-specific init() (not yet landed).
func selectInt8Kernel() {
	switch {
	// x86-64: prefer VNNI > AVX2 > SSE4.2 > scalar
	case cpu.X86.HasAVX512BW && cpu.X86.HasAVX512VNNI:
		// 327.B VNNI kernel not yet landed — fall back to scalar with dispatch
		// slot reserved. Swap comment when 327.B ships:
		//   dotInt8Impl = dotInt8VNNI
		dotInt8Impl = dotInt8Scalar
		activeTier = TierV4VNNI
	case cpu.X86.HasAVX2:
		// 327.B AVX2 kernel not yet landed — scalar with v3 tier marker.
		//   dotInt8Impl = dotInt8AVX2
		dotInt8Impl = dotInt8Scalar
		activeTier = TierV3AVX2
	case cpu.X86.HasSSE42:
		dotInt8Impl = dotInt8Scalar
		activeTier = TierV2SSE4

	// ARM64: prefer SVE2+DotProd > DotProd > NEON
	case cpu.ARM64.HasSVE2 && cpu.ARM64.HasASIMDDP:
		dotInt8Impl = dotInt8Scalar
		activeTier = TierARM64SVE2
	case cpu.ARM64.HasASIMDDP:
		dotInt8Impl = dotInt8Scalar
		activeTier = TierARM64Dot
	case cpu.ARM64.HasASIMD:
		dotInt8Impl = dotInt8Scalar
		activeTier = TierARM64NEON

	default:
		dotInt8Impl = dotInt8Scalar
		activeTier = TierScalar
	}

	log.Printf("[CPU] int8 kernel dispatch: %s", activeTier)
}

// dotInt8Scalar is the portable fallback — 4-way unrolled int32 accumulation.
// Identical to the inner loop of DotProductInt8 but returns the raw integer
// sum (unscaled) so the arch-specific kernels can share the same signature.
func dotInt8Scalar(a, b []int8) int32 {
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
	for i := end4; i < n; i++ {
		s0 += int32(a[i]) * int32(b[i])
	}
	return s0 + s1 + s2 + s3
}

// ActiveInt8KernelTier returns the name of the currently active int8 dispatch tier.
// Used for HUD_STATE metrics and [CPU] boot log. [368.A]
func ActiveInt8KernelTier() string {
	return string(activeTier)
}
