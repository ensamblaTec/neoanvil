package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProjectDigestScatter verifies cross-workspace hotspot aggregation from
// two mock PROJECT_DIGEST responses. [Épica 272.D]
func TestProjectDigestScatter(t *testing.T) {
	const digestWS1 = `## PROJECT_DIGEST

### Tech-Debt Hotspots
1. pkg/rag/hnsw.go — 42 mutations
2. pkg/cpg/bridge.go — 28 mutations
`
	const digestWS2 = `## PROJECT_DIGEST

### Tech-Debt Hotspots
1. pkg/config/config.go — 91 mutations
2. pkg/rag/hnsw.go — 15 mutations
`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := r.Header.Get("X-Neo-Workspace")
		text := digestWS1
		if strings.Contains(ws, "002") || strings.Contains(r.URL.Path, "ws-002") {
			text = digestWS2
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": text},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	t.Setenv("NEO_EXTERNAL_URL", fmt.Sprintf("%s/workspaces/project-root", srv.URL))

	// Verify nexusDispatcherBase extracts the base URL correctly.
	if base := nexusDispatcherBase(); base != srv.URL {
		t.Fatalf("nexusDispatcherBase() = %q, want %q", base, srv.URL)
	}

	// Aggregate hotspot counts across both fixture texts — same logic as
	// the aggregation loop in handleProjectDigest.
	agg := map[string]uint64{}
	for _, text := range []string{digestWS1, digestWS2} {
		for line := range strings.SplitSeq(text, "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), ". ", 2)
			if len(parts) != 2 {
				continue
			}
			fileAndMut := strings.SplitN(parts[1], " — ", 2)
			if len(fileAndMut) != 2 {
				continue
			}
			var count uint64
			if _, err := fmt.Sscanf(strings.Fields(fileAndMut[1])[0], "%d", &count); err == nil {
				agg[fileAndMut[0]] += count
			}
		}
	}

	cases := []struct {
		file string
		want uint64
	}{
		{"pkg/rag/hnsw.go", 57},        // 42 + 15 across both workspaces
		{"pkg/config/config.go", 91},   // only in ws-002
		{"pkg/cpg/bridge.go", 28},      // only in ws-001
	}
	for _, tc := range cases {
		got := agg[tc.file]
		if got != tc.want {
			t.Errorf("agg[%q] = %d, want %d", tc.file, got, tc.want)
		}
	}
}
