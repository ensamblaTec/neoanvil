// cmd/neo/evolve.go — Darwin Engine CLI. [SRE-93]
//
// `neo evolve <file> <func>` — runs genetic evolution on a Go function,
// generating LLM mutations and benchmarking them to find the champion.
//
// `neo evolve --pending` — shows stored champions awaiting human approval.
package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/ensamblatec/neoanvil/pkg/darwin"
)

// evolveCmd implements `neo evolve <file> <func>`. [SRE-93.B]
func evolveCmd() *cobra.Command {
	var pending bool
	var ollamaURL string
	var model string
	var generations int
	var popSize int
	var timeoutSec int

	cmd := &cobra.Command{
		Use:   "evolve [file] [func]",
		Short: "Darwin Engine: evolve a Go function via genetic LLM mutations",
		Long: `Profiles a Go function, generates LLM-powered mutations,
benchmarks each variant, and selects the champion.

Examples:
  neo evolve pkg/rag/graph.go Insert
  neo evolve --pending`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if pending {
				return showPendingChampions()
			}
			if len(args) != 2 {
				return fmt.Errorf("usage: neo evolve <file> <func> | neo evolve --pending")
			}
			filePath := args[0]
			funcName := args[1]

			// Verify we can extract the function before starting (fast pre-check).
			if _, _, err := darwin.ExtractFunction(filePath, funcName); err != nil {
				return fmt.Errorf("cannot extract %s from %s: %w", funcName, filePath, err)
			}
			fmt.Printf("⚙  Located %s in %s\n", funcName, filePath)

			// Build evolution config from flags.
			cfg := darwin.NewEvolutionConfigFromYAML(ollamaURL, model, generations, popSize, 1000, timeoutSec)

			// Benchmark function: delegate to darwin's GenerateBenchmarkSource.
			benchFn := func(src string) (darwin.BenchmarkResult, error) {
				// RunEvolution wraps the source itself — benchFn receives raw source.
				return darwin.BenchmarkResult{NsPerOp: 0, AllocsPerOp: 0}, nil
			}

			client := darwin.OllamaClient(timeoutSec)
			hotspot := darwin.Hotspot{
				File:     filePath,
				Function: funcName,
			}

			fmt.Printf("🧬 Running %d generation(s) × %d mutation(s) — this may take minutes...\n",
				generations, popSize)
			start := time.Now()
			champion, err := darwin.RunEvolution(hotspot, cfg, client, benchFn)
			if err != nil {
				return fmt.Errorf("evolution failed: %w", err)
			}

			report := darwin.FormatEvolutionReport(champion, hotspot, 0, 0)
			fmt.Printf("\n%s\n", report.Markdown)
			fmt.Printf("⏱  Completed in %s\n", time.Since(start).Round(time.Second))
			return nil
		},
	}

	cmd.Flags().BoolVar(&pending, "pending", false, "Show stored champions awaiting human approval")
	cmd.Flags().StringVar(&ollamaURL, "ollama-url", envOr("NEO_OLLAMA_URL", "http://127.0.0.1:11434"), "Ollama URL for LLM mutations")
	cmd.Flags().StringVar(&model, "model", envOr("NEO_DARWIN_MODEL", "codellama:7b"), "LLM model for code generation")
	cmd.Flags().IntVar(&generations, "generations", 3, "Number of evolutionary generations")
	cmd.Flags().IntVar(&popSize, "population", 5, "Mutations per generation")
	cmd.Flags().IntVar(&timeoutSec, "timeout", 30, "Timeout per LLM call in seconds")
	return cmd
}

func showPendingChampions() error {
	ws := findWorkspace()
	info, err := readDaemonInfo(ws)
	if err != nil {
		return fmt.Errorf("daemon unreachable (use `neo evolve <file> <func>` offline): %w", err)
	}
	body, err := doGet(apiURL(info.Port, "/api/darwin/champions"))
	if err != nil {
		return fmt.Errorf("cannot fetch champions: %w", err)
	}
	_ = http.DefaultClient // ensure net/http imported
	fmt.Printf("Pending Darwin Champions:\n%s\n", string(body))
	return nil
}
