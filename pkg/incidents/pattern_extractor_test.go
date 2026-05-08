package incidents

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestExtractRecurringPatterns_FiltersByMinCount [Épica 231.B]
func TestExtractRecurringPatterns_FiltersByMinCount(t *testing.T) {
	metas := []IncidentMeta{
		{ID: "A", Severity: "CRITICAL", AffectedServices: []string{"RAG"}},
		{ID: "B", Severity: "WARNING", AffectedServices: []string{"RAG", "HNSW"}},
		{ID: "C", Severity: "INFO", AffectedServices: []string{"HNSW"}},
		{ID: "D", Severity: "INFO", AffectedServices: []string{"LONELY"}}, // only 1 → filtered
	}
	patterns := ExtractRecurringPatterns(metas)
	if len(patterns) != 2 {
		t.Fatalf("expected 2 patterns (RAG, HNSW), got %d: %+v", len(patterns), patterns)
	}
	names := []string{patterns[0].Component, patterns[1].Component}
	has := func(s string) bool {
		return slices.Contains(names, s)
	}
	if !has("RAG") || !has("HNSW") {
		t.Errorf("expected RAG+HNSW, got %v", names)
	}
}

// TestExtractRecurringPatterns_SortByCountDesc [Épica 231.B]
func TestExtractRecurringPatterns_SortByCountDesc(t *testing.T) {
	metas := []IncidentMeta{
		{ID: "A", Severity: "INFO", AffectedServices: []string{"Common"}},
		{ID: "B", Severity: "INFO", AffectedServices: []string{"Common"}},
		{ID: "C", Severity: "INFO", AffectedServices: []string{"Common"}},
		{ID: "D", Severity: "INFO", AffectedServices: []string{"Rare", "Common"}},
		{ID: "E", Severity: "INFO", AffectedServices: []string{"Rare"}},
	}
	patterns := ExtractRecurringPatterns(metas)
	if len(patterns) != 2 {
		t.Fatalf("expected 2 patterns, got %d", len(patterns))
	}
	if patterns[0].Component != "Common" || patterns[0].Count != 4 {
		t.Errorf("expected Common first with 4, got %+v", patterns[0])
	}
	if patterns[1].Component != "Rare" || patterns[1].Count != 2 {
		t.Errorf("expected Rare second with 2, got %+v", patterns[1])
	}
}

// TestExtractRecurringPatterns_HighestSeverityWins [Épica 231.B]
func TestExtractRecurringPatterns_HighestSeverityWins(t *testing.T) {
	metas := []IncidentMeta{
		{ID: "A", Severity: "INFO", AffectedServices: []string{"X"}},
		{ID: "B", Severity: "CRITICAL", AffectedServices: []string{"X"}},
		{ID: "C", Severity: "WARNING", AffectedServices: []string{"X"}},
	}
	patterns := ExtractRecurringPatterns(metas)
	if len(patterns) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(patterns))
	}
	if patterns[0].Severity != "CRITICAL" {
		t.Errorf("expected CRITICAL, got %q", patterns[0].Severity)
	}
}

// TestExtractRecurringPatterns_TruncatesExampleIDsAt5 [Épica 231.B]
func TestExtractRecurringPatterns_TruncatesExampleIDsAt5(t *testing.T) {
	var metas []IncidentMeta
	for i := range 10 {
		metas = append(metas, IncidentMeta{
			ID:               string(rune('A' + i)),
			Severity:         "INFO",
			AffectedServices: []string{"S"},
		})
	}
	patterns := ExtractRecurringPatterns(metas)
	if len(patterns) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(patterns))
	}
	if len(patterns[0].IncidentIDs) != 5 {
		t.Errorf("expected 5 example IDs (truncated), got %d", len(patterns[0].IncidentIDs))
	}
}

