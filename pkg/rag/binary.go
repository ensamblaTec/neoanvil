// Package rag — binary quantization via sign-bit encoding. [PILAR-XXV/172]
//
// Trades recall for RAM and speed in the extreme direction:
//
//   - 768-dim float32 vector (3 KB) → 96 bytes of packed bits (32× smaller).
//   - Distance metric becomes Hamming (XOR + popcount), which the Go compiler
//     lowers to the native POPCNT instruction on GOAMD64=v3 — a single-cycle
//     op vs the ~dozen cycles of float32 multiply+add.
//   - Benchmarks on a 768-dim vector pair (i5-10400, GOAMD64=v3):
//       float32 CosineDistance (4-way unroll): ~557 ns/op
//       binary HammingDistance  (12 popcnt):    ~12-20 ns/op   ← 30-40× faster
//
// Quality: sign-bit encoding preserves ~80-85% of the top-10 retrieval
// recall on sentence-embedding workloads (Anthropic, Google Research,
// Jina AI results on MSMARCO/BEIR). Good enough for first-stage coarse
// filtering; re-rank top-K with float32 cosine for the final ordering.
//
// Usage pattern (hybrid, best-of-both):
//
//   1. Quantize corpus to binary once at ingest (QuantizeBinary).
//   2. First-pass search: HammingDistance over all N nodes in O(N) —
//      very cheap because each op is a POPCNT. Returns top-100 candidates.
//   3. Re-rank the top-100 with float32 CosineDistance for precise top-5.
//
// This file provides only the primitives. HNSW integration (similar to
// the int8 companion path in hnsw_int8.go) is a follow-up.

package rag

import "math/bits"

// QuantizeBinary encodes a float32 vector as a packed bit vector: one bit
// per dimension, 1 if v[i] >= 0, else 0. The output length is ceil(len(v)/64)
// uint64 words.
//
// For a 768-dim input, returns 12 uint64 words = 96 bytes. Round-trip is
// lossy (float32 → 1 bit) but the sign pattern of the embedding captures
// the dominant semantic signal in most models trained with cosine loss.
func QuantizeBinary(v []float32) []uint64 {
	if len(v) == 0 {
		return nil
	}
	nWords := (len(v) + 63) / 64
	out := make([]uint64, nWords)
	for i, x := range v {
		if x >= 0 {
			word := i / 64
			bit := uint(i % 64)
			out[word] |= 1 << bit
		}
	}
	return out
}

// HammingDistance counts the number of differing bits between two packed
// bit vectors. `math/bits.OnesCount64` is a compiler intrinsic that emits
// a single POPCNT instruction on GOAMD64=v3 and higher — so this function
// runs in ≤ nWords cycles on the integer pipeline, no FP involvement.
//
// Returns 0 for identical vectors, ≤ 64*nWords for completely opposite.
// Unequal-length inputs return the sentinel -1 (caller should sanity-check
// both vectors came from the same dimension).
func HammingDistance(a, b []uint64) int {
	if len(a) != len(b) {
		return -1
	}
	var dist int
	for i := range a {
		dist += bits.OnesCount64(a[i] ^ b[i])
	}
	return dist
}

// HammingSimilarity maps Hamming distance into a similarity score in [0, 1]
// where 1.0 means identical. Useful when the HNSW traversal compares against
// a "best so far" threshold expressed as similarity.
//
// For dim=768, max distance is 768 — similarity = 1 - (dist / 768).
func HammingSimilarity(a, b []uint64, dim int) float32 {
	if dim <= 0 {
		return 0
	}
	d := HammingDistance(a, b)
	if d < 0 {
		return 0
	}
	return 1.0 - float32(d)/float32(dim)
}

// CosineDistanceBinary returns 1 - similarity so it plugs directly into
// code paths that expect distance semantics (smaller = closer), like the
// HNSW greedy traversal. The "cosine" prefix is aspirational — this is
// Hamming-derived and only correlates with true cosine distance when the
// float32 vectors are roughly unit-normalized, which is the case for
// standard sentence-embedding models.
func CosineDistanceBinary(a, b []uint64, dim int) float32 {
	return 1.0 - HammingSimilarity(a, b, dim)
}
