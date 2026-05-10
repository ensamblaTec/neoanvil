//go:build hnsw_live

// Live recall measurement against THIS repo's actual hnsw.bin snapshot.
// Run with: go test -tags hnsw_live -v ./pkg/rag/ -run TestRecall_Live -timeout 5m
//
// Purpose: empirical evidence for the int8/binary/hybrid plans before
// wiring any of them. Measures top-K overlap of int8/binary/hybrid search
// vs the float32 baseline on the OPERATOR'S OWN corpus, not synthetic vectors.

package rag

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/memx"
	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// loadProductionGraph attempts to read the workspace's hnsw.bin snapshot.
// Skips the test if the file is absent (CI-friendly).
func loadProductionGraph(t *testing.T) *Graph {
	t.Helper()
	path := "/home/ensamblatec/go/src/github.com/ensamblatec/neoanvil/.neo/db/hnsw.bin"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no hnsw.bin at %s: %v", path, err)
	}
	snap, err := LoadHNSWSnapshot(path)
	if err != nil {
		t.Skipf("LoadHNSWSnapshot failed: %v", err)
	}
	if snap == nil || snap.Graph == nil || len(snap.Graph.Nodes) == 0 {
		t.Skip("loaded snapshot has 0 nodes")
	}
	return snap.Graph
}

// pickQueryVectors selects N random vectors from the graph to use as queries.
// We use existing vectors so each query has a known nearest neighbour
// (itself), making top-K overlap a meaningful metric.
func pickQueryVectors(g *Graph, n int) [][]float32 {
	r := rand.New(rand.NewPCG(42, 0xdeadbeef))
	out := make([][]float32, 0, n)
	used := make(map[int]bool)
	for len(out) < n && len(used) < len(g.Nodes) {
		i := r.IntN(len(g.Nodes))
		if used[i] {
			continue
		}
		used[i] = true
		start := i * g.VecDim
		end := start + g.VecDim
		v := make([]float32, g.VecDim)
		copy(v, g.Vectors[start:end])
		out = append(out, v)
	}
	return out
}

func overlapTopK(a, b []uint32) float64 {
	set := make(map[uint32]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	hits := 0
	for _, x := range b {
		if set[x] {
			hits++
		}
	}
	if len(a) == 0 {
		return 0
	}
	return float64(hits) / float64(len(a))
}

func TestRecall_Live_Float32VsInt8VsBinary(t *testing.T) {
	g := loadProductionGraph(t)

	// Setup CPU + measure baseline graph stats
	pool := memx.NewObservablePool(
		func() *memx.F32Slab { return &memx.F32Slab{Data: make([]float32, 0, 1024)} },
		func(s *memx.F32Slab) { s.Data = s.Data[:0] },
		1024,
	)
	cpu := tensorx.NewCPUDevice(pool)

	fmt.Printf("\n┌──── HNSW recall measurement on PRODUCTION corpus ──────────────────┐\n")
	fmt.Printf("│ Source: .neo/db/hnsw.bin                                            │\n")
	fmt.Printf("│ Nodes:  %d                                                       │\n", len(g.Nodes))
	fmt.Printf("│ Vec dim: %d                                                          │\n", g.VecDim)
	fmt.Printf("│ Float32 RAM: %.1f MB                                                  │\n", float64(len(g.Vectors)*4)/1024/1024)
	fmt.Println("└────────────────────────────────────────────────────────────────────┘")

	// Populate companion storage
	popStart := time.Now()
	g.PopulateInt8()
	int8Time := time.Since(popStart)
	popStart = time.Now()
	g.PopulateBinary()
	binTime := time.Since(popStart)
	fmt.Printf("\nPopulate timings:\n")
	fmt.Printf("  int8:   %v (now %.1f MB extra: int8+scales)\n", int8Time.Round(time.Millisecond), float64(len(g.Int8Vectors)+len(g.Int8Scales)*4)/1024/1024)
	fmt.Printf("  binary: %v (now %.1f MB extra)\n", binTime.Round(time.Millisecond), float64(len(g.BinaryVectors)*8)/1024/1024)

	// Run 50 queries, measure top-10 overlap of each backend vs float32
	queries := pickQueryVectors(g, 50)
	if len(queries) < 50 {
		t.Skipf("not enough vectors in graph (%d) for 50 queries", len(queries))
	}

	const topK = 10
	ctx := context.Background()

	type measurement struct {
		name     string
		overlaps []float64
		latency  []time.Duration
	}
	int8Meas := measurement{name: "int8"}
	binMeas := measurement{name: "binary"}
	hybMeas := measurement{name: "hybrid"}

	for _, q := range queries {
		// Float32 baseline
		f32Hits, err := g.Search(ctx, q, topK, cpu)
		if err != nil {
			t.Fatalf("Float32 Search: %v", err)
		}

		// int8
		t0 := time.Now()
		i8Hits, err := g.SearchInt8(ctx, q, topK)
		i8Lat := time.Since(t0)
		if err != nil {
			t.Fatalf("Int8 Search: %v", err)
		}
		int8Meas.overlaps = append(int8Meas.overlaps, overlapTopK(f32Hits, i8Hits))
		int8Meas.latency = append(int8Meas.latency, i8Lat)

		// binary
		t0 = time.Now()
		bHits, err := g.SearchBinary(ctx, q, topK)
		bLat := time.Since(t0)
		if err != nil {
			t.Fatalf("Binary Search: %v", err)
		}
		binMeas.overlaps = append(binMeas.overlaps, overlapTopK(f32Hits, bHits))
		binMeas.latency = append(binMeas.latency, bLat)

		// hybrid (binary candidate filter + float32 rerank)
		t0 = time.Now()
		hHits, err := g.SearchHybridBinary(ctx, q, topK, cpu)
		hLat := time.Since(t0)
		if err != nil {
			t.Fatalf("Hybrid Search: %v", err)
		}
		hybMeas.overlaps = append(hybMeas.overlaps, overlapTopK(f32Hits, hHits))
		hybMeas.latency = append(hybMeas.latency, hLat)
	}

	// Compute medians
	statsLine := func(m measurement) {
		if len(m.overlaps) == 0 {
			return
		}
		sort.Float64s(m.overlaps)
		sort.Slice(m.latency, func(i, j int) bool { return m.latency[i] < m.latency[j] })
		oMedian := m.overlaps[len(m.overlaps)/2]
		oP95 := m.overlaps[(len(m.overlaps)*95)/100]
		oMin := m.overlaps[0]
		lMedian := m.latency[len(m.latency)/2]
		lP95 := m.latency[(len(m.latency)*95)/100]
		fmt.Printf("  %-7s  recall_median=%.3f  p95=%.3f  min=%.3f  lat_median=%v  lat_p95=%v\n",
			m.name, oMedian, oP95, oMin, lMedian.Round(time.Microsecond), lP95.Round(time.Microsecond))
	}

	fmt.Printf("\n┌──── Recall + Latency vs Float32 baseline (50 queries, top-%d) ───────┐\n", topK)
	statsLine(int8Meas)
	statsLine(binMeas)
	statsLine(hybMeas)
	fmt.Println("└────────────────────────────────────────────────────────────────────┘")
	fmt.Printf("\nDecision rule:\n")
	fmt.Printf("  recall_median >= 0.80  → safe to wire as opt-in\n")
	fmt.Printf("  recall_median 0.50-0.80 → marginal, use only with rerank (hybrid)\n")
	fmt.Printf("  recall_median < 0.50  → not viable for production, archive primitives\n")
}
