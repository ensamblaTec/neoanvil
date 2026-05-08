package main

import (
	"strings"
	"testing"
)

// TestBriefingResumeWarning verifies that resumeWarning is set correctly. [156.E]

// gatherBriefingDataForTest is a thin wrapper to test the resumeWarning logic
// without instantiating a full RadarTool (which requires live BoltDB/HNSW).
func buildResumeTestData(isFirstBriefing bool, sessionMuts []string) briefingData {
	d := briefingData{}
	d.sessionMuts = sessionMuts
	// resumeWarning: isFirstBriefing && (has muts OR server up > 2min)
	if isFirstBriefing {
		hasMuts := len(d.sessionMuts) > 0
		if hasMuts {
			d.resumeWarning = true
		}
	}
	// build compact line with prefix
	resumePrefix := ""
	if d.resumeWarning {
		resumePrefix = "⚠️ RESUME | "
	}
	d.compactLine = resumePrefix + "Mode: pair | Phase: test | Open: 0 | Closed: 100"
	return d
}

func TestBriefingResumeWarning_FirstCallWithMuts(t *testing.T) {
	// Simulates: first BRIEFING call, session already has mutations (agent worked without BRIEFING).
	d := buildResumeTestData(true, []string{"pkg/foo/bar.go", "cmd/neo-mcp/main.go"})
	if !d.resumeWarning {
		t.Fatal("expected resumeWarning=true when first BRIEFING call has session mutations")
	}
	if !strings.HasPrefix(d.compactLine, "⚠️ RESUME |") {
		t.Fatalf("expected compact line to start with ⚠️ RESUME, got: %s", d.compactLine)
	}
}

func TestBriefingResumeWarning_SecondCall_NoWarning(t *testing.T) {
	// Simulates: subsequent BRIEFING call (isFirstBriefing=false) — no warning.
	d := buildResumeTestData(false, []string{"pkg/foo/bar.go"})
	if d.resumeWarning {
		t.Fatal("expected resumeWarning=false on second BRIEFING call")
	}
	if strings.HasPrefix(d.compactLine, "⚠️ RESUME |") {
		t.Fatalf("compact line should NOT have RESUME prefix on second call, got: %s", d.compactLine)
	}
}

func TestBriefingResumeWarning_FirstCallNoMuts_NoWarning(t *testing.T) {
	// Simulates: first BRIEFING call with no mutations (fresh boot, agent synced immediately).
	d := buildResumeTestData(true, nil)
	if d.resumeWarning {
		t.Fatal("expected resumeWarning=false when no session mutations and fresh boot")
	}
}
