package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// TestFetchPeerBootProgress_OK covers the happy path: Nexus /status
// returns a list of workspaces and the parser extracts boot_phase +
// boot_pct per workspace ID. [148.C]
func TestFetchPeerBootProgress_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "alpha", "status": "starting", "boot_phase": "hnsw_load", "boot_pct": 0.67},
			{"id": "beta", "status": "running", "boot_phase": "", "boot_pct": 0},
		})
	}))
	defer srv.Close()

	got := fetchPeerBootProgress(srv.URL, sre.SafeInternalHTTPClient(2))
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if a, ok := got["alpha"]; !ok || a.Status != "starting" || a.Phase != "hnsw_load" || a.Pct != 0.67 {
		t.Errorf("alpha entry wrong: %+v (ok=%v)", a, ok)
	}
	if b, ok := got["beta"]; !ok || b.Status != "running" {
		t.Errorf("beta entry wrong: %+v (ok=%v)", b, ok)
	}
}

// TestFetchPeerBootProgress_NetworkError covers the silent-fallback
// path: Nexus unreachable → empty map (caller falls back to "last:
// Ns ago" rendering).
func TestFetchPeerBootProgress_NetworkError(t *testing.T) {
	got := fetchPeerBootProgress("http://127.0.0.1:1", sre.SafeInternalHTTPClient(1)) // unreachable port
	if len(got) != 0 {
		t.Errorf("expected empty map on unreachable, got %+v", got)
	}
}

// TestFetchPeerBootProgress_MalformedJSON covers the silent-fallback
// path when Nexus returns garbage. [148.C robustness]
func TestFetchPeerBootProgress_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	got := fetchPeerBootProgress(srv.URL, sre.SafeInternalHTTPClient(1))
	if len(got) != 0 {
		t.Errorf("expected empty map on malformed JSON, got %+v", got)
	}
}

// TestFetchPeerBootProgress_Non200 covers HTTP error responses (older
// Nexus without /status endpoint, 500 from internal panic, etc.).
func TestFetchPeerBootProgress_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()
	got := fetchPeerBootProgress(srv.URL, sre.SafeInternalHTTPClient(1))
	// Decode runs even on 500 (we don't gate on status code in this
	// helper) but the response body "internal\n" is not valid JSON →
	// empty map. Verify the silent-fallback contract.
	if len(got) != 0 {
		t.Errorf("expected empty map on 500, got %+v", got)
	}
}
