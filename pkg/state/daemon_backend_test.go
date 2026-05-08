// pkg/state/daemon_backend_test.go — tests for daemon backend router. [132.F]
package state

import (
	"testing"
)

// TestAutoResolvesDeepSeek verifies that mode=auto with key available routes
// a boilerplate task to deepseek. [132.F]
func TestAutoResolvesDeepSeek(t *testing.T) {
	task := &SRETask{ID: "T-1", Description: "generate_boilerplate for handler.go"}
	backend, tool := ResolveSuggestedBackend(task, "auto", true, false)
	if backend != "deepseek" {
		t.Errorf("backend = %q, want %q", backend, "deepseek")
	}
	if tool != "generate_boilerplate" {
		t.Errorf("deepseekTool = %q, want %q", tool, "generate_boilerplate")
	}
}

// TestAutoFallsBackNoKey verifies that mode=auto without a DeepSeek key
// falls back to claude even for an eligible task. [132.F]
func TestAutoFallsBackNoKey(t *testing.T) {
	task := &SRETask{ID: "T-2", Description: "refactor pkg/rag/hnsw.go for clarity"}
	backend, _ := ResolveSuggestedBackend(task, "auto", false, false)
	if backend != "claude" {
		t.Errorf("backend = %q, want %q", backend, "claude")
	}
}

// TestForcedDeepSeekFallsBackNoKey verifies that task.Backend="deepseek" with no
// key returns claude (transparent fallback). [132.F]
func TestForcedDeepSeekFallsBackNoKey(t *testing.T) {
	task := &SRETask{ID: "T-3", Description: "distill context", Backend: "deepseek"}
	backend, _ := ResolveSuggestedBackend(task, "auto", false, false)
	if backend != "claude" {
		t.Errorf("backend = %q, want %q", backend, "claude")
	}
}

// TestEligibilityPatterns verifies the eligibility table for each pattern bucket. [132.F]
func TestEligibilityPatterns(t *testing.T) {
	cases := []struct {
		desc    string
		backend string
		tool    string
	}{
		{"certify mutation in pkg/sre/healer.go", "claude", ""},
		{"audit complexity of service layer", "claude", ""},
		{"architecture decision for federation tier", "claude", ""},
		{"design new API contract", "claude", ""},
		{"review PR diff for quality", "claude", ""},
		{"generate_boilerplate CRUD handler", "deepseek", "generate_boilerplate"},
		{"boilerplate tests for planner.go", "deepseek", "generate_boilerplate"},
		{"distill session context for compression", "deepseek", "distill_payload"},
		{"summarize technical debt backlog", "deepseek", "distill_payload"},
		{"document pkg/memx exported functions", "deepseek", "distill_payload"},
		{"refactor pkg/rag/hnsw.go search path", "deepseek", "map_reduce_refactor"},
		{"rename SRETask.Status to LifecycleState", "deepseek", "map_reduce_refactor"},
		{"migrate bolt bucket schema v2", "deepseek", "map_reduce_refactor"},
	}
	for _, tc := range cases {
		b, tool := resolveByDescription(tc.desc)
		if b != tc.backend {
			t.Errorf("desc=%q: backend=%q, want %q", tc.desc, b, tc.backend)
		}
		if tool != tc.tool {
			t.Errorf("desc=%q: tool=%q, want %q", tc.desc, tool, tc.tool)
		}
	}
}

// TestCircuitOpenFallback verifies that when the DeepSeek circuit is open,
// the router falls back to claude regardless of key availability. [132.F]
func TestCircuitOpenFallback(t *testing.T) {
	task := &SRETask{ID: "T-5", Description: "distill context", Backend: "deepseek"}
	// Key is present but circuit is open.
	backend, _ := ResolveSuggestedBackend(task, "auto", true, true)
	if backend != "claude" {
		t.Errorf("backend = %q, want %q when circuit open", backend, "claude")
	}
}
