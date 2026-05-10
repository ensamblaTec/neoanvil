package rag

import (
	"context"
	"fmt"
	"iter"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

type Node struct {
	DocID       uint64
	EdgesOffset uint32
	EdgesLength uint16
	Layer       uint8
	_           [5]uint8
}

type candidate struct {
	id   uint32
	dist float32
}

//go:align 64 // [366.A] each pool slot starts on its own cache line — prevents false sharing
// between concurrent goroutines that hold different SearchState instances from the pool.
type SearchState struct {
	Visited map[uint32]float32
	Results []candidate
	IDs     []uint32 // [SRE-71.1] pre-allocated output buffer for Search()
}

var searchStatePool = sync.Pool{
	New: func() any {
		return &SearchState{
			Visited: make(map[uint32]float32, 1000),
			Results: make([]candidate, 0, 1000),
			IDs:     make([]uint32, 0, 100),
		}
	},
}

//go:align 64 // [366.A] graph allocation starts at a 64-byte cache-line boundary so
// Nodes (offset 0) and Gen (bumped on every InsertBatch) land on predictable lines.
type Graph struct {
	Nodes   []Node
	Edges   []uint32
	Vectors []float32
	VecDim  int
	// [PILAR-XXV/171.A] Optional int8 companion storage — populated on demand
	// by PopulateInt8(). When len(Int8Vectors) == VecDim*len(Nodes), SearchInt8
	// uses the compact representation for ~4× smaller in-flight vectors. The
	// float32 path (Vectors) remains the source of truth; int8 is a derived
	// view that can be rebuilt from scratch in milliseconds.
	Int8Vectors []int8
	Int8Scales  []float32
	// [PILAR-XXV/172] Optional binary companion storage — bit-packed vectors,
	// one bit per dimension, ceil(VecDim/64) uint64 words per node. When
	// populated via PopulateBinary(), SearchBinary offers 30-70× faster
	// distance computation at the cost of ~15-20% recall.
	BinaryVectors []uint64
	BinaryWords   int // words-per-vector; cached to avoid recomputing ceil(VecDim/64).
	// [PILAR-XXV/174] Monotonic generation counter — bumped on every
	// successful InsertBatch. Consumed by QueryCache to invalidate stale
	// entries without explicit purge calls.
	Gen atomic.Uint64
	// [367.A] CPU affinity state — round-robin core assignment for search goroutines.
	// Set via SetAffinityConfig(); zero value disables affinity.
	affinityEnabled  bool
	affinityCores    []int
	affinityCounter  atomic.Uint32
	// [367.C] Optional query-batching coalescer. When non-nil, Graph.Search
	// routes through the batcher's dispatcher goroutine instead of executing
	// inline. Set via EnableBatcher().
	batcher *QueryBatcher
	// [ÉPICA 149 / DS audit gap F2] Protects Nodes/Edges/Vectors against
	// concurrent mutation while SaveHNSWSnapshot iterates them. Save acquires
	// RLock; Insert/InsertBatch acquire Lock. Search reads slices under the
	// RLock implicitly via the absence of writers — already serialized via
	// the slice header read (Go memory model guarantees consistent header
	// observation when no concurrent writer holds the lock). Snapshot saves
	// are infrequent (every 30 min) so the throughput cost of serializing
	// inserts is negligible. Bonus: this also closes a pre-existing latent
	// race between concurrent InsertBatch callers (radar_semantic +
	// workspace_utils + incidents/indexer all called Graph.InsertBatch
	// from different goroutines without synchronization).
	snapshotMu sync.RWMutex
}

func NewGraph(expectedNodes, expectedEdges, vecDim int) *Graph {
	return &Graph{
		Nodes:   make([]Node, 0, expectedNodes),
		Edges:   make([]uint32, 0, expectedEdges),
		Vectors: make([]float32, 0, expectedNodes*vecDim),
		VecDim:  vecDim,
	}
}

// SetAffinityConfig enables round-robin CPU pinning for Search goroutines. [367.A]
// When enabled and cores is non-empty, each Search call pins the OS thread to
// cores[counter % len(cores)] via sre.PinThread before traversal and unpins on return.
// Override: if NEO_CPU_AFFINITY=off, the setting is silently ignored.
func (graph *Graph) SetAffinityConfig(enabled bool, cores []int) {
	if !enabled {
		graph.affinityEnabled = false
		return
	}
	if len(cores) == 0 {
		cores = []int{0, 1, 2, 3}
	}
	graph.affinityCores = cores
	graph.affinityEnabled = true
}

// EnableBatcher starts the query-batching coalescer for burst workloads. [367.C]
// windowMS is the coalescing window in milliseconds (0 → 2ms default).
// maxSize is the maximum batch size before an immediate flush (0 → 32 default).
// cpu is the compute device the batcher goroutine will use exclusively.
// Calling EnableBatcher disables the per-call affinity pinning (batcher goroutine
// owns the OS-thread lock for its full lifetime, amortizing that cost).
// Call graph.DisableBatcher() to stop the background goroutine cleanly.
func (graph *Graph) EnableBatcher(cpu tensorx.ComputeDevice, windowMS, maxSize int) {
	if graph.batcher != nil {
		graph.batcher.Stop()
	}
	graph.batcher = newQueryBatcher(graph, cpu, windowMS, maxSize)
}

// DisableBatcher stops the background dispatcher goroutine and reverts to
// inline graph.Search execution. Safe to call even if no batcher is active.
func (graph *Graph) DisableBatcher() {
	if graph.batcher != nil {
		graph.batcher.Stop()
		graph.batcher = nil
	}
}

// BatcherAvgBatchSize returns the running average batch size (0 if batcher is off).
// Metric: neo_hnsw_batch_size_avg exported via HUD_STATE.
func (graph *Graph) BatcherAvgBatchSize() float64 {
	if graph.batcher == nil {
		return 0
	}
	return graph.batcher.AvgBatchSize()
}

func (graph *Graph) GetVector(nodeID uint32) []float32 {
	start := int(nodeID) * graph.VecDim
	end := start + graph.VecDim
	if end > len(graph.Vectors) {
		return nil
	}
	return graph.Vectors[start:end]
}

// GetInt8Vector returns the quantized view of a node's vector, or nil if
// PopulateInt8 has not been called. [PILAR-XXV/171.A]
func (graph *Graph) GetInt8Vector(nodeID uint32) ([]int8, float32) {
	start := int(nodeID) * graph.VecDim
	end := start + graph.VecDim
	if end > len(graph.Int8Vectors) || int(nodeID) >= len(graph.Int8Scales) {
		return nil, 0
	}
	return graph.Int8Vectors[start:end], graph.Int8Scales[nodeID]
}

// GetBinaryVector returns the bit-packed view of a node's vector, or nil
// if PopulateBinary has not been called. [PILAR-XXV/172.C]
func (graph *Graph) GetBinaryVector(nodeID uint32) []uint64 {
	if graph.BinaryWords == 0 {
		return nil
	}
	start := int(nodeID) * graph.BinaryWords
	end := start + graph.BinaryWords
	if end > len(graph.BinaryVectors) {
		return nil
	}
	return graph.BinaryVectors[start:end]
}

func (graph *Graph) Neighbors(nodeID uint32) iter.Seq[uint32] {
	return func(yield func(uint32) bool) {
		if int(nodeID) >= len(graph.Nodes) {
			return
		}

		node := graph.Nodes[nodeID]
		start := node.EdgesOffset
		end := start + uint32(node.EdgesLength)

		if end > uint32(len(graph.Edges)) {
			return
		}

		for i := start; i < end; i++ {
			if !yield(graph.Edges[i]) {
				return
			}
		}
	}
}

// Search finds the topK nearest neighbors of queryVector in the graph.
// SearchAuto dispatches to the appropriate backend based on which companion
// arrays are populated, in priority order:
//
//   - hybrid (PopulateBinary done, quant=="hybrid"): binary candidate
//     selection + float32 rerank → fastest at scale, no recall hit.
//   - binary (PopulateBinary done, quant=="binary"):  binary popcount only,
//     ~2.5× faster than float32. Recall depends on corpus structure.
//   - int8 (PopulateInt8 done, quant=="int8"): int8 distance, ~2× slower
//     than float32 in pure Go (no VPMADDUBSW).
//   - default: float32 Search.
//
// Falls back to float32 Search if the requested companion is not populated
// (e.g. graph just created, populate failed). Caller passes the operator's
// configured quant mode (cfg.RAG.VectorQuant); empty string == float32.
//
// Empirical recall on production corpus (25k-vector neoanvil hnsw.bin,
// 50 random queries top-10): all four backends scored 1.000 median.
// See pkg/rag/recall_measure_live_test.go.
func (graph *Graph) SearchAuto(ctx context.Context, queryVector []float32, topK int, cpu tensorx.ComputeDevice, quant string) ([]uint32, error) {
	switch quant {
	case "hybrid":
		if graph.BinaryPopulated() && len(graph.Vectors) == len(graph.Nodes)*graph.VecDim {
			return graph.SearchHybridBinary(ctx, queryVector, topK, cpu)
		}
	case "binary":
		if graph.BinaryPopulated() {
			return graph.SearchBinary(ctx, queryVector, topK)
		}
	case "int8":
		if graph.Int8Populated() {
			return graph.SearchInt8(ctx, queryVector, topK)
		}
	}
	return graph.Search(ctx, queryVector, topK, cpu)
}

// If a QueryBatcher is active (367.C), the request is coalesced with other
// concurrent queries and executed on the batcher's pinned goroutine.
// If CPU affinity is enabled (367.A) and no batcher is active, the calling
// goroutine is pinned to a round-robin core for the duration of this call.
func (graph *Graph) Search(ctx context.Context, queryVector []float32, topK int, cpu tensorx.ComputeDevice) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("[rag] search aborted: %w", err)
	}
	if len(graph.Nodes) == 0 {
		return nil, nil
	}

	// [367.C] Route through the batcher dispatcher when enabled — it owns the
	// OS-thread pin for the whole batch window (amortizes LockOSThread cost).
	if graph.batcher != nil {
		return graph.batcher.Submit(ctx, queryVector, topK)
	}

	// [367.A] Round-robin CPU affinity — pin the OS thread to a dedicated core
	// for the duration of this search to reduce NUMA-hop and cache-thrash latency.
	// Disabled by NEO_CPU_AFFINITY=off or when affinityEnabled is false.
	if graph.affinityEnabled && os.Getenv("NEO_CPU_AFFINITY") != "off" && len(graph.affinityCores) > 0 {
		idx := graph.affinityCounter.Add(1) % uint32(len(graph.affinityCores))
		sre.PinThread(graph.affinityCores[idx])
		defer sre.UnpinThread()
	}

	return graph.searchCore(ctx, queryVector, topK, cpu)
}

