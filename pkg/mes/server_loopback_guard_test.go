package mes

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLoopbackHostGuard_AcceptsLocalhost — regression for the DNS-rebinding
// finding surfaced during PILAR XXVIII 143.B audit (2026-05-02). The
// tactical aux HTTP port at 127.0.0.1:tactical_port (plain HTTP, by design)
// must accept legitimate localhost callers but REJECT requests whose Host
// header carries an attacker-controlled FQDN — even when the attacker
// successfully DNS-rebound that FQDN to 127.0.0.1. The guard fires before
// the wrapped mux runs, so the existing CORS:* on /api/v1/log_frontend
// can no longer be exploited from a browser context.
func TestLoopbackHostGuard_AcceptsLocalhost(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	guard := loopbackHostGuard(inner)

	cases := []struct {
		name string
		host string
	}{
		{"127.0.0.1 with port", "127.0.0.1:8084"},
		{"127.0.0.1 no port", "127.0.0.1"},
		{"localhost with port", "localhost:8084"},
		{"localhost no port", "localhost"},
		{"ipv6 loopback with port", "[::1]:8084"},
		{"ipv6 loopback no port", "::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest("POST", "/api/v1/log_frontend", nil)
			req.Host = tc.host
			w := httptest.NewRecorder()
			guard.ServeHTTP(w, req)
			if !called {
				t.Errorf("legitimate Host=%q was rejected (status=%d body=%q)", tc.host, w.Code, w.Body.String())
			}
			if w.Code != http.StatusOK {
				t.Errorf("Host=%q got status=%d, want 200", tc.host, w.Code)
			}
		})
	}
}

// TestLoopbackHostGuard_RejectsDNSRebind — the core defense: requests
// carrying a non-loopback Host (the attacker's domain that DNS-rebound to
// 127.0.0.1 in the browser) must be rejected with 403 BEFORE the inner
// handler runs. This stops the operator's browser from being weaponised
// against the tactical aux port.
func TestLoopbackHostGuard_RejectsDNSRebind(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	guard := loopbackHostGuard(inner)

	cases := []struct {
		name string
		host string
	}{
		{"attacker FQDN", "evil.example.com:8084"},
		{"attacker subdomain", "rebind.attacker.org"},
		{"shadow IP 0.0.0.0", "0.0.0.0:8084"},
		{"shadow IP 127.1 (compressed loopback variant — intentionally rejected for strictness)", "127.1:8084"},
		{"public IP", "1.2.3.4:8084"},
		{"empty host", ""},
		{"raw IPv6 attacker", "[2001:db8::1]:8084"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest("POST", "/api/v1/log_frontend", nil)
			req.Host = tc.host
			w := httptest.NewRecorder()
			guard.ServeHTTP(w, req)
			if called {
				t.Errorf("Host=%q reached the inner handler (audit S9-3 / 143.B regression — DNS rebinding now succeeds)", tc.host)
			}
			if w.Code != http.StatusForbidden {
				t.Errorf("Host=%q got status=%d, want 403", tc.host, w.Code)
			}
		})
	}
}
