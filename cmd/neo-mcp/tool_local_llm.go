package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/llm"
)

// LocalLLMTool exposes the local Ollama LLM (Qwen 2.5-Coder 32B by default)
// as an MCP tool. Mirrors the DeepSeek plugin shape but routes traffic to
// the on-box GPU instead of the remote API. Designed for daemon-mode
// audits and any task that doesn't strictly need DeepSeek-pro quality.
//
// Key trade-off (documented in ADR-013):
//   · Local model:  $0/call, ~5-30s/audit on RTX 3090, ~80% of DS-pro quality
//   · DeepSeek API: $0.04-0.10/session, ~10-60s/audit, frontier quality
//
// The intended use is a router: trivial / mechanical / refactor work goes
// to the local model; SEV ≥ 9 audits and decisions still go to DS. The
// router lives in the agent prompt, not the tool itself — this tool is
// just the local-side dispatch surface.
type LocalLLMTool struct {
	defaultModel string
	baseURL      string
}

// NewLocalLLMTool wires the tool with the operator-configured Ollama URL
// (cfg.AI.BaseURL) and a default model. The model can still be overridden
// per-call via args["model"].
//
// Default: qwen2.5-coder:7b (4.5 GB, fits any 8 GB+ GPU + 16 GB+ system RAM).
// qwen2.5-coder:32b would be the better quality option but Ollama estimates
// 44.5 GB system memory needed, so it won't fit on a 32 GB box. Operators
// with 64 GB+ can override via args["model"] or by setting cfg.AI.LocalModel
// once that config field lands (ADR-013 follow-up).
func NewLocalLLMTool(baseURL, defaultModel string) *LocalLLMTool {
	if defaultModel == "" {
		defaultModel = "qwen2.5-coder:7b"
	}
	return &LocalLLMTool{baseURL: baseURL, defaultModel: defaultModel}
}

func (t *LocalLLMTool) Name() string { return "neo_local_llm" }

func (t *LocalLLMTool) Description() string {
	return "SRE Tool: Routes a prompt to the LOCAL Ollama LLM (default qwen2.5-coder:7b on the operator's GPU). Zero per-call cost, ~5-30s latency on a 3090-class card. Use for refactor sketches, mechanical fan-out tasks, daemon-mode audits, and anything where DeepSeek-pro frontier quality is overkill. SEV ≥ 9 security audits and architectural decisions should still route to DeepSeek (the deepseek_call plugin) per ADR-013. Returns generated text + token counts. system field is injected as the chat-style system prompt."
}

func (t *LocalLLMTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "User prompt sent to the local LLM. The Babel rule from the DeepSeek doctrine still applies: prefer English for code-related prompts.",
			},
			"model": map[string]any{
				"type":        "string",
				"description": "Optional model override. Default qwen2.5-coder:7b. Operators with ≥64 GB system RAM can use qwen2.5-coder:32b for higher quality. qwen2:0.5b (336 MB) is useful for trivial classification at 500-1000 tok/s.",
			},
			"system": map[string]any{
				"type":        "string",
				"description": "Optional system prompt prefix. When provided, the tool concatenates: SYSTEM:\\n<system>\\n\\nUSER:\\n<prompt>. Keep under 4 KB to avoid context-window overflow.",
			},
			"max_tokens": map[string]any{
				"type":        "integer",
				"description": "Optional cap on generated tokens. Default 4096; raise to 16384 for long audits, 32768 for deep reviews. Higher caps mean longer latency.",
			},
			"temperature": map[string]any{
				"type":        "number",
				"description": "Optional sampling temperature 0.0-1.0. Default 0.2 for deterministic code work.",
			},
		},
		Required: []string{"prompt"},
	}
}

func (t *LocalLLMTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	prompt, _ := args["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	model, _ := args["model"].(string)
	if model == "" {
		model = t.defaultModel
	}
	system, _ := args["system"].(string)
	maxTokens := 4096
	if v, ok := args["max_tokens"].(float64); ok && v > 0 {
		maxTokens = int(v)
	}
	temperature := 0.2
	if v, ok := args["temperature"].(float64); ok && v >= 0 {
		temperature = v
	}

	finalPrompt := prompt
	if strings.TrimSpace(system) != "" {
		finalPrompt = "SYSTEM:\n" + system + "\n\nUSER:\n" + prompt
	}

	client := llm.NewClient(t.baseURL, model, maxTokens, temperature)
	start := time.Now()
	out, err := client.Generate(finalPrompt, &llm.GenerateOpts{
		Temperature: temperature,
		MaxTokens:   maxTokens,
	})
	elapsed := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("local llm: %w", err)
	}
	// MCP requires the standard content-block envelope so Claude Code (and any
	// well-behaved MCP client) renders the result. The metadata footer keeps
	// latency / model / size visible for debugging without a separate field.
	body := fmt.Sprintf("%s\n\n---\n_model: %s · latency: %dms · prompt: %d chars · response: %d chars_",
		strings.TrimSpace(out), model, elapsed.Milliseconds(), len(finalPrompt), len(out))
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": body}},
	}, nil
}