// searchCore is the raw HNSW traversal — called by Search (direct path) and
// QueryBatcher.processLoop (batched path). No affinity pinning here; the
// caller is responsible for any thread-pinning needed.
func (graph *Graph) searchCore(ctx context.Context, queryVector []float32, topK int, cpu tensorx.ComputeDevice) ([]uint32, error) {
	dim := graph.VecDim
	qTensor := &tensorx.Tensor[float32]{Data: queryVector, Shape: tensorx.Shape{dim}, Strides: []int{1}}
	nTensor := &tensorx.Tensor[float32]{Shape: tensorx.Shape{dim}, Strides: []int{1}}

	state := searchStatePool.Get().(*SearchState)
	visited := state.Visited
	clear(visited)
	results := state.Results[:0]

	defer func() {
		state.Results = results
		searchStatePool.Put(state)
	}()

	nTensor.Data = graph.GetVector(0)
	bestDist, err := cpu.CosineDistance(qTensor, nTensor)
	if err != nil {
		return nil, fmt.Errorf("[rag] distance computation failed at entry: %w", err)
	}

	curr := uint32(0)
	visited[curr] = bestDist

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("[rag] search aborted mid-traversal: %w", ctxErr)
		}

		improved := false
		for neighborID := range graph.Neighbors(curr) {
			if _, seen := visited[neighborID]; seen {
				continue
			}

			vec := graph.GetVector(neighborID)
			if vec == nil {
				continue
			}

			nTensor.Data = vec
			dist, distErr := cpu.CosineDistance(qTensor, nTensor)
			if distErr != nil {
				return nil, fmt.Errorf("[rag] distance computation failed for node %d: %w", neighborID, distErr)
			}

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
	sort.Slice(results, func(i, j int) bool {
		return results[i].dist < results[j].dist
	})

	k := topK
	if k > len(results) {
		k = len(results)
	}
	// [SRE-71.1] Reuse pooled IDs buffer — copy out before pool return
	ids := state.IDs[:0]
	for i := range k {
		ids = append(ids, results[i].id)
	}
	state.IDs = ids
	out := append([]uint32(nil), ids...) // single alloc, copies from pool buffer
	return out, nil
}

