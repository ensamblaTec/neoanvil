package sre

// [SRE-103.C] HTTP client contract — use the right tool for the right target.
//
//	┌─────────────────────────────┬──────────────────────────────────────────┐
//	│ Constructor                  │ Use when ...                             │
//	├─────────────────────────────┼──────────────────────────────────────────┤
//	│ SafeHTTPClient()             │ URL is user-configurable or external     │
//	│                              │ (neo.yaml endpoints, webhooks, Ollama    │
//	│                              │ over network). Enforces SSRF protections │
//	│                              │ against arbitrary hostnames, permits     │
//	│                              │ loopback + trusted_local_ports only.     │
//	├─────────────────────────────┼──────────────────────────────────────────┤
//	│ SafeInternalHTTPClient(sec)  │ URL is server-controlled loopback        │
//	│                              │ (Nexus → child on dynamic port, watchdog │
//	│                              │ health, service manager → managed Ollama │
//	│                              │ instance). Loopback-only; does NOT rely  │
//	│                              │ on trusted_local_ports — fewer config    │
//	│                              │ knobs, stronger guarantee.               │
//	└─────────────────────────────┴──────────────────────────────────────────┘
//
// Never use the raw http.DefaultClient or &http.Client{} for outbound traffic.
// Never use SafeHTTPClient for Nexus→children — trusted_local_ports is fragile
// and the child port is dynamic anyway.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// trustedPorts holds the runtime-configured localhost ports that bypass SSRF.
// Set once at boot via InitTrustedPorts, read concurrently by SafeHTTPClient.
var (
	trustedPorts     map[string]bool
	trustedPortsOnce sync.Once
)

// cgnatBlock is the Carrier-Grade NAT range (RFC 6598). Go's net.IP.IsPrivate()
// does NOT cover 100.64.0.0/10, so we check it explicitly.
var cgnatBlock = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// InitTrustedPorts configures which localhost ports bypass the SSRF guard.
// Called once from main.go with values from neo.yaml (sre.trusted_local_ports).
func InitTrustedPorts(ports []int) {
	trustedPortsOnce.Do(func() {
		trustedPorts = make(map[string]bool, len(ports))
		for _, p := range ports {
			trustedPorts[strconv.Itoa(p)] = true
		}
	})
}

// canonicalIP returns the IPv4 form of ip when ip is an IPv4-mapped IPv6
// address (::ffff:127.0.0.1, ::ffff:10.0.0.1, ::ffff:169.254.169.254, etc.).
// Otherwise the original ip is returned unchanged.
//
// This canonicalisation is required before any IsLoopback / IsPrivate /
// IsLinkLocalUnicast / IsMulticast classification: net.IP's predicates
// only recognise IPv4 patterns when the slice is in 4-byte form. The IPv6
// representation of the same address (16 bytes with the ::ffff: prefix)
// returns FALSE for IsLoopback even though the dial would still reach
// 127.0.0.1. Without canonicalisation, an attacker who feeds a hostname
// resolving to ::ffff:127.0.0.1 (or any RFC1918 IPv4-mapped form) bypasses
// the SafeHTTPClient SSRF guard entirely and reaches localhost services.
//
// Audit finding pkg/sre/ssrf.go (2026-05-01, SEV 8: IPv4-mapped IPv6 SSRF
// bypass). The fix applies to all three constructors below.
func canonicalIP(ip net.IP) net.IP {
	if four := ip.To4(); four != nil {
		return four
	}
	return ip
}

// SafeInternalHTTPClient returns an HTTP client for server-side loopback calls where the target
// URL is fully server-controlled (e.g., Nexus→children at dynamic child ports).
// Unlike SafeHTTPClient, it permits all loopback addresses — NEVER use with user-provided URLs.
func SafeInternalHTTPClient(timeoutSec int) *http.Client {
	if timeoutSec <= 0 {
		timeoutSec = 15
	}
	return &http.Client{
		Timeout: time.Duration(timeoutSec) * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.LookupIP(host)
				if err != nil || len(ips) == 0 {
					return nil, fmt.Errorf("resolución DNS fallida para %s", host)
				}
				// Only permit loopback — reject anything that escapes to the network.
				// canonicalIP() unwraps IPv4-mapped IPv6 (::ffff:127.0.0.1) so the
				// loopback predicate matches; without it the IPv6-mapped form would
				// erroneously REJECT internal loopback callers.
				for _, raw := range ips {
					if !canonicalIP(raw).IsLoopback() {
						return nil, fmt.Errorf("[SRE-SSRF FATAL] SafeInternalHTTPClient: dirección no-loopback bloqueada (%s)", raw)
					}
				}
				return net.DialTimeout(network, addr, 5*time.Second)
			},
		},
	}
}

// SafeInternalHTTPClientD is identical to SafeInternalHTTPClient but accepts a time.Duration
// for sub-second precision (e.g. 500ms for parallel briefing gathering).
func SafeInternalHTTPClientD(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.LookupIP(host)
				if err != nil || len(ips) == 0 {
					return nil, fmt.Errorf("resolución DNS fallida para %s", host)
				}
				for _, raw := range ips {
					if !canonicalIP(raw).IsLoopback() {
						return nil, fmt.Errorf("[SRE-SSRF FATAL] SafeInternalHTTPClientD: dirección no-loopback bloqueada (%s)", raw)
					}
				}
				return net.DialTimeout(network, addr, timeout)
			},
		},
	}
}

// SafeHTTPClient crea un cliente HTTP Zero-Trust blindado contra SSRF.
// Internal localhost services on trusted ports (configured in neo.yaml) bypass the guard.
func SafeHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}

				ips, err := net.LookupIP(host)
				if err != nil || len(ips) == 0 {
					return nil, fmt.Errorf("resolución DNS fallida para %s", host)
				}

				// Allow trusted internal services on localhost.
				// canonicalIP() handles ::ffff:127.0.0.1 — without it, IPv4-mapped
				// IPv6 hostnames bypass both the trusted-port allowlist (isLoopback
				// would be false) AND the Zero-Trust barrier below (predicates also
				// false on the IPv6-mapped form), reaching arbitrary localhost
				// services. The fix unwraps once and reuses for both checks.
				isLoopback := false
				for _, raw := range ips {
					if canonicalIP(raw).IsLoopback() {
						isLoopback = true
						break
					}
				}
				if isLoopback && trustedPorts[port] {
					return net.DialTimeout(network, addr, 5*time.Second)
				}

				// Barrera Zero-Trust: Auditar la IP resuelta antes de abrir el socket.
				// IsLinkLocalUnicast (fe80::/10) added 2026-05-01 — audit finding
				// SEV 7 noted Go's IsPrivate does not cover IPv6 link-local.
				for _, raw := range ips {
					ip := canonicalIP(raw)
					if ip.IsLoopback() || ip.IsPrivate() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
						return nil, fmt.Errorf("[SRE-SSRF FATAL] Bloqueo de Capa 4: Intento de acceso a IP no enrutable o privada (%s). Operación abortada", raw.String())
					}
					if cgnatBlock != nil && cgnatBlock.Contains(ip) {
						return nil, fmt.Errorf("[SRE-SSRF FATAL] Bloqueo de Capa 4: Intento de acceso a CGNAT address (%s). Operación abortada", raw.String())
					}
				}

				safeAddr := net.JoinHostPort(ips[0].String(), port)
				return net.DialTimeout(network, safeAddr, 5*time.Second)
			},
		},
	}
}
