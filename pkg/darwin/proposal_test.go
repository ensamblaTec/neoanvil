package darwin

import (
	"strings"
	"testing"
)

func TestFormatEvolutionReport_ContainsKeyFields(t *testing.T) {
	champion := Champion{
		Source:         "func OptimizedFn() {}",
		NsPerOp:        50,
		AllocsPerOp:    0,
		ImprovementPct: 50.0,
		Generation:     2,
		MutationID:     3,
	}
	hotspot := Hotspot{
		Package:  "pkg/rag",
		Function: "SearchKNN",
		File:     "pkg/rag/hnsw.go",
		Line:     42,
	}

	report := FormatEvolutionReport(champion, hotspot, 100, 2)

	if !strings.Contains(report.Markdown, "SearchKNN") {
		t.Error("report should mention the target function")
	}
	if !strings.Contains(report.Markdown, "50.0%") {
		t.Error("report should show improvement percentage")
	}
	if !strings.Contains(report.Markdown, "OptimizedFn") {
		t.Error("report should include champion source")
	}
	if report.Hotspot.Function != "SearchKNN" {
		t.Errorf("Hotspot.Function mismatch: %q", report.Hotspot.Function)
	}
	if report.Champion.ImprovementPct != 50.0 {
		t.Errorf("Champion.ImprovementPct: got %f, want 50.0", report.Champion.ImprovementPct)
	}
	if report.Timestamp.IsZero() {
		t.Error("Timestamp should be set")
	}
}

func TestFormatEvolutionReport_ZeroBaselineAllocs(t *testing.T) {
	// allocImprovement path: baselineAllocs == 0 → improvement = 0
	champion := Champion{NsPerOp: 80, AllocsPerOp: 0, ImprovementPct: 20.0, Generation: 1, MutationID: 1}
	hotspot := Hotspot{Package: "p", Function: "F", File: "f.go", Line: 1}
	report := FormatEvolutionReport(champion, hotspot, 100, 0)
	if !strings.Contains(report.Markdown, "Darwin Evolution Report") {
		t.Error("should contain report header")
	}
}

func TestPendingChampionsSummary_NoChampions(t *testing.T) {
	got := PendingChampionsSummary(nil)
	if got != "" {
		t.Errorf("expected empty for nil slice, got %q", got)
	}
}

func TestPendingChampionsSummary_AllApplied(t *testing.T) {
	champions := []StoredChampion{
		{Applied: true, Champion: Champion{ImprovementPct: 30.0}},
		{Applied: true, Champion: Champion{ImprovementPct: 50.0}},
	}
	got := PendingChampionsSummary(champions)
	if got != "" {
		t.Errorf("all applied → expected empty, got %q", got)
	}
}

func TestPendingChampionsSummary_SomePending(t *testing.T) {
	champions := []StoredChampion{
		{Applied: false, Champion: Champion{ImprovementPct: 35.5}},
		{Applied: true, Champion: Champion{ImprovementPct: 60.0}},
		{Applied: false, Champion: Champion{ImprovementPct: 20.0}},
	}
	got := PendingChampionsSummary(champions)
	if !strings.Contains(got, "2") {
		t.Errorf("should mention 2 pending: %q", got)
	}
	if !strings.Contains(got, "35.5") {
		t.Errorf("should mention best improvement 35.5%%: %q", got)
	}
}
