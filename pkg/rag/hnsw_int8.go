// pkg/rag/hnsw_int8.go — int8 companion search path for the HNSW graph.
// [PILAR-XXV/171 — builds on the primitives in quantize.go]
//
// Design goals:
//   - Non-invasive: the existing float32 search (Graph.Search) is untouched.
//     The int8 path is additive — callers opt in by calling SearchInt8.
//   - No WAL changes: int8 companion is rebuilt from float32 on demand, so
//     the on-disk schema (hnsw_vectors bucket) stays stable across restarts.
//   - Correctness parity: SearchInt8 returns node IDs ranked by the same
//     greedy-best-first traversal as Search, just with a different distance
//     function. Top-K results are expected to overlap ≥90% with the float32
//     path on 768-dim embeddings.
//
// When to use: benchmarks only, today. Once 170.C lands the WAL persistence
// for Int8Vectors, this path becomes the production default for workspaces
// with VectorQuant="int8".

package rag

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
)

// PopulateInt8 scans the float32 Vectors array and fills the int8 companion
// storage (Int8Vectors, Int8Scales). Safe to call multiple times — the int8
// slices are rebuilt from scratch.
//
// Runs in O(N × dim) time, roughly 5-10 ms for a 16k-node 768-dim corpus on
// an i5-10400. Callers should invoke this after bulk ingestion completes
// and before the first SearchInt8 call.
func (graph *Graph) PopulateInt8() {
	nNodes := len(graph.Nodes)
	if nNodes == 0 || graph.VecDim == 0 {
		return
	}
	if len(graph.Vectors) != nNodes*graph.VecDim {
		// Partial state — refuse rather than emit a truncated int8 view.
		return
	}
	int8Buf := make([]int8, nNodes*graph.VecDim)
	scales := make([]float32, nNodes)
	for i := 0; i < nNodes; i++ {
		start := i * graph.VecDim
		end := start + graph.VecDim
		q, s := QuantizeInt8(graph.Vectors[start:end])
		copy(int8Buf[start:end], q)
		scales[i] = s
	}
	graph.Int8Vectors = int8Buf
	graph.Int8Scales = scales
}

// Int8Populated reports whether PopulateInt8 has been called and the int8
// slices are consistent with the current node count.
func (graph *Graph) Int8Populated() bool {
	return len(graph.Int8Vectors) == len(graph.Nodes)*graph.VecDim &&
		len(graph.Int8Scales) == len(graph.Nodes) &&
		len(graph.Nodes) > 0
}

// searchInt8Hits counts the number of SearchInt8 invocations in this session.
// Exposed for BRIEFING / HUD telemetry.
var searchInt8Hits int64

// SearchInt8Count returns the count of SearchInt8 invocations this session.
func SearchInt8Count() int64 { return atomic.LoadInt64(&searchInt8Hits) }

// SearchInt8 is the int8-path analog of Search. It quantizes the query once,
// then traverses the HNSW neighborhood using CosineDistanceInt8 for every
// node-vs-query comparison.
//
// Returns node IDs ranked by ascending distance (best match first). Same
// contract as Search, so callers can swap between the two paths transparently.
func (graph *Graph) SearchInt8(ctx context.Context, queryVector []float32, topK int) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("[rag] int8 search aborted: %w", err)
	}
	if !graph.Int8Populated() {
		return nil, fmt.Errorf("[rag] int8 search: graph not populated — call PopulateInt8 first")
	}
	if topK <= 0 {
		return nil, nil
	}

	atomic.AddInt64(&searchInt8Hits, 1)

	// Quantize the query once. Its scale factor does NOT need to match the
	// node scales because CosineDistanceInt8 is scale-invariant (cosine
	// cancels the scale from both operands).
	qInt8, _ := QuantizeInt8(queryVector)

	state := searchStatePool.Get().(*SearchState)
	visited := state.Visited
	clear(visited)
	results := state.Results[:0]
	defer func() {
		state.Results = results
		searchStatePool.Put(state)
	}()

	// Start from node 0 — same entry policy as Search.
	entryVec, _ := graph.GetInt8Vector(0)
	bestDist := CosineDistanceInt8(qInt8, entryVec)
	curr := uint32(0)
	visited[curr] = bestDist

	// Greedy best-first traversal: same algorithm as Search.
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("[rag] int8 search aborted mid-traversal: %w", ctxErr)
		}
		improved := false
		for neighborID := range graph.Neighbors(curr) {
			if _, seen := visited[neighborID]; seen {
				continue
			}
			nVec, _ := graph.GetInt8Vector(neighborID)
			if nVec == nil {
				continue
			}
			dist := CosineDistanceInt8(qInt8, nVec)
			visited[neighborID] = dist
			if dist < bestDist {
				bestDist = dist
				curr = neighborID
				improved = true
			}
		}
		if !improved {
			break
		}
	}

	for id, dist := range visited {
		results = append(results, candidate{id: id, dist: dist})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].dist < results[j].dist })

	k := topK
	if k > len(results) {
		k = len(results)
	}
	ids := state.IDs[:0]
	for i := 0; i < k; i++ {
		ids = append(ids, results[i].id)
	}
	state.IDs = ids
	out := append([]uint32(nil), ids...)
	return out, nil
}
