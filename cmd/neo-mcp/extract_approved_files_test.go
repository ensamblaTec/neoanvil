// cmd/neo-mcp/extract_approved_files_test.go — tests for the helper
// that parses certify result strings to find approved files. [138.E.2]
package main

import "testing"

// TestExtractApprovedFiles_HappyPath — three results: 2 approved, 1
// rejected. Helper returns the 2 approved file paths in order.
func TestExtractApprovedFiles_HappyPath(t *testing.T) {
	results := []string{
		`{"status":"Aprobado e Indexado","file":"pkg/state/daemon_trust.go"}`,
		`{"status":"Rechazado","file":"pkg/state/daemon_audit.go","reason":"AST CC>15"}`,
		`{"status":"Aprobado e Indexado","file":"cmd/neo-mcp/main.go"}`,
	}
	got := extractApprovedFiles(results)
	if len(got) != 2 {
		t.Fatalf("got %d approved, want 2: %v", len(got), got)
	}
	if got[0] != "pkg/state/daemon_trust.go" || got[1] != "cmd/neo-mcp/main.go" {
		t.Errorf("ordering wrong: %v", got)
	}
}

// TestExtractApprovedFiles_DryRunMatches — dry-run status starts with
// "Aprobado" too; helper accepts the prefix to stay robust to
// variations.
func TestExtractApprovedFiles_DryRunMatches(t *testing.T) {
	results := []string{
		`{"status":"Aprobado (dry-run)","file":"pkg/state/x.go"}`,
	}
	got := extractApprovedFiles(results)
	if len(got) != 1 {
		t.Errorf("dry-run should be approved-prefix match, got %v", got)
	}
}

// TestExtractApprovedFiles_GarbageInput — non-JSON results are
// silently skipped (legacy plain-text outputs from earlier certify
// versions don't crash the hook). The strict status whitelist also
// rejects "Aprobado" (without the "e Indexado" suffix) since the
// real certify never returns that bare form. [DeepSeek VULN-INPUT-001]
func TestExtractApprovedFiles_GarbageInput(t *testing.T) {
	results := []string{
		"not json at all",
		`{"status":"Aprobado e Indexado","file":"good.go"}`,
		`{"missing_status_field":true,"file":"x.go"}`,
		`{"status":"Aprobado e Indexado","file":""}`,
	}
	got := extractApprovedFiles(results)
	if len(got) != 1 || got[0] != "good.go" {
		t.Errorf("expected only [good.go], got %v", got)
	}
}

// TestExtractApprovedFiles_RejectsLookalikeStatuses — variants that
// START with "Aprobado" but aren't real certify approvals must NOT
// be treated as approved. Defends against [DeepSeek VULN-INPUT-001].
func TestExtractApprovedFiles_RejectsLookalikeStatuses(t *testing.T) {
	results := []string{
		`{"status":"AprobadoFalso","file":"evil1.go"}`,
		`{"status":"Aprobado pero rechazado","file":"evil2.go"}`,
		`{"status":"Aprobado-X","file":"evil3.go"}`,
		`{"status":"Aprobado e Indexado","file":"good.go"}`,
	}
	got := extractApprovedFiles(results)
	if len(got) != 1 || got[0] != "good.go" {
		t.Errorf("only \"Aprobado e Indexado\" should pass whitelist, got %v", got)
	}
}

// TestExtractApprovedFiles_Empty — nil/empty input returns empty
// slice without panic.
func TestExtractApprovedFiles_Empty(t *testing.T) {
	if got := extractApprovedFiles(nil); len(got) != 0 {
		t.Errorf("nil input: got %v, want empty", got)
	}
	if got := extractApprovedFiles([]string{}); len(got) != 0 {
		t.Errorf("empty input: got %v, want empty", got)
	}
}