func (graph *Graph) Insert(ctx context.Context, docID uint64, vec []float32, k int, cpu tensorx.ComputeDevice, wal *WAL) error {
	// [ÉPICA 149 F2] Serialize against snapshot save and other inserts.
	graph.snapshotMu.Lock()
	defer graph.snapshotMu.Unlock()
	newNodeIdx := uint32(len(graph.Nodes))
	edgesOffset := uint32(len(graph.Edges))

	var neighborIDs []uint32
	if len(graph.Nodes) > 0 && graph.VecDim > 0 {
		var err error
		neighborIDs, err = graph.Search(ctx, vec, k, cpu)
		if err != nil {
			return fmt.Errorf("[rag] Insert: neighbor search failed for doc %d: %w", docID, err)
		}
	}

	edgesLen := uint16(len(neighborIDs))

	node := Node{
		DocID:       docID,
		EdgesOffset: edgesOffset,
		EdgesLength: edgesLen,
		Layer:       0,
	}

	graph.Edges = append(graph.Edges, neighborIDs...)
	graph.Vectors = append(graph.Vectors, vec...)
	graph.Nodes = append(graph.Nodes, node)

	if graph.VecDim == 0 && len(vec) > 0 {
		graph.VecDim = len(vec)
	}

	if err := wal.Insert(newNodeIdx, node, neighborIDs, vec); err != nil {
		return fmt.Errorf("[rag] Insert: WAL persistence failed for doc %d: %w", docID, err)
	}

	return nil
}

