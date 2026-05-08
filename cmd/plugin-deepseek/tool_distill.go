package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/deepseek"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/cache"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/chunker"
)

// distillPayload implements the distill_payload action.
//
// Flow:
//  1. Load file content from disk.
//  2. Chunk each file: ASTChunker for .go, LineChunker for others.
//  3. Build Block 1 via StructuralCacheBuilder (cached by SHA-256).
//  4. POST each chunk to DeepSeek and collect the distilled output.
//
// When s.client is nil the handler returns a stub (maintains test compatibility).
func distillPayload(s *state, id any, args map[string]any) map[string]any {
	prompt, _ := args["target_prompt"].(string)
	maxTok := 1000
	if v, ok := args["max_output_tokens"].(float64); ok && v > 0 {
		maxTok = int(v)
	}

	// Extract file list.
	var files []string
	if raw, ok := args["files"].([]any); ok {
		for _, f := range raw {
			if s, ok := f.(string); ok {
				files = append(files, s)
			}
		}
	}

	if s.client == nil {
		// Stub path: no API key / client initialised yet.
		return ok(id, textContent(fmt.Sprintf(
			"[deepseek/distill_payload] stub — client not initialised (no DEEPSEEK_API_KEY). "+
				"files:%d prompt_len:%d", len(files), len(prompt))))
	}

	ast := chunker.NewASTChunker(2000)
	line := chunker.NewLineChunker(2000)
	builder := cache.NewBuilder("You are a code-analysis assistant.", "", 80000, time.Hour)

	var allChunks []chunker.Chunk
	for _, path := range files {
		data, err := os.ReadFile(path) //nolint:gosec // G304-CLI-CONSENT: operator-supplied paths
		if err != nil {
			continue
		}
		src := string(data)
		var chunks []chunker.Chunk
		if strings.HasSuffix(path, ".go") {
			chunks = ast.Chunk(src)
		} else {
			chunks = line.Chunk(src)
		}
		allChunks = append(allChunks, chunks...)
	}

	if len(allChunks) == 0 {
		return ok(id, textContent("[deepseek/distill_payload] no chunks produced (empty or unreadable files)"))
	}

	block1, _, _ := builder.BuildBlock1(files)

	var sb strings.Builder
	totalTokens := 0
	cacheHit := false

	model, thinking := parseModelAndThinking(args)
	for i, ch := range allChunks {
		assembled := builder.AssemblePrompt(block1, fmt.Sprintf("%s\n\n---CHUNK %d---\n%s", prompt, i+1, ch.Body))
		resp, err := s.client.Call(context.Background(), deepseek.CallRequest{
			Action:    "distill_payload",
			Prompt:    assembled,
			Mode:      deepseek.SessionModeEphemeral,
			MaxTokens: maxTok,
			Model:     model,
			Thinking:  thinking,
		})
		if err != nil {
			sb.WriteString(fmt.Sprintf("[chunk %d error: %v]\n", i+1, err))
			continue
		}
		sb.WriteString(resp.Text)
		sb.WriteString("\n")
		totalTokens += resp.InputTokens + resp.OutputTokens
		s.recordAPICall(resp) // [ÉPICA 151.E] cache discipline aggregate
		if resp.CacheHit {
			cacheHit = true
		}
	}

	result := map[string]any{
		"chunks_processed": len(allChunks),
		"tokens_used":      totalTokens,
		"cache_hit":        cacheHit,
		"distilled_output": strings.TrimSpace(sb.String()),
	}
	return ok(id, map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": fmt.Sprintf("chunks_processed=%d tokens=%d cache_hit=%v\n%s",
					result["chunks_processed"], result["tokens_used"], result["cache_hit"],
					result["distilled_output"]),
			},
		},
	})
}