// TestHigherSeverity [Épica 231.B]
func TestHigherSeverity(t *testing.T) {
	cases := []struct {
		a, b, want string
	}{
		{"CRITICAL", "WARNING", "CRITICAL"},
		{"WARNING", "CRITICAL", "CRITICAL"},
		{"INFO", "WARNING", "WARNING"},
		{"", "INFO", "INFO"},
		{"INFO", "INFO", "INFO"},
	}
	for _, c := range cases {
		if got := higherSeverity(c.a, c.b); got != c.want {
			t.Errorf("higherSeverity(%q,%q)=%q want %q", c.a, c.b, got, c.want)
		}
	}
}

// TestBuildDirective_Contents [Épica 231.B]
func TestBuildDirective_Contents(t *testing.T) {
	got := buildDirective("RAG", 5, "CRITICAL")
	for _, substring := range []string{"RAG", "5", "CRITICAL", "neo_log_analyzer"} {
		if !strings.Contains(got, substring) {
			t.Errorf("directive should contain %q, got: %s", substring, got)
		}
	}
}

// TestFormatPatternAudit_NonEmpty [Épica 231.B]
func TestFormatPatternAudit_NonEmpty(t *testing.T) {
	patterns := []Pattern{
		{Component: "RAG", Count: 3, Severity: "CRITICAL", IncidentIDs: []string{"A", "B"}, Directive: "do X"},
	}
	out := FormatPatternAudit(patterns)
	for _, s := range []string{"RAG", "3", "CRITICAL"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q: %s", s, out)
		}
	}
}

// TestFormatPatternAudit_Empty [Épica 231.B]
func TestFormatPatternAudit_Empty(t *testing.T) {
	out := FormatPatternAudit(nil)
	if out == "" {
		t.Error("expected non-empty placeholder text on empty input")
	}
}

// TestCountIncidentFiles_Empty [Épica 231.B]
func TestCountIncidentFiles_Empty(t *testing.T) {
	ws := t.TempDir()
	n := CountIncidentFiles(ws)
	if n != 0 {
		t.Errorf("expected 0 on missing dir, got %d", n)
	}
}

// TestCountIncidentFiles_Counts [Épica 231.B]
func TestCountIncidentFiles_Counts(t *testing.T) {
	ws := t.TempDir()
	dir := ws + "/.neo/incidents"
	mustMkdir(t, dir)
	for _, name := range []string{"INC-1.md", "INC-2.md", "not-INC.md", "INC-3.txt"} {
		mustWrite(t, dir+"/"+name, "body")
	}
	if n := CountIncidentFiles(ws); n != 2 {
		t.Errorf("expected 2 INC-*.md, got %d", n)
	}
}

// TestArchiveOldIncidents_DisabledWhenZero [Épica 330.C]
func TestArchiveOldIncidents_DisabledWhenZero(t *testing.T) {
	ws := t.TempDir()
	dir := ws + "/.neo/incidents"
	mustMkdir(t, dir)
	mustWrite(t, dir+"/INC-1.md", "body")
	if n := ArchiveOldIncidents(ws, 0); n != 0 {
		t.Errorf("days=0 must short-circuit, got archived=%d", n)
	}
	if CountIncidentFiles(ws) != 1 {
		t.Error("INC should remain untouched with days=0")
	}
}

// TestArchiveOldIncidents_MovesOldFiles [Épica 330.C]
func TestArchiveOldIncidents_MovesOldFiles(t *testing.T) {
	ws := t.TempDir()
	dir := ws + "/.neo/incidents"
	mustMkdir(t, dir)
	old := dir + "/INC-old.md"
	recent := dir + "/INC-recent.md"
	mustWrite(t, old, "old body")
	mustWrite(t, recent, "recent body")
	// Backdate `old` by 40 days, leave `recent` at now.
	past := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	archived := ArchiveOldIncidents(ws, 30)
	if archived != 1 {
		t.Fatalf("expected 1 archived, got %d", archived)
	}
	// Verify: old moved, recent stayed.
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old INC should have been moved")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent INC should remain: %v", err)
	}
	if _, err := os.Stat(dir + "/archive/INC-old.md"); err != nil {
		t.Errorf("archived INC not found: %v", err)
	}
}
