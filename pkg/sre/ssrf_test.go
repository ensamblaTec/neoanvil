package sre

import (
	"net"
	"testing"
)

// TestCanonicalIP_UnwrapsIPv4MappedIPv6 — regression for the SSRF bypass
// discovered in the 2026-05-01 audit (SEV 8). The IPv6 representation of
// IPv4 addresses (::ffff:127.0.0.1) must be canonicalised to its 4-byte
// IPv4 form before any IsLoopback / IsPrivate / IsLinkLocalUnicast check;
// otherwise the predicates return false and the address bypasses the
// guard.
//
// Each case below pairs the IPv6-mapped form with the predicate that the
// CANONICAL form must trip. Pre-fix, every "want_loopback / want_private"
// row would have read FALSE on the raw IPv6, and the SSRF guard would
// have allowed the dial through.
func TestCanonicalIP_UnwrapsIPv4MappedIPv6(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantLoop    bool
		wantPriv    bool
		wantLinkLoc bool
	}{
		{"loopback v4", "127.0.0.1", true, false, false},
		{"loopback v6-mapped", "::ffff:127.0.0.1", true, false, false},
		{"loopback v6-mapped 127.0.0.42", "::ffff:127.0.0.42", true, false, false},
		{"private 10.x v4", "10.1.2.3", false, true, false},
		{"private 10.x v6-mapped", "::ffff:10.1.2.3", false, true, false},
		{"link-local v4", "169.254.169.254", false, false, true},
		{"link-local v6-mapped (cloud metadata)", "::ffff:169.254.169.254", false, false, true},
		{"public v4", "8.8.8.8", false, false, false},
		{"public v6-mapped", "::ffff:8.8.8.8", false, false, false},
		{"native ipv6 loopback", "::1", true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := net.ParseIP(tc.raw)
			if raw == nil {
				t.Fatalf("ParseIP(%q) returned nil", tc.raw)
			}
			canon := canonicalIP(raw)
			if got := canon.IsLoopback(); got != tc.wantLoop {
				t.Errorf("canonicalIP(%s).IsLoopback() = %v, want %v", tc.raw, got, tc.wantLoop)
			}
			if got := canon.IsPrivate(); got != tc.wantPriv {
				t.Errorf("canonicalIP(%s).IsPrivate() = %v, want %v", tc.raw, got, tc.wantPriv)
			}
			if got := canon.IsLinkLocalUnicast(); got != tc.wantLinkLoc {
				t.Errorf("canonicalIP(%s).IsLinkLocalUnicast() = %v, want %v", tc.raw, got, tc.wantLinkLoc)
			}
		})
	}
}

// TestCanonicalIP_PreservesNonV4 — ensures we don't accidentally truncate
// genuine IPv6 addresses. Only IPv4-mapped IPv6 should be unwrapped.
func TestCanonicalIP_PreservesNonV4(t *testing.T) {
	cases := []string{"::1", "fe80::1", "2001:db8::1", "ff02::1"}
	for _, c := range cases {
		raw := net.ParseIP(c)
		if raw == nil {
			t.Fatalf("ParseIP(%q) returned nil", c)
		}
		canon := canonicalIP(raw)
		if canon.To4() != nil {
			t.Errorf("canonicalIP(%s) returned an IPv4 form (%s) for a genuine IPv6 address", c, canon)
		}
	}
}
