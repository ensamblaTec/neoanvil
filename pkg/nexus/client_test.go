package nexus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClient_EmptyBaseReturnsUnavailable verifies that calls against an
// unconfigured Client (base=="") fast-fail with ErrNexusUnavailable.
// [Épica 360.A]
func TestClient_EmptyBaseReturnsUnavailable(t *testing.T) {
	c := NewClient("", http.DefaultClient, 5, 30*time.Second)
	if c.IsAvailable() {
		t.Error("empty base should render available=false")
	}
	_, err := c.Post(context.Background(), "/internal/foo", []byte("{}"))
	if !errors.Is(err, ErrNexusUnavailable) {
		t.Errorf("want ErrNexusUnavailable, got %v", err)
	}
	_, err = c.Get(context.Background(), "/internal/bar")
	if !errors.Is(err, ErrNexusUnavailable) {
		t.Errorf("want ErrNexusUnavailable on GET, got %v", err)
	}
}

// TestClient_HappyPath verifies a normal round-trip updates availability.
func TestClient_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), 5, 30*time.Second)
	body, err := c.Post(context.Background(), "/internal/test", []byte("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body: %q", body)
	}
	if !c.IsAvailable() {
		t.Error("after successful call, available should be true")
	}
}

// TestClient_CircuitOpensAfterFailures verifies that N=3 consecutive 5xx
// failures trip the breaker, and subsequent calls fast-fail without hitting
// the network. [Épica 360.A — the core requirement: no hangs]
func TestClient_CircuitOpensAfterFailures(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), 3, 1*time.Hour) // 3 fails → open
	ctx := context.Background()
	endpoint := "/internal/flaky"

	// First 3 calls should hit the server and fail with 500.
	for i := range 3 {
		_, err := c.Post(ctx, endpoint, []byte("{}"))
		if err == nil {
			t.Fatalf("call %d should have errored (server returns 500)", i)
		}
	}
	preBreakerHits := hits

	// Next call should NOT hit the server — breaker is open.
	_, err := c.Post(ctx, endpoint, []byte("{}"))
	if !errors.Is(err, ErrNexusUnavailable) {
		t.Errorf("want ErrNexusUnavailable after breaker trip, got %v", err)
	}
	if hits != preBreakerHits {
		t.Errorf("breaker should have short-circuited, but server was hit again (%d → %d)",
			preBreakerHits, hits)
	}
	if c.IsAvailable() {
		t.Error("after trip, available should be false")
	}
}

// TestClient_PerEndpointBreakerIsolation verifies that one failing endpoint
// does not break an independent healthy one.
func TestClient_PerEndpointBreakerIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), 2, 1*time.Hour)
	ctx := context.Background()

	// Trip the breaker for /internal/bad.
	for range 2 {
		_, _ = c.Post(ctx, "/internal/bad", nil)
	}
	_, err := c.Post(ctx, "/internal/bad", nil)
	if !errors.Is(err, ErrNexusUnavailable) {
		t.Errorf("bad endpoint should be circuit-open, got %v", err)
	}

	// /internal/good must still work.
	body, err := c.Post(ctx, "/internal/good", nil)
	if err != nil {
		t.Fatalf("good endpoint should work: %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("body: %q", body)
	}
}

// TestClient_BreakerStats returns registered endpoints for observability.
func TestClient_BreakerStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client(), 5, 30*time.Second)
	ctx := context.Background()
	_, _ = c.Post(ctx, "/internal/a", nil)
	_, _ = c.Get(ctx, "/internal/b")
	stats := c.BreakerStats()
	if _, ok := stats["/internal/a"]; !ok {
		t.Error("stats missing /internal/a")
	}
	if _, ok := stats["/internal/b"]; !ok {
		t.Error("stats missing /internal/b")
	}
}
