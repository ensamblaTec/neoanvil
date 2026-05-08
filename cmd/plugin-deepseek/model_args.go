// model_args.go — shared helpers for parsing the model + thinking
// overrides that every deepseek/* tool action accepts.
//
// Phase 4 audit fix (2026-05-01): the canonical DeepSeek API exposes a
// unified `thinking` parameter (type + reasoning_effort) plus a
// per-request `model` selector. Previously the plugin sent
// `deepseek-chat` (deprecated alias, thinking disabled) for every call
// with no override. These helpers expose those knobs at the tool layer
// so callers can route security-critical audits to v4-pro+max while
// leaving cheap distill calls on flash defaults.

package main

import "github.com/ensamblatec/neoanvil/pkg/deepseek"

// parseModelAndThinking pulls `model` and `reasoning_effort` from the
// incoming MCP tool args. Both are optional; absent values fall through
// to client / server defaults. `thinking_type` is also accepted for
// callers that want to explicitly disable reasoning on a per-call basis.
func parseModelAndThinking(args map[string]any) (string, *deepseek.ThinkingConfig) {
	model, _ := args["model"].(string)

	t, _ := args["thinking_type"].(string)
	e, _ := args["reasoning_effort"].(string)
	if t == "" && e == "" {
		return model, nil
	}
	cfg := &deepseek.ThinkingConfig{Type: t, ReasoningEffort: e}
	return model, cfg
}

// formatUsageLine builds the metadata header for tool responses.
// Standardized across actions so log parsers and the dispatch layer
// can extract token counts uniformly.
//
// Format:
//
//	tokens=N reasoning=M cache_hit=K cache_miss=L thread_id=X model=Y
//
// Empty fields are omitted (e.g. thread_id only on threaded mode).
func formatUsageLine(resp *deepseek.CallResponse, prefix string) string {
	if resp == nil {
		return prefix
	}
	out := prefix
	if resp.InputTokens > 0 || resp.OutputTokens > 0 {
		out += " tokens=" + itoa(resp.InputTokens+resp.OutputTokens)
	}
	if resp.ReasoningTokens > 0 {
		out += " reasoning=" + itoa(resp.ReasoningTokens)
	}
	if resp.CacheHitTokens > 0 {
		out += " cache_hit=" + itoa(resp.CacheHitTokens)
	}
	if resp.CacheMissTokens > 0 {
		out += " cache_miss=" + itoa(resp.CacheMissTokens)
	}
	if resp.ThreadID != "" {
		out += " thread_id=" + resp.ThreadID
	}
	if resp.ModelUsed != "" {
		out += " model=" + resp.ModelUsed
	}
	if resp.SystemFingerprint != "" {
		out += " fp=" + resp.SystemFingerprint
	}
	return out
}

// itoa avoids pulling in strconv just for these helpers; identical to
// strconv.Itoa for non-negative ints which is all this layer produces.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + itoa(-n)
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
