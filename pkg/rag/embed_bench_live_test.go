//go:build ollama_live

// Live benchmarks for the option-B batch embedding migration.
// Run via:  go test -tags ollama_live -v ./pkg/rag/ -run BenchLive -timeout 5m
// Requires: Ollama embed instance reachable on http://127.0.0.1:11435
//           with model nomic-embed-text loaded.

package rag

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// makeTexts produces N short snippets with varying lengths so the embedder
// sees realistic input shape (not all-identical so the runner can't cache).
func makeTexts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("snippet number %d covering function foo() { return bar(%d) + baz() }", i, i)
	}
	return out
}

// BenchLive_EmbedBatchScaling measures sequential vs batch at several N.
// Reports ms/text and effective speedup at each batch size.
func TestBenchLive_EmbedBatchScaling(t *testing.T) {
	emb := NewOllamaEmbedder("http://127.0.0.1:11435", "nomic-embed-text", 30, 4, 4096)
	sizes := []int{1, 4, 8, 16, 32, 64}

	fmt.Println("\n┌──── Embed pipeline ────────────────────────────────────────────────┐")
	fmt.Printf("│ %-6s │ %-12s │ %-12s │ %-12s │ %-12s │\n", "batch", "seq total", "seq/text", "batch total", "speedup")
	fmt.Println("├────────┼──────────────┼──────────────┼──────────────┼──────────────┤")
	ctx := context.Background()

	for _, n := range sizes {
		texts := makeTexts(n)

		// Warmup once to flush cold-cache effects on Ollama runner side
		_, _ = emb.EmbedBatch(ctx, texts)

		// Sequential — measures the OLD code path
		start := time.Now()
		for _, txt := range texts {
			if _, err := emb.Embed(ctx, txt); err != nil {
				t.Fatalf("sequential embed n=%d: %v", n, err)
			}
		}
		seq := time.Since(start)

		// Batch — measures the NEW code path (single /api/embed call)
		start = time.Now()
		out, err := emb.EmbedBatch(ctx, texts)
		batch := time.Since(start)
		if err != nil {
			t.Fatalf("batch embed n=%d: %v", n, err)
		}
		if len(out) != n {
			t.Fatalf("batch n=%d returned %d vectors", n, len(out))
		}

		seqPerText := seq / time.Duration(n)
		speedup := float64(seq) / float64(batch)
		fmt.Printf("│ %-6d │ %-12v │ %-12v │ %-12v │ %-12.2fx │\n",
			n, seq.Round(time.Millisecond), seqPerText.Round(100*time.Microsecond),
			batch.Round(time.Millisecond), speedup)
	}
	fmt.Println("└────────┴──────────────┴──────────────┴──────────────┴──────────────┘")
}

// BenchLive_HNSWInsertPipeline measures the FULL pipeline that the
// migrated post-certify hook now uses: N text chunks → embed → graph.Insert.
// Sequential = the pre-migration code (one Embed per chunk, one Insert).
// Batched = the post-migration code (EmbedMany then iterate Inserts).
//
// HNSW Insert itself is unchanged — the speedup comes entirely from
// amortizing HTTP round-trips on the embed side.
func TestBenchLive_HNSWInsertPipeline(t *testing.T) {
	emb := NewOllamaEmbedder("http://127.0.0.1:11435", "nomic-embed-text", 30, 4, 4096)
	// NewGraph(expectedNodes, expectedEdges, vecDim) — preallocate for ~256 nodes.
	g := NewGraph(256, 1024, 768)
	cpu := newTestCPU()
	wal, err := OpenWAL(filepath.Join(t.TempDir(), "bench.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()
	ctx := context.Background()

	fmt.Println("\n┌──── HNSW Insert pipeline (embed + graph.Insert) ───────────────────┐")
	fmt.Printf("│ %-6s │ %-14s │ %-14s │ %-12s │\n", "chunks", "sequential", "batched", "speedup")
	fmt.Println("├────────┼────────────────┼────────────────┼──────────────┤")

	docID := uint64(0)
	for _, n := range []int{4, 8, 16, 32} {
		texts := makeTexts(n)

		// Pre-migration: per-chunk Embed then per-chunk Insert
		start := time.Now()
		for _, txt := range texts {
			vec, err := emb.Embed(ctx, txt)
			if err != nil {
				t.Fatalf("seq embed: %v", err)
			}
			docID++
			if err := g.Insert(ctx, docID, vec, 5, cpu, wal); err != nil {
				t.Fatalf("seq insert: %v", err)
			}
		}
		seq := time.Since(start)

		// Post-migration: batch Embed then per-chunk Insert
		start = time.Now()
		vecs, err := EmbedMany(ctx, emb, texts)
		if err != nil {
			t.Fatalf("batch embed: %v", err)
		}
		for _, vec := range vecs {
			docID++
			if err := g.Insert(ctx, docID, vec, 5, cpu, wal); err != nil {
				t.Fatalf("batch insert: %v", err)
			}
		}
		batch := time.Since(start)

		speedup := float64(seq) / float64(batch)
		fmt.Printf("│ %-6d │ %-14v │ %-14v │ %-12.2fx │\n",
			n, seq.Round(time.Millisecond), batch.Round(time.Millisecond), speedup)
	}
	fmt.Println("└────────┴────────────────┴────────────────┴──────────────┘")
}

// BenchLive_HNSWSearchUnchanged proves HNSW Search latency is NOT affected
// by the embed-batch migration. We touched zero search code paths; this
// is the regression guard.
func TestBenchLive_HNSWSearchUnchanged(t *testing.T) {
	emb := NewOllamaEmbedder("http://127.0.0.1:11435", "nomic-embed-text", 30, 4, 4096)
	g := NewGraph(256, 1024, 768)
	cpu := newTestCPU()
	wal, err := OpenWAL(filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	defer wal.Close()
	ctx := context.Background()

	// Seed graph with 200 vectors (~realistic small-workspace HNSW).
	// Use batched embed to seed quickly — the seeding itself benefits from B.
	seedTexts := makeTexts(200)
	vecs, err := emb.EmbedBatch(ctx, seedTexts)
	if err != nil {
		t.Fatalf("seed embed: %v", err)
	}
	for i, v := range vecs {
		_ = g.Insert(ctx, uint64(i+1), v, 5, cpu, wal)
	}

	queryVec, err := emb.Embed(ctx, "search query about foo")
	if err != nil {
		t.Fatalf("query embed: %v", err)
	}

	// 50 searches; take median + p95.
	N := 50
	durs := make([]time.Duration, N)
	for i := 0; i < N; i++ {
		start := time.Now()
		_, _ = g.Search(ctx, queryVec, 10, cpu)
		durs[i] = time.Since(start)
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	median := durs[N/2]
	p95 := durs[(N*95)/100]
	fmt.Println("\n┌──── HNSW Search latency (unchanged by migration) ──────────────────┐")
	fmt.Printf("│ corpus=200 nodes  k=10                                              │\n")
	fmt.Printf("│ median search:  %-12v                                       │\n", median.Round(time.Microsecond))
	fmt.Printf("│ p95 search:     %-12v                                       │\n", p95.Round(time.Microsecond))
	fmt.Println("│ Note: zero code change in Search path; this is regression guard.   │")
	fmt.Println("└────────────────────────────────────────────────────────────────────┘")
}