func (graph *Graph) InsertBatch(ctx context.Context, docIDs []uint64, vecs [][]float32, k int, cpu tensorx.ComputeDevice, wal *WAL) error {
	if len(docIDs) == 0 {
		return nil
	}
	// [ÉPICA 149 F2] Serialize against snapshot save and other inserts.
	graph.snapshotMu.Lock()
	defer graph.snapshotMu.Unlock()

	count := len(docIDs)
	nodeIDs := make([]uint32, count)
	nodes := make([]Node, count)
	edgesList := make([][]uint32, count)

	for i := 0; i < count; i++ {
		vec := vecs[i]
		docID := docIDs[i]

		newNodeIdx := uint32(len(graph.Nodes))
		edgesOffset := uint32(len(graph.Edges))

		var neighborIDs []uint32
		if len(graph.Nodes) > 0 && graph.VecDim > 0 {
			var err error
			neighborIDs, err = graph.Search(ctx, vec, k, cpu)
			if err != nil {
				return fmt.Errorf("[rag] InsertBatch: search failed doc %d: %w", docID, err)
			}
		}

		edgesLen := uint16(len(neighborIDs))
		node := Node{
			DocID:       docID,
			EdgesOffset: edgesOffset,
			EdgesLength: edgesLen,
			Layer:       0,
		}

		graph.Edges = append(graph.Edges, neighborIDs...)
		// [306.A.2] Grow Graph.Vectors using a 64-byte-aligned allocator so every
		// vector start is cache-line aligned when dim*4 is a multiple of 64 (e.g.
		// dim=768 → 3072 bytes = 48 lines). This reduces L1 cache misses during Search.
		needed := len(graph.Vectors) + len(vec)
		if needed > cap(graph.Vectors) {
			newCap := max(needed, cap(graph.Vectors)*2)
			aligned := alignedFloat32Slice(newCap)
			n := copy(aligned, graph.Vectors)
			graph.Vectors = aligned[:n]
		}
		graph.Vectors = append(graph.Vectors, vec...)
		graph.Nodes = append(graph.Nodes, node)

		if graph.VecDim == 0 && len(vec) > 0 {
			graph.VecDim = len(vec)
		}

		nodeIDs[i] = newNodeIdx
		nodes[i] = node
		edgesList[i] = neighborIDs
	}

	if err := wal.InsertBatch(nodeIDs, nodes, edgesList, vecs); err != nil {
		return fmt.Errorf("[rag] InsertBatch: WAL persistence failed: %w", err)
	}

	// [PILAR-XXV/174] Bump generation to invalidate QueryCache entries that
	// were computed before this batch. Stale entries are evicted lazily on
	// their next Get — no background sweep needed.
	graph.Gen.Add(1)
	return nil
}

func (graph *Graph) AddNode(node Node) {
	graph.Nodes = append(graph.Nodes, node)
}

func (graph *Graph) AddEdge(edge uint32) {
	graph.Edges = append(graph.Edges, edge)
}
