package shadow

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMiddleware_NoOp_WhenDisabled verifies the middleware is a pass-through
// when Enabled=false — production safety. [SRE-116.B]
func TestMiddleware_NoOp_WhenDisabled(t *testing.T) {
	cfg := DefaultMirrorConfig()
	cfg.Enabled = false
	cfg.TargetURL = "http://shadow.invalid"

	m := NewMirror(cfg, nil)
	called := false
	wrapped := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	wrapped.ServeHTTP(rec, req)

	if !called {
		t.Fatal("inner handler not invoked when middleware disabled")
	}
	if m.Stats.Mirrored.Load() != 0 {
		t.Fatalf("expected 0 mirrored when disabled, got %d", m.Stats.Mirrored.Load())
	}
}

// TestMiddleware_NoOp_WhenTargetURLEmpty mirrors the disabled case for the
// other half of the activation gate.
func TestMiddleware_NoOp_WhenTargetURLEmpty(t *testing.T) {
	cfg := DefaultMirrorConfig()
	cfg.Enabled = true
	cfg.TargetURL = "" // not configured → middleware must skip

	m := NewMirror(cfg, nil)
	called := false
	wrapped := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !called {
		t.Fatal("inner handler not invoked when target URL is empty")
	}
	if m.Stats.Mirrored.Load() != 0 {
		t.Fatalf("Mirrored counter advanced despite empty TargetURL: %d", m.Stats.Mirrored.Load())
	}
}

// TestMiddleware_SkipsUnsafeMethodsByDefault verifies POST is not mirrored
// when UnsafeMethods=false — the default and recommended setting.
func TestMiddleware_SkipsUnsafeMethodsByDefault(t *testing.T) {
	var mirroredFired atomic.Int64
	cfg := DefaultMirrorConfig()
	cfg.Enabled = true
	cfg.TargetURL = "http://example.invalid" // would never resolve, but should never be hit
	cfg.SampleRate = 1.0
	cfg.UnsafeMethods = false

	m := NewMirror(cfg, func(_ DiffReport) { mirroredFired.Add(1) })
	wrapped := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/write", strings.NewReader("payload"))
	wrapped.ServeHTTP(httptest.NewRecorder(), req)
	time.Sleep(50 * time.Millisecond) // give any spurious goroutine a chance to fire

	if m.Stats.Mirrored.Load() != 0 {
		t.Errorf("POST counted as mirrored despite UnsafeMethods=false: %d", m.Stats.Mirrored.Load())
	}
	if mirroredFired.Load() != 0 {
		t.Errorf("verdict callback fired for POST despite UnsafeMethods=false")
	}
}

// TestMiddleware_SamplesIdempotentMethod verifies a GET hits the shadow path
// (Mirrored counter increments) — even when the upstream is unreachable, the
// counter should advance because the dispatch was attempted.
func TestMiddleware_SamplesIdempotentMethod(t *testing.T) {
	cfg := DefaultMirrorConfig()
	cfg.Enabled = true
	cfg.TargetURL = "http://shadow.invalid:9999" // DNS fail expected
	cfg.SampleRate = 1.0
	cfg.TimeoutMs = 500

	m := NewMirror(cfg, nil)
	wrapped := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	wrapped.ServeHTTP(httptest.NewRecorder(), req)

	// Wait up to 1s for the shadow goroutine to advance Mirrored counter.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && m.Stats.Mirrored.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Stats.Mirrored.Load() == 0 {
		t.Fatalf("expected Mirrored counter > 0 after GET dispatch")
	}
}

// TestIsIdempotent covers the helper directly.
func TestIsIdempotent(t *testing.T) {
	tests := map[string]bool{
		http.MethodGet:     true,
		http.MethodHead:    true,
		http.MethodOptions: true,
		http.MethodPost:    false,
		http.MethodPut:     false,
		http.MethodDelete:  false,
	}
	for method, want := range tests {
		if got := isIdempotent(method); got != want {
			t.Errorf("isIdempotent(%s) = %v, want %v", method, got, want)
		}
	}
}

// TestDefaultMirrorConfig pins the documented defaults so a reviewer notices
// silent changes that affect operator expectations.
func TestDefaultMirrorConfig(t *testing.T) {
	c := DefaultMirrorConfig()
	if c.Enabled {
		t.Errorf("DefaultMirrorConfig must ship disabled (got Enabled=true)")
	}
	if c.SampleRate <= 0 || c.SampleRate > 1 {
		t.Errorf("SampleRate %v out of (0,1]", c.SampleRate)
	}
	if c.TimeoutMs <= 0 {
		t.Errorf("TimeoutMs must be positive, got %d", c.TimeoutMs)
	}
	if c.MaxInflight <= 0 {
		t.Errorf("MaxInflight must be positive, got %d", c.MaxInflight)
	}
}
