// pkg/rag/quantize_dispatch_test.go — tests for runtime CPU kernel dispatch. [368.A]
package rag

import (
	"slices"
	"strings"
	"testing"
)

// TestActiveInt8KernelTier verifies the dispatch table is initialized and returns
// a non-empty tier string matching one of the known CPUTier constants. [368.A]
func TestActiveInt8KernelTier(t *testing.T) {
	tier := ActiveInt8KernelTier()
	if tier == "" {
		t.Fatal("ActiveInt8KernelTier returned empty string")
	}
	known := []CPUTier{
		TierV4VNNI, TierV3AVX2, TierV2SSE4,
		TierARM64SVE2, TierARM64Dot, TierARM64NEON,
		TierScalar,
	}
	if slices.Contains(known, CPUTier(tier)) {
		return
	}
	// Also accept any string that contains at least one of the known arch keywords.
	for _, kw := range []string{"AVX", "SSE", "arm64", "scalar", "NEON", "VNNI"} {
		if strings.Contains(tier, kw) {
			return
		}
	}
	t.Errorf("ActiveInt8KernelTier=%q does not match any known tier", tier)
}

// TestDotInt8ImplNotNil ensures the dispatch function pointer is set. [368.A]
func TestDotInt8ImplNotNil(t *testing.T) {
	if dotInt8Impl == nil {
		t.Fatal("dotInt8Impl is nil after init — dispatch not configured")
	}
}

// TestDotInt8Scalar_Correctness verifies the scalar kernel gives the same result
// as the reference DotProductInt8 (same algorithm, used as regression guard). [368.A]
func TestDotInt8Scalar_Correctness(t *testing.T) {
	a := []int8{1, 2, 3, 4, 5, 6, 7, 8}
	b := []int8{8, 7, 6, 5, 4, 3, 2, 1}

	scalarResult := dotInt8Scalar(a, b)

	// Reference: compute manually.
	var ref int32
	for i := range a {
		ref += int32(a[i]) * int32(b[i])
	}
	if scalarResult != ref {
		t.Errorf("dotInt8Scalar(%v, %v) = %d, want %d", a, b, scalarResult, ref)
	}
}

// TestDotInt8Impl_MatchesScalar verifies the dispatched kernel produces the
// same output as the scalar reference for the current CPU tier. [368.A]
func TestDotInt8Impl_MatchesScalar(t *testing.T) {
	a := []int8{3, -1, 2, 0, 5, -3, 1, 4}
	b := []int8{1, 2, -1, 3, -2, 4, 0, -3}

	dispatchResult := dotInt8Impl(a, b)
	scalarResult := dotInt8Scalar(a, b)

	if dispatchResult != scalarResult {
		t.Errorf("dotInt8Impl=%d, dotInt8Scalar=%d (tier=%s)", dispatchResult, scalarResult, activeTier)
	}
}
