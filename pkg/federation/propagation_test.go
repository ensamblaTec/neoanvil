package federation

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fleetNodeFromTestServer extracts FleetNode params from an httptest server URL.
func fleetNodeFromTestServer(t *testing.T, srv *httptest.Server, id string, healthy bool) FleetNode {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return FleetNode{
		ID:      id,
		Host:    u.Hostname(),
		Port:    port,
		UseTLS:  false,
		Healthy: healthy,
	}
}

// TestPropagateManifest_PostsToHealthyNodesOnly verifies the function POSTs
// to healthy nodes and skips unhealthy ones. [SRE-116.C]
func TestPropagateManifest_PostsToHealthyNodesOnly(t *testing.T) {
	var hits atomic.Int64
	var receivedBody atomic.Value // last manifest seen, for body shape check
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/memex/ingest" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		receivedBody.Store(body)
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	nodes := []FleetNode{
		fleetNodeFromTestServer(t, srv, "alpha", true),
		fleetNodeFromTestServer(t, srv, "beta", false), // unhealthy → must skip
		fleetNodeFromTestServer(t, srv, "gamma", true),
	}

	manifest := Manifest{
		Date: "2026-04-17",
		Directives: []Directive{
			{Rule: "rule one", Why: "because", When: "always", Topic: "test"},
		},
		SourceNodes: []string{"alpha", "gamma"},
		VectorCount: 2,
		CreatedAt:   time.Now(),
	}

	client := &http.Client{Timeout: 2 * time.Second}
	if err := PropagateManifest(manifest, nodes, client); err != nil {
		t.Fatalf("PropagateManifest: %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("expected 2 POSTs (healthy nodes), got %d", hits.Load())
	}
	// Sanity-check the wire format: the manifest must round-trip through JSON.
	if v := receivedBody.Load(); v != nil {
		var got Manifest
		if err := json.Unmarshal(v.([]byte), &got); err != nil {
			t.Errorf("manifest body not valid JSON: %v", err)
		}
		if got.Date != manifest.Date {
			t.Errorf("manifest date round-trip mismatch: %q vs %q", got.Date, manifest.Date)
		}
	}
}

// TestPropagateManifest_NoNodesNoOp — empty fleet must succeed without error.
func TestPropagateManifest_NoNodesNoOp(t *testing.T) {
	manifest := Manifest{Date: "2026-04-17"}
	client := &http.Client{}
	if err := PropagateManifest(manifest, nil, client); err != nil {
		t.Errorf("empty nodes should be no-op, got error: %v", err)
	}
	if err := PropagateManifest(manifest, []FleetNode{}, client); err != nil {
		t.Errorf("empty []FleetNode should be no-op, got error: %v", err)
	}
}

// TestPropagateManifest_HandlesUpstreamErrors verifies that 500 responses
// don't kill the whole pipeline — propagation continues to the next node.
func TestPropagateManifest_HandlesUpstreamErrors(t *testing.T) {
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv500.Close()

	var ok atomic.Int64
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ok.Add(1)
		w.WriteHeader(http.StatusAccepted) // 202 also counts as success
	}))
	defer srvOK.Close()

	nodes := []FleetNode{
		fleetNodeFromTestServer(t, srv500, "broken", true),
		fleetNodeFromTestServer(t, srvOK, "healthy", true),
	}

	manifest := Manifest{Date: "2026-04-17"}
	client := &http.Client{Timeout: 2 * time.Second}
	if err := PropagateManifest(manifest, nodes, client); err != nil {
		t.Errorf("upstream error should not bubble up: %v", err)
	}
	if ok.Load() != 1 {
		t.Errorf("expected healthy node to still receive POST, got %d", ok.Load())
	}
}

// TestNewDreamConfigFromYAML pins the YAML→struct mapping so that future
// neo.yaml schema changes that reorder/rename fields are caught at compile
// time first and at test time second.
func TestNewDreamConfigFromYAML(t *testing.T) {
	cfg := NewDreamConfigFromYAML("0 3 * * *", 0.85, "http://ollama.local:11434", "qwen2:7b", 30, 50000)
	if cfg.DreamSchedule != "0 3 * * *" {
		t.Errorf("DreamSchedule lost: %q", cfg.DreamSchedule)
	}
	if cfg.DedupThreshold != float32(0.85) {
		t.Errorf("DedupThreshold mismatch: %v", cfg.DedupThreshold)
	}
	if !strings.Contains(cfg.OllamaURL, "ollama.local") {
		t.Errorf("OllamaURL mismatch: %q", cfg.OllamaURL)
	}
	if cfg.HarvestTimeoutSec != 30 {
		t.Errorf("HarvestTimeoutSec mismatch: %d", cfg.HarvestTimeoutSec)
	}
	if cfg.MaxVectorsPerNode != 50000 {
		t.Errorf("MaxVectorsPerNode mismatch: %d", cfg.MaxVectorsPerNode)
	}
}
