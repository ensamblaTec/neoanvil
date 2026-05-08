package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/cache"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/chunker"
)

const (
	defaultMaxParallel = 10
	maxParallelCap     = 50
)

// fileResult holds the outcome for a single file in the map phase.
type fileResult struct {
	file       string
	original   string
	refactored string
	tokens     int
	err        error
}

// mapReduceRefactor implements the map_reduce_refactor action.
//
// Map phase: up to max_parallel goroutines, each file → BuildBlock1 (shared cache) +
// AssemblePrompt + POST to DeepSeek. MCP progress notifications emitted after each file.
// Reduce phase: unified diff-style Markdown report.
// Partial failure: failed files collected, batch continues.
// dry_run: plan only — no API calls.
func mapReduceRefactor(s *state, id any, args map[string]any) map[string]any {
	instructions, _ := args["refactor_instructions"].(string)
	if instructions == "" {
		instructions, _ = args["target_prompt"].(string)
	}

	maxPar := defaultMaxParallel
	if v, ok := args["max_parallel"].(float64); ok && v > 0 {
		maxPar = min(int(v), maxParallelCap)
	}

	dryRun, _ := args["dry_run"].(bool)

	var files []string
	if raw, ok := args["files"].([]any); ok {
		for _, f := range raw {
			if p, ok := f.(string); ok {
				files = append(files, p)
			}
		}
	}

	// Extract MCP progress token if caller supplied _meta.
	var progressToken any
	if meta, ok := args["_meta"].(map[string]any); ok {
		progressToken = meta["progressToken"]
	}

	if s.client == nil {
		return ok(id, textContent(fmt.Sprintf(
			"[deepseek/map_reduce_refactor] stub — client not initialised. files:%d", len(files))))
	}

	totalFiles := len(files)
	estimatedTokens := totalFiles * 2000 // rough 2K tokens per file

	if dryRun {
		return ok(id, textContent(fmt.Sprintf(
			"[dry_run] map_reduce_refactor plan:\n  files: %d\n  max_parallel: %d\n  estimated_tokens: ~%d\n  instructions_len: %d",
			totalFiles, maxPar, estimatedTokens, len(instructions))))
	}

	builder := cache.NewBuilder("You are a senior code refactor assistant.", "", 80000, time.Hour)

	// [375.C] Smoke-test-before-bulk: when > 5 files, test the first file
	// alone. If it fails or returns unparseable output, abort the batch.
	if len(files) > 5 && s.client != nil {
		smokeFile := files[0]
		smokeData, serr := os.ReadFile(smokeFile) //nolint:gosec // G304-CLI-CONSENT
		if serr == nil {
			block1, _, _ := builder.BuildBlock1([]string{smokeFile})
			assembled := builder.AssemblePrompt(block1, instructions+"\n\nFile: "+smokeFile+"\n```\n"+string(smokeData)+"\n```")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			resp, cerr := s.client.Call(ctx, deepseek.CallRequest{Prompt: assembled, MaxTokens: 1000})
			cancel()
			if cerr != nil || resp == nil || len(resp.Text) < 10 {
				return ok(id, textContent(fmt.Sprintf(
					"⚠️ SMOKE_TEST_ABORT: first file %s failed smoke test (err=%v, response_len=%d). Aborting batch of %d files to avoid wasting tokens.",
					smokeFile, cerr, func() int { if resp != nil { return len(resp.Text) }; return 0 }(), len(files))))
			}
			s.recordAPICall(resp)
		}
	}

	sem := make(chan struct{}, maxPar)

	results := make([]fileResult, len(files))
	var wg sync.WaitGroup
	var progress atomic.Int64

	for i, path := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: operator-supplied paths
			if err != nil {
				results[i] = fileResult{file: path, err: err}
			} else {
				original := string(data)
				block1, _, _ := builder.BuildBlock1([]string{path})
				ch := chunker.NewASTChunker(2000)
				chunks := ch.Chunk(original)

				var sb strings.Builder
				totalTok := 0
				model, thinking := parseModelAndThinking(args)
				for _, chunk := range chunks {
					assembled := builder.AssemblePrompt(block1,
						fmt.Sprintf("%s\n\n---FILE: %s---\n%s", instructions, path, chunk.Body))
					resp, callErr := s.client.Call(context.Background(), deepseek.CallRequest{
						Action:    "map_reduce_refactor",
						Prompt:    assembled,
						Mode:      deepseek.SessionModeEphemeral,
						MaxTokens: 4096,
						Model:     model,
						Thinking:  thinking,
					})
					if callErr != nil {
						sb.WriteString(fmt.Sprintf("[chunk error: %v]\n", callErr))
					} else {
						sb.WriteString(resp.Text)
						totalTok += resp.InputTokens + resp.OutputTokens
						s.recordAPICall(resp) // [ÉPICA 151.E]
					}
				}
				results[i] = fileResult{
					file:       path,
					original:   original,
					refactored: sb.String(),
					tokens:     totalTok,
				}
			}

			done := progress.Add(1)
			if s.notify != nil && progressToken != nil {
				s.notify(map[string]any{
					"jsonrpc": "2.0",
					"method":  "notifications/progress",
					"params": map[string]any{
						"progressToken": progressToken,
						"progress":      done,
						"total":         int64(totalFiles),
					},
				})
			}
		}()
	}
	wg.Wait()

	// Reduce: build Markdown diff report.
	var report strings.Builder
	var failedFiles []string
	totalTokens := 0
	partialResults := false

	report.WriteString("# map_reduce_refactor Report\n\n")
	for _, r := range results {
		if r.err != nil {
			failedFiles = append(failedFiles, r.file)
			partialResults = true
			report.WriteString(fmt.Sprintf("## ❌ %s\n\nError: %v\n\n", r.file, r.err))
			continue
		}
		totalTokens += r.tokens
		report.WriteString(fmt.Sprintf("## %s\n\n```diff\n%s\n```\n\n", r.file, diffSnippet(r.original, r.refactored)))
	}

	summary := fmt.Sprintf("files_processed=%d failed=%d tokens_used=%d partial_results=%v\n\n%s",
		len(results)-len(failedFiles), len(failedFiles), totalTokens, partialResults, report.String())

	return ok(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": summary}},
	})
}

// diffSnippet produces a minimal +/- diff-style snippet for display.
func diffSnippet(original, refactored string) string {
	if original == refactored {
		return "(no changes)"
	}
	origLines := strings.Split(original, "\n")
	newLines := strings.Split(refactored, "\n")
	var sb strings.Builder
	max := len(origLines)
	if len(newLines) > max {
		max = len(newLines)
	}
	shown := 0
	for i := 0; i < max && shown < 10; i++ {
		orig := ""
		if i < len(origLines) {
			orig = origLines[i]
		}
		nw := ""
		if i < len(newLines) {
			nw = newLines[i]
		}
		if orig != nw {
			if orig != "" {
				sb.WriteString("- " + orig + "\n")
			}
			if nw != "" {
				sb.WriteString("+ " + nw + "\n")
			}
			shown++
		}
	}
	if shown == 0 {
		sb.WriteString("(changes detected but identical line-by-line within first 10 diff lines)")
	}
	return sb.String()
}
