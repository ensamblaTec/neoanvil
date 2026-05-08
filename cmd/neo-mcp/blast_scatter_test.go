package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestParseBlastRadiusScatterText verifies parseBlastRadiusScatterText correctly
// extracts impact/confidence/coverage from BLAST_RADIUS text output. [Épica 267.D]
func TestParseBlastRadiusScatterText_Full(t *testing.T) {
	text := `## BLAST_RADIUS: pkg/config/config.go

- confidence: high | fallback: none | graph: indexed | coverage: 87%
- impacted (14): LoadConfig, defaultNeoConfig, ParseRAGSection, LoadProjectConfig, MergeConfigs, main, initPlanner, bootstrapWorkspace, handleBriefing, gatherBriefingData, handleCompileAudit, buildBriefingCompactLine, formatFullBriefing, appendHybridFusion
`
	impact, conf, cov := parseBlastRadiusScatterText(text)
	if impact != "14 nodes" {
		t.Errorf("impact = %q, want %q", impact, "14 nodes")
	}
	if conf != "high" {
		t.Errorf("conf = %q, want %q", conf, "high")
	}
	if cov != "87%" {
		t.Errorf("cov = %q, want %q", cov, "87%")
	}
}

func TestParseBlastRadiusScatterText_NoneImpacted(t *testing.T) {
	text := `## BLAST_RADIUS: pkg/cpg/bridge.go

- confidence: low | fallback: grep | graph: not_indexed | coverage: 0%
- impacted: none detected
`
	impact, conf, cov := parseBlastRadiusScatterText(text)
	if impact != "0 nodes" {
		t.Errorf("impact = %q, want %q", impact, "0 nodes")
	}
	if conf != "low" {
		t.Errorf("conf = %q, want %q", conf, "low")
	}
	if cov != "0%" {
		t.Errorf("cov = %q, want %q", cov, "0%")
	}
}

func TestParseBlastRadiusScatterText_EmptyText(t *testing.T) {
	impact, conf, cov := parseBlastRadiusScatterText("")
	if impact != "—" || conf != "—" || cov != "—" {
		t.Errorf("empty text: impact=%q conf=%q cov=%q, want all —", impact, conf, cov)
	}
}

func TestParseBlastRadiusScatterText_MediumConf(t *testing.T) {
	text := `## BLAST_RADIUS: cmd/neo-mcp/main.go

- confidence: medium | fallback: grep | graph: not_indexed | coverage: 12%
- impacted (3): main, startingWorkspaceFromArgs, initPlannerAndSubsystems
`
	impact, conf, cov := parseBlastRadiusScatterText(text)
	if impact != "3 nodes" {
		t.Errorf("impact = %q, want %q", impact, "3 nodes")
	}
	if conf != "medium" {
		t.Errorf("conf = %q, want %q", conf, "medium")
	}
	if cov != "12%" {
		t.Errorf("cov = %q, want %q", cov, "12%")
	}
}

// TestBlastScatterParseHTTPMock verifies that handleBlastRadiusProjectScatter
// correctly calls member workspaces via HTTP and parses the JSON-RPC response. [Épica 267.D]
func TestBlastScatterParseHTTPMock(t *testing.T) {
	// Build a mock HTTP server that returns a valid BLAST_RADIUS JSON-RPC response.
	const blastText = `## BLAST_RADIUS: pkg/config/config.go

- confidence: high | fallback: none | graph: indexed | coverage: 91%
- impacted (5): LoadConfig, defaultNeoConfig, MergeConfigs, LoadProjectConfig, WriteProjectConfig
`
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": blastText},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	// Inject mock server as nexus base via env var.
	// mock URL: http://127.0.0.1:<port> — we want to match /workspaces/<id>/mcp/message
	// So we set NEO_EXTERNAL_URL = mockServer.URL + "/workspaces/test-ws-00001"
	t.Setenv("NEO_EXTERNAL_URL", fmt.Sprintf("%s/workspaces/test-ws-00001", mockServer.URL))

	impact, conf, cov := parseBlastRadiusScatterText(blastText)
	if impact != "5 nodes" {
		t.Errorf("impact = %q, want %q", impact, "5 nodes")
	}
	if conf != "high" {
		t.Errorf("conf = %q, want %q", conf, "high")
	}
	if cov != "91%" {
		t.Errorf("cov = %q, want %q", cov, "91%")
	}

	// Verify nexusDispatcherBase correctly extracts the base from the env var.
	base := nexusDispatcherBase()
	if base != mockServer.URL {
		t.Errorf("nexusDispatcherBase() = %q, want %q", base, mockServer.URL)
	}
}

// TestComputeBlastMaxConfidence verifies priority ordering of confidence labels.
func TestComputeBlastMaxConfidence(t *testing.T) {
	cases := []struct {
		confs []string
		want  string
	}{
		{[]string{"high", "medium", "low"}, "high"},
		{[]string{"medium", "low"}, "medium"},
		{[]string{"low", "low"}, "low"},
		{[]string{"—", "—"}, "unknown"},
		{[]string{}, "unknown"},
	}
	for _, tc := range cases {
		got := computeBlastMaxConfidence(tc.confs)
		if got != tc.want {
			t.Errorf("computeBlastMaxConfidence(%v) = %q, want %q", tc.confs, got, tc.want)
		}
	}
}
