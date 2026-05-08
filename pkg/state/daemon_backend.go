// Package state — daemon backend router (Claude ↔ DeepSeek). [132.F]
package state

import (
	"log"
	"strings"
)

// backendPattern maps a description keyword to a (backend, deepseekTool) pair.
type backendPattern struct {
	keyword     string
	backend     string
	deepseekTool string
}

// eligibilityPatterns defines which task descriptions are eligible for DeepSeek. [132.F]
// Evaluated in order — first match wins. Claude is the safe default.
var eligibilityPatterns = []backendPattern{
	// Claude-always: judgment-heavy operations come first so they cannot be hijacked.
	{keyword: "certify", backend: "claude"},
	{keyword: "audit", backend: "claude"},
	{keyword: "architecture", backend: "claude"},
	{keyword: "design", backend: "claude"},
	{keyword: "blast_radius", backend: "claude"},
	{keyword: "review", backend: "claude"},
	// DeepSeek eligible: mechanical / bulk operations.
	{keyword: "boilerplate", backend: "deepseek", deepseekTool: "generate_boilerplate"},
	{keyword: "distill", backend: "deepseek", deepseekTool: "distill_payload"},
	{keyword: "summarize", backend: "deepseek", deepseekTool: "distill_payload"},
	{keyword: "document", backend: "deepseek", deepseekTool: "distill_payload"},
	{keyword: "refactor", backend: "deepseek", deepseekTool: "map_reduce_refactor"},
	{keyword: "rename", backend: "deepseek", deepseekTool: "map_reduce_refactor"},
	{keyword: "migrate", backend: "deepseek", deepseekTool: "map_reduce_refactor"},
}

// resolveByDescription returns (backend, deepseekTool) based on description keywords. [132.F]
func resolveByDescription(description string) (string, string) {
	lower := strings.ToLower(description)
	for _, p := range eligibilityPatterns {
		if strings.Contains(lower, p.keyword) {
			return p.backend, p.deepseekTool
		}
	}
	return "claude", "" // safe default
}

// ResolveSuggestedBackend determines the suggested execution backend for a task. [132.F]
//
// mode controls the routing policy ("auto", "deepseek", or "claude").
// hasKey and circuitOpen are injected by the caller so this function stays testable.
//
// Precedence:
//  1. task.Backend == "claude" → always claude.
//  2. task.Backend == "deepseek" → deepseek if key available + circuit closed; else claude (transparent fallback).
//  3. mode == "auto" (or task.Backend == "auto" or "") → eligibility table; deepseek only if key + circuit ok.
//  4. mode == "claude" → always claude.
//  5. mode == "deepseek" → deepseek if key + circuit ok; else claude.
func ResolveSuggestedBackend(task *SRETask, mode string, hasKey bool, circuitOpen bool) (backend, deepseekTool string) {
	canUseDeepSeek := hasKey && !circuitOpen

	// Explicit per-task override.
	switch task.Backend {
	case "claude":
		return "claude", ""
	case "deepseek":
		if !canUseDeepSeek {
			log.Printf("[DAEMON-BACKEND] task %s requests deepseek but key absent or circuit open — falling back to claude.", task.ID)
			return "claude", ""
		}
		_, tool := resolveByDescription(task.Description)
		return "deepseek", tool
	}

	// Policy-level routing.
	switch mode {
	case "claude":
		return "claude", ""
	case "deepseek":
		if !canUseDeepSeek {
			log.Printf("[DAEMON-BACKEND] mode=deepseek but key absent or circuit open — falling back to claude.")
			return "claude", ""
		}
		_, tool := resolveByDescription(task.Description)
		return "deepseek", tool
	default: // "auto" or empty
		b, tool := resolveByDescription(task.Description)
		if b == "deepseek" && !canUseDeepSeek {
			log.Printf("[DAEMON-BACKEND] auto: task %s eligible for deepseek but key absent or circuit open — falling back to claude.", task.ID)
			return "claude", ""
		}
		return b, tool
	}
}
