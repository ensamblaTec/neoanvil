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
// mapReduceArgs is the parsed shape of a map_reduce_refactor invocation.
// Extracted to keep mapReduceRefactor at CC ≤ 15. [CC refactor]
type mapReduceArgs struct {
	instructions  string
	maxPar        int
	dryRun        bool
	files         []string
	progressToken any
	model         string
	thinking      *deepseek.ThinkingConfig
}

// parseMapReduceArgs extracts and validates the operator's args. Falls
// back to target_prompt when refactor_instructions is empty (legacy
// alias). Caps max_parallel at maxParallelCap.
func parseMapReduceArgs(args map[string]any) mapReduceArgs {
	mr := mapReduceArgs{maxPar: defaultMaxParallel}
	if instr, _ := args["refactor_instructions"].(string); instr != "" {
		mr.instructions = instr
	} else {
		mr.instructions, _ = args["target_prompt"].(string)
	}
	if v, ok := args["max_parallel"].(float64); ok && v > 0 {
		mr.maxPar = min(int(v), maxParallelCap)
	}
	mr.dryRun, _ = args["dry_run"].(bool)
	if raw, ok := args["files"].([]any); ok {
		for _, f := range raw {
			if p, ok := f.(string); ok {
				mr.files = append(mr.files, p)
			}
		}
	}
	if meta, ok := args["_meta"].(map[string]any); ok {
		mr.progressToken = meta["progressToken"]
	}
	mr.model, mr.thinking = parseModelAndThinking(args)
	return mr
}

// runMapReduceSmokeTest issues a single API call with the first file
// to validate the operator's instructions before fanning out to N
// files. Returns nil when smoke passes (or batch is small enough to
// skip), or a pre-formatted abort response when the smoke fails.
// [CC refactor — split out of mapReduceRefactor — 375.C]
func runMapReduceSmokeTest(s *state, id any, mr mapReduceArgs, builder *cache.StructuralCacheBuilder) map[string]any {
	if len(mr.files) <= 5 || s.client == nil {
		return nil
	}
	smokeFile := mr.files[0]
	smokeData, serr := os.ReadFile(smokeFile) //nolint:gosec // G304-CLI-CONSENT
	if serr != nil {
		return nil
	}
	block1, _, _ := builder.BuildBlock1([]string{smokeFile})
	assembled := builder.AssemblePrompt(block1, mr.instructions+"\n\nFile: "+smokeFile+"\n```\n"+string(smokeData)+"\n```")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	resp, cerr := s.client.Call(ctx, deepseek.CallRequest{Prompt: assembled, MaxTokens: 1000})
	cancel()
	if cerr != nil || resp == nil || len(resp.Text) < 10 {
		respLen := 0
		if resp != nil {
			respLen = len(resp.Text)
		}
		return ok(id, textContent(fmt.Sprintf(
			"⚠️ SMOKE_TEST_ABORT: first file %s failed smoke test (err=%v, response_len=%d). Aborting batch of %d files to avoid wasting tokens.",
			smokeFile, cerr, respLen, len(mr.files))))
	}
	s.recordAPICall(resp)
	return nil
}

// mapPhase fans out per-file refactor calls in parallel up to mr.maxPar.
// Each goroutine reads its file, chunks it via AST, and emits an
// MCP progress notification when the operator supplied a token.
// [CC refactor]
func mapPhase(s *state, mr mapReduceArgs, builder *cache.StructuralCacheBuilder) []fileResult {
	results := make([]fileResult, len(mr.files))
	sem := make(chan struct{}, mr.maxPar)
	var wg sync.WaitGroup
	var progress atomic.Int64
	totalFiles := int64(len(mr.files))
	for i, path := range mr.files {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = refactorOneFile(s, mr, builder, path)
			done := progress.Add(1)
			emitProgressNotification(s, mr.progressToken, done, totalFiles)
		}()
	}
	wg.Wait()
	return results
}

// refactorOneFile reads, AST-chunks, and refactor-prompts a single
// file. Returns a fileResult with err set on read failure.
// [CC refactor]
func refactorOneFile(s *state, mr mapReduceArgs, builder *cache.StructuralCacheBuilder, path string) fileResult {
	data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: operator-supplied paths
	if err != nil {
		return fileResult{file: path, err: err}
	}
	original := string(data)
	block1, _, _ := builder.BuildBlock1([]string{path})
	ch := chunker.NewASTChunker(2000)
	chunks := ch.Chunk(original)
	var sb strings.Builder
	totalTok := 0
	for _, chunk := range chunks {
		assembled := builder.AssemblePrompt(block1,
			fmt.Sprintf("%s\n\n---FILE: %s---\n%s", mr.instructions, path, chunk.Body))
		resp, callErr := s.client.Call(context.Background(), deepseek.CallRequest{
			Action:    "map_reduce_refactor",
			Prompt:    assembled,
			Mode:      deepseek.SessionModeEphemeral,
			MaxTokens: 4096,
			Model:     mr.model,
			Thinking:  mr.thinking,
		})
		if callErr != nil {
			sb.WriteString(fmt.Sprintf("[chunk error: %v]\n", callErr))
			continue
		}
		sb.WriteString(resp.Text)
		totalTok += resp.InputTokens + resp.OutputTokens
		s.recordAPICall(resp) // [ÉPICA 151.E]
	}
	return fileResult{file: path, original: original, refactored: sb.String(), tokens: totalTok}
}

// emitProgressNotification sends an MCP progress notification when
// the caller passed a progress token in _meta. No-op otherwise.
// [CC refactor]
func emitProgressNotification(s *state, token any, done, total int64) {
	if s.notify == nil || token == nil {
		return
	}
	s.notify(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params": map[string]any{
			"progressToken": token,
			"progress":      done,
			"total":         total,
		},
	})
}

func mapReduceRefactor(s *state, id any, args map[string]any) map[string]any {
	mr := parseMapReduceArgs(args)
	if s.client == nil {
		return ok(id, textContent(fmt.Sprintf(
			"[deepseek/map_reduce_refactor] stub — client not initialised. files:%d", len(mr.files))))
	}
	totalFiles := len(mr.files)
	if mr.dryRun {
		estimatedTokens := totalFiles * 2000 // rough 2K tokens per file
		return ok(id, textContent(fmt.Sprintf(
			"[dry_run] map_reduce_refactor plan:\n  files: %d\n  max_parallel: %d\n  estimated_tokens: ~%d\n  instructions_len: %d",
			totalFiles, mr.maxPar, estimatedTokens, len(mr.instructions))))
	}
	builder := cache.NewBuilder("You are a senior code refactor assistant.", "", 80000, time.Hour)
	// [375.C] Smoke-test-before-bulk: pay one call to confirm the
	// instructions parse before fanning out to N files.
	if abortResp := runMapReduceSmokeTest(s, id, mr, builder); abortResp != nil {
		return abortResp
	}
	results := mapPhase(s, mr, builder)

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
