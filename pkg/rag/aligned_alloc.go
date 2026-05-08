// pkg/rag/aligned_alloc.go — Cache-line aligned float32 slice allocator. [Épica 306.A]
//
// Why alignment matters: a 768-dim float32 vector spans 3072 bytes = 48 cache
// lines. If the first element falls mid-line the CPU fetches one extra cache
// line on every sequential read. Alignment to 64 bytes eliminates that extra
// fetch, improving throughput on SIMD-width reads (AVX2 = 32 bytes, AVX-512
// = 64 bytes).
package rag

import "unsafe"

// alignedFloat32Slice allocates a []float32 of length n whose first element
// is aligned to a 64-byte cache-line boundary. The backing array is larger
// than necessary; the returned slice is pre-trimmed to exactly n elements.
func alignedFloat32Slice(n int) []float32 {
	if n <= 0 {
		return nil
	}
	// Allocate n*4 bytes + 63 bytes of padding to guarantee that a 64-byte
	// aligned address exists within the allocation.
	raw := make([]byte, n*4+63)
	// Round up to the next 64-byte boundary.
	ptr := (uintptr(unsafe.Pointer(&raw[0])) + 63) &^ 63
	return unsafe.Slice((*float32)(unsafe.Pointer(ptr)), n) //nolint:gosec // G103-UNSAFE-ALIGNED: controlled allocation for cache-line alignment; ptr is derived from a live Go allocation and bounded to n elements
}
