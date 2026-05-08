package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTranscriptEmpty verifies that an empty .jsonl returns a valid (zero) report. [130.2.4]
func TestTranscriptEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	rpt, err := parseTranscript(p)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if rpt.totalTurns != 0 {
		t.Errorf("expected 0 turns, got %d", rpt.totalTurns)
	}
	if len(rpt.toolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(rpt.toolCalls))
	}
	out := buildTranscriptReport(rpt)
	if !strings.Contains(out, "## Tool Usage") {
		t.Error("expected ## Tool Usage section")
	}
	if !strings.Contains(out, "local-only") {
		t.Error("expected privacy notice")
	}
}

// TestTranscriptValidTwoTools verifies that a two-tool transcript is parsed correctly. [130.2.4]
func TestTranscriptValidTwoTools(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "two_tools.jsonl")
	// Two assistant turns, each calling a different tool.
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"neo_radar","input":{"intent":"BRIEFING"}}],"usage":{"input_tokens":100,"output_tokens":200}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/a.go","old_string":"x","new_string":"y"}},{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/b.go","old_string":"a","new_string":"b"}}],"usage":{"input_tokens":50,"output_tokens":80}}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	rpt, err := parseTranscript(p)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if rpt.totalTurns != 2 {
		t.Errorf("expected 2 turns, got %d", rpt.totalTurns)
	}
	if rpt.toolCalls["neo_radar"] == nil || rpt.toolCalls["neo_radar"].calls != 1 {
		t.Error("expected 1 neo_radar call")
	}
	if rpt.toolCalls["Edit"] == nil || rpt.toolCalls["Edit"].calls != 2 {
		t.Errorf("expected 2 Edit calls, got %v", rpt.toolCalls["Edit"])
	}
	if rpt.filesEdited["/repo/a.go"] != 1 || rpt.filesEdited["/repo/b.go"] != 1 {
		t.Errorf("expected both files tracked, got %v", rpt.filesEdited)
	}
	if rpt.totalTokensIn != 150 || rpt.totalTokensOut != 280 {
		t.Errorf("unexpected token counts in=%d out=%d", rpt.totalTokensIn, rpt.totalTokensOut)
	}
}

// TestTranscriptRetryLoopDetected verifies that 3 consecutive same-tool turns are flagged. [130.2.4]
func TestTranscriptRetryLoopDetected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "retry.jsonl")
	// 3 assistant turns each calling neo_radar.
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"neo_radar","input":{}}],"usage":{"input_tokens":10,"output_tokens":20}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"neo_radar","input":{}}],"usage":{"input_tokens":10,"output_tokens":20}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"neo_radar","input":{}}],"usage":{"input_tokens":10,"output_tokens":20}}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	rpt, err := parseTranscript(p)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(rpt.retryLoops) == 0 {
		t.Error("expected retry loop detected for neo_radar")
	}
	found := false
	for _, loop := range rpt.retryLoops {
		if loop.tool == "neo_radar" {
			found = true
		}
	}
	if !found {
		t.Errorf("neo_radar not in retry loops: %v", rpt.retryLoops)
	}
	out := buildTranscriptReport(rpt)
	if !strings.Contains(out, "neo_radar") || !strings.Contains(out, "retry loop") {
		t.Error("expected retry loop mention in report")
	}
}
