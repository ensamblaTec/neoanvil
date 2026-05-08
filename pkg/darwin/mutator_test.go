package darwin

import (
	"errors"
	"testing"
)

func TestParseGoBlocks_ExtractsVariants(t *testing.T) {
	input := "Here are 2 variants:\n```go\nfunc A() int { return 1 }\n```\nAnd:\n```go\nfunc B() int { return 2 }\n```\n"
	got := parseGoBlocks(input, 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("IDs should be 1 and 2, got %d and %d", got[0].ID, got[1].ID)
	}
}

func TestParseGoBlocks_RespectsMaxCount(t *testing.T) {
	input := "```go\nfunc A() {}\n```\n```go\nfunc B() {}\n```\n```go\nfunc C() {}\n```\n"
	got := parseGoBlocks(input, 2)
	if len(got) != 2 {
		t.Errorf("expected max 2 blocks, got %d", len(got))
	}
}

func TestParseGoBlocks_EmptyInput(t *testing.T) {
	got := parseGoBlocks("no code blocks here", 5)
	if len(got) != 0 {
		t.Errorf("expected 0 blocks, got %d", len(got))
	}
}

func TestParseGoBlocks_SkipsEmptyBlocks(t *testing.T) {
	input := "```go\n\n```\n```go\nfunc A() {}\n```\n"
	got := parseGoBlocks(input, 5)
	if len(got) != 1 {
		t.Errorf("expected 1 non-empty block, got %d", len(got))
	}
}

func TestEvaluateGeneration_AllCompile(t *testing.T) {
	mutations := []Mutation{
		{ID: 1, Source: "func A() {}"},
		{ID: 2, Source: "func B() {}"},
	}
	benchFn := func(src string) (BenchmarkResult, error) {
		return BenchmarkResult{Compiled: true, NsPerOp: 100, AllocsPerOp: 0}, nil
	}
	results := EvaluateGeneration(mutations, 200, benchFn, 2)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if !r.Compiles {
			t.Errorf("result %d should compile", r.MutationID)
		}
		if r.Improvement <= 0 {
			t.Errorf("result %d should show improvement vs baseline 200 ns", r.MutationID)
		}
	}
}

func TestEvaluateGeneration_BenchmarkError(t *testing.T) {
	mutations := []Mutation{{ID: 1, Source: "func Bad() {}"}}
	benchFn := func(src string) (BenchmarkResult, error) {
		return BenchmarkResult{}, errors.New("compile error")
	}
	results := EvaluateGeneration(mutations, 100, benchFn, 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Compiles {
		t.Error("expected Compiles=false on error")
	}
	if results[0].Error == "" {
		t.Error("expected Error to be set on failure")
	}
}

func TestEvaluateGeneration_ZeroBaseline_NoImprovement(t *testing.T) {
	mutations := []Mutation{{ID: 1, Source: "func A() {}"}}
	benchFn := func(src string) (BenchmarkResult, error) {
		return BenchmarkResult{Compiled: true, NsPerOp: 50}, nil
	}
	results := EvaluateGeneration(mutations, 0, benchFn, 1)
	// With baselineNs=0, improvement should not be computed (avoid div by zero)
	if results[0].Improvement != 0 {
		t.Errorf("expected 0 improvement with zero baseline, got %f", results[0].Improvement)
	}
}

func TestEvaluateGeneration_DefaultParallelism(t *testing.T) {
	mutations := []Mutation{{ID: 1, Source: "func A() {}"}}
	benchFn := func(src string) (BenchmarkResult, error) {
		return BenchmarkResult{Compiled: true, NsPerOp: 10}, nil
	}
	// maxParallel <= 0 should use default of 10
	results := EvaluateGeneration(mutations, 100, benchFn, 0)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestNewEvolutionConfigFromYAML(t *testing.T) {
	cfg := NewEvolutionConfigFromYAML("http://localhost:11434", "codellama", 3, 5, 100, 30)
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL mismatch: %q", cfg.OllamaURL)
	}
	if cfg.Model != "codellama" {
		t.Errorf("Model mismatch: %q", cfg.Model)
	}
	if cfg.Generations != 3 {
		t.Errorf("Generations: got %d, want 3", cfg.Generations)
	}
	if cfg.PopulationSize != 5 {
		t.Errorf("PopulationSize: got %d, want 5", cfg.PopulationSize)
	}
	if cfg.BenchmarkIters != 100 {
		t.Errorf("BenchmarkIters: got %d, want 100", cfg.BenchmarkIters)
	}
	if cfg.TimeoutSec != 30 {
		t.Errorf("TimeoutSec: got %d, want 30", cfg.TimeoutSec)
	}
}
