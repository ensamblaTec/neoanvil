// cmd/neo-migrate-quant — offline companion-vector builder + memory report.
// [Épica 170.D]
//
// Loads an existing HNSW WAL from a workspace, builds the int8 and binary
// companion arrays in memory, and prints the observed RAM overhead + a
// recall/speed tradeoff summary so operators can decide whether to flip
// `rag.vector_quant` in neo.yaml.
//
// This command does NOT mutate disk. The float32 Vectors stay
// authoritative in the WAL; the int8/binary arrays are derived views
// rebuilt on every neo-mcp boot (when `rag.vector_quant != "float32"`).
//
// Usage (requires neo-mcp stopped — bbolt WAL is exclusive-locked):
//   # Stop the child first:
//   curl -X POST http://127.0.0.1:9000/api/v1/workspaces/stop/<workspace-id>
//
//   # Then run the report:
//   go run ./cmd/neo-migrate-quant --workspace /path/to/workspace
//   go run ./cmd/neo-migrate-quant --workspace /path/to/ws --mode binary
//   go run ./cmd/neo-migrate-quant --workspace /path/to/ws --bench 1000
//
//   # Restart neo-mcp after inspection:
//   curl -X POST http://127.0.0.1:9000/api/v1/workspaces/start/<workspace-id>
//
// Exit codes:
//   0  report written, no errors
//   1  workspace / WAL not found or unreadable
//   2  graph empty (< 1 node) — nothing to report

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/rag"
)

func main() {
	workspace := flag.String("workspace", "", "absolute path to the workspace (contains .neo/db/hnsw.db)")
	mode := flag.String("mode", "both", "companion mode to exercise: int8 | binary | both")
	bench := flag.Int("bench", 100, "number of random search queries to time (0 disables bench)")
	flag.Parse()

	if *workspace == "" {
		log.Println("[migrate-quant] --workspace is required")
		os.Exit(1)
	}
	dbPath := filepath.Join(*workspace, ".neo", "db", "hnsw.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		log.Printf("[migrate-quant] WAL not found at %s", dbPath)
		os.Exit(1)
	}

	wal, err := rag.OpenWAL(dbPath)
	if err != nil {
		log.Printf("[migrate-quant] failed to open WAL: %v", err)
		os.Exit(1)
	}
	defer func() { _ = wal.Close() }()

	ctx := context.Background()
	graph, err := wal.LoadGraph(ctx)
	if err != nil {
		log.Printf("[migrate-quant] failed to load graph: %v", err)
		os.Exit(1)
	}
	if len(graph.Nodes) == 0 {
		log.Println("[migrate-quant] graph is empty — nothing to migrate")
		os.Exit(2)
	}

	fmt.Printf("─────────── NeoAnvil Quantization Report ───────────\n\n")
	fmt.Printf("Workspace:  %s\n", *workspace)
	fmt.Printf("WAL:        %s\n", dbPath)
	fmt.Printf("Nodes:      %d\n", len(graph.Nodes))
	fmt.Printf("VecDim:     %d\n", graph.VecDim)
	baselineBytes := len(graph.Vectors) * 4
	fmt.Printf("float32:    %s (baseline)\n\n", fmtBytes(baselineBytes))

	var int8Start, binStart runtime.MemStats
	switch *mode {
	case "int8", "both":
		runtime.ReadMemStats(&int8Start)
		t0 := time.Now()
		graph.PopulateInt8()
		dur := time.Since(t0)
		overhead := len(graph.Int8Vectors) + len(graph.Int8Scales)*4
		fmt.Printf("int8:       %s overhead (%.1f%% of baseline)\n",
			fmtBytes(overhead), float64(overhead)*100.0/float64(baselineBytes))
		fmt.Printf("            populate latency: %v\n", dur.Round(time.Millisecond))
		if *bench > 0 {
			p50Int8, p99Int8 := benchmarkSearch(ctx, graph, *bench, "int8")
			fmt.Printf("            search p50=%v p99=%v (over %d queries)\n\n",
				p50Int8.Round(time.Microsecond), p99Int8.Round(time.Microsecond), *bench)
		} else {
			fmt.Println()
		}
	}
	if *mode == "binary" || *mode == "both" {
		runtime.ReadMemStats(&binStart)
		t0 := time.Now()
		graph.PopulateBinary()
		dur := time.Since(t0)
		overhead := len(graph.BinaryVectors) * 8
		fmt.Printf("binary:     %s overhead (%.1f%% of baseline)\n",
			fmtBytes(overhead), float64(overhead)*100.0/float64(baselineBytes))
		fmt.Printf("            populate latency: %v\n", dur.Round(time.Millisecond))
		if *bench > 0 {
			p50Bin, p99Bin := benchmarkSearch(ctx, graph, *bench, "binary")
			fmt.Printf("            search p50=%v p99=%v (over %d queries)\n\n",
				p50Bin.Round(time.Microsecond), p99Bin.Round(time.Microsecond), *bench)
		} else {
			fmt.Println()
		}
	}

	if *bench > 0 {
		p50F, p99F := benchmarkSearch(ctx, graph, *bench, "float32")
		fmt.Printf("float32:    search p50=%v p99=%v (over %d queries, baseline)\n\n",
			p50F.Round(time.Microsecond), p99F.Round(time.Microsecond), *bench)
	}

	fmt.Printf("─────────── Guidance ───────────\n")
	fmt.Println("• Default: keep `rag.vector_quant: float32` in neo.yaml.")
	fmt.Println("• Flip to `int8` only if baseline heap > 300 MB (corpus > 50k nodes).")
	fmt.Println("• Flip to `binary` for massive corpora (>200k nodes) with hybrid")
	fmt.Println("  coarse-then-rerank where 3-5% recall loss is acceptable.")
	fmt.Println()
	fmt.Println("No changes were written to disk. Companion arrays rebuild at every boot.")
}

// benchmarkSearch runs N random queries against the requested search path
// and returns p50/p99 wall-clock latency. [Épica 170.D]
func benchmarkSearch(ctx context.Context, g *rag.Graph, n int, path string) (p50, p99 time.Duration) {
	rng := rand.New(rand.NewSource(42))
	query := make([]float32, g.VecDim)
	samples := make([]time.Duration, 0, n)

	for range n {
		for j := range query {
			query[j] = rng.Float32()*2 - 1
		}
		var t0 time.Time
		var err error
		switch path {
		case "int8":
			t0 = time.Now()
			_, err = g.SearchInt8(ctx, query, 16)
		case "binary":
			t0 = time.Now()
			_, err = g.SearchBinary(ctx, query, 16)
		default:
			t0 = time.Now()
			_, err = g.Search(ctx, query, 16, nil)
		}
		if err != nil {
			continue
		}
		samples = append(samples, time.Since(t0))
	}

	if len(samples) == 0 {
		return 0, 0
	}
	// Simple insertion sort — n is small (default 100).
	for i := 1; i < len(samples); i++ {
		for j := i; j > 0 && samples[j-1] > samples[j]; j-- {
			samples[j-1], samples[j] = samples[j], samples[j-1]
		}
	}
	p50 = samples[len(samples)/2]
	p99 = samples[(len(samples)*99)/100]
	return
}

func fmtBytes(n int) string {
	const (
		kb = 1024
		mb = kb * 1024
	)
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
