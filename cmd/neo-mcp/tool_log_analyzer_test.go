package main

import (
	"context"
	"strings"
	"testing"
)

const fixtureLog = `2026/04/17 08:27:19.026841 main.go:265: [BOOT] initialize NeoAnvil MCP Orchestrator
2026/04/17 08:27:19.027125 main.go:291: [BOOT] memory subsystem is being instanced (memx)
2026/04/17 08:27:19.027381 main.go:303: [BOOT] long-term memory subsystem (RAG WAL)
2026/04/17 08:27:22.095860 wal.go:394: [SRE-INFO] HNSW graph recovered: 12082 nodes
2026/04/17 08:27:22.095947 main.go:355: [BOOT] initializing WebAssembly Sandbox
2026/04/17 08:32:11.000000 main.go:800: [WARN] high latency detected in [RAG] subsystem
2026/04/17 08:32:12.000000 main.go:801: [ERROR] timeout in [RAG] subsystem
2026/04/17 08:32:13.000000 main.go:802: [ERROR] retry failed in [RAG] subsystem
2026/04/17 08:32:14.000000 main.go:900: [CRITICAL] OOM detected in [MCTS] engine`

// TestLogAnalyzerPatterns verifies pattern counting and gap detection.
func TestLogAnalyzerPatterns(t *testing.T) {
	tool := &LogAnalyzerTool{} // no embedder/graph — incident correlation will be skipped

	result, err := tool.Execute(context.Background(), map[string]any{
		"content": fixtureLog,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", result)
	}
	contents, _ := m["content"].([]map[string]any)
	if len(contents) == 0 {
		t.Fatal("expected at least one content entry")
	}
	body, _ := contents[0]["text"].(string)

	t.Log(body)

	// Should count [BOOT] events.
	if !strings.Contains(body, "[BOOT]") {
		t.Error("expected [BOOT] count in report")
	}

	// Should count [ERROR] events (2 in fixture).
	if !strings.Contains(body, "[ERROR]") {
		t.Error("expected [ERROR] count in report")
	}

	// Should detect [CRITICAL].
	if !strings.Contains(body, "[CRITICAL]") {
		t.Error("expected [CRITICAL] count in report")
	}

	// Should detect timestamp gaps (3s gap between 08:27:22 and 08:32:11 = ~289s).
	if !strings.Contains(body, "gap at") {
		t.Error("expected timestamp gap detection")
	}

	// Should list [RAG] as top error component.
	if !strings.Contains(body, "RAG") {
		t.Error("expected RAG as error component")
	}
}

// TestLogAnalyzerMissingInput verifies validation.
func TestLogAnalyzerMissingInput(t *testing.T) {
	tool := &LogAnalyzerTool{}
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Error("expected error when neither content nor log_path provided")
	}
}
