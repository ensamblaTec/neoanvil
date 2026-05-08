package main

import (
	"strings"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

func TestAppendToolNudges_SemanticCode(t *testing.T) {
	cfg := &config.NeoConfig{}
	rt := &RadarTool{cfg: cfg}
	d := &briefingData{
		ragCoverage:       80,
		semanticCodeCount: 0,
		readSliceCount:    5,
	}
	var sb strings.Builder
	appendToolNudges(&sb, rt, d)
	if !strings.Contains(sb.String(), "SEMANTIC_CODE") {
		t.Error("expected SEMANTIC_CODE nudge when RAG>=50 + 0 calls + >=3 READ_SLICE")
	}
}

func TestAppendToolNudges_NoDuplicate(t *testing.T) {
	cfg := &config.NeoConfig{}
	rt := &RadarTool{cfg: cfg}
	d := &briefingData{
		ragCoverage:       80,
		semanticCodeCount: 0,
		readSliceCount:    5,
		nudgeShown:        map[string]bool{"SEMANTIC_CODE": true},
	}
	var sb strings.Builder
	appendToolNudges(&sb, rt, d)
	if strings.Contains(sb.String(), "SEMANTIC_CODE") {
		t.Error("nudge should not repeat when already shown")
	}
}

func TestAppendToolNudges_AlreadyCalled(t *testing.T) {
	cfg := &config.NeoConfig{}
	rt := &RadarTool{cfg: cfg}
	d := &briefingData{
		ragCoverage:       80,
		semanticCodeCount: 2,
		readSliceCount:    5,
	}
	var sb strings.Builder
	appendToolNudges(&sb, rt, d)
	if strings.Contains(sb.String(), "SEMANTIC_CODE") {
		t.Error("should not nudge when SEMANTIC_CODE already called")
	}
}

func TestAppendToolNudges_ConfigOff(t *testing.T) {
	cfg := &config.NeoConfig{}
	cfg.SRE.ToolNudgesOff = true
	rt := &RadarTool{cfg: cfg}
	d := &briefingData{
		ragCoverage:       80,
		semanticCodeCount: 0,
		readSliceCount:    5,
	}
	var sb strings.Builder
	appendToolNudges(&sb, rt, d)
	if sb.Len() > 0 {
		t.Error("nudges should be empty when config disabled")
	}
}

func TestAppendToolNudges_CompactNoNudges(t *testing.T) {
	cfg := &config.NeoConfig{}
	rt := &RadarTool{cfg: cfg}
	d := &briefingData{
		ragCoverage:       80,
		semanticCodeCount: 0,
		readSliceCount:    5,
	}
	var sb strings.Builder
	appendToolNudges(&sb, rt, d)
	if !strings.Contains(sb.String(), "Tool Suggestions") {
		t.Error("full mode should show Tool Suggestions header")
	}
}
