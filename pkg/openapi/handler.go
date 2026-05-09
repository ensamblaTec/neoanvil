// pkg/openapi/handler.go — HTTP handler for /openapi.json with an
// in-memory cache so each hit doesn't rebuild the spec. The cache is
// invalidated via InvalidateCache (wired to existing
// /internal/openapi/refresh on the neo-mcp side).
//
// [Area 4.1.D + 4.2.A]

package openapi

import (
	"encoding/json"
	"net/http"
	"sync"
)

// BuildFunc returns a fresh Spec — supplied by the caller so the
// handler doesn't have to know about cpg / registry. Lets us share
// this cache across neo-mcp + neo-nexus without import gymnastics.
type BuildFunc func(includeInternal bool) *Spec

// Cache memoises the rendered JSON bytes per (includeInternal) bucket.
// Two buckets: false (default) and true (when ?include_internal=true
// is set). Each bucket is a tiny pointer + lock — no concurrency hot
// path so plain RWMutex is fine.
type Cache struct {
	mu    sync.RWMutex
	build BuildFunc
	cache map[bool][]byte
}

// NewCache wires a build function into a fresh cache. Spec is built
// lazily on the first request, then memoised until InvalidateCache.
func NewCache(b BuildFunc) *Cache {
	return &Cache{
		build: b,
		cache: make(map[bool][]byte, 2),
	}
}

// Handler returns the http.Handler that serves /openapi.json. Caller
// wires it into their mux at the path of their choice (typically
// "/openapi.json" but the dispatcher might prefix with /api/v1/).
//
// `?include_internal=true` is gated behind a loopback check: when the
// server is bound to a non-loopback interface (e.g., Docker mode with
// 0.0.0.0), revealing /internal/* paths to remote requesters is an
// information leak. Loopback callers (operators on the host) get full
// access. [DS-AUDIT 4 Finding 3]
func (c *Cache) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		includeInternal := r.URL.Query().Get("include_internal") == "true"
		if includeInternal && !isLoopbackRemote(r.RemoteAddr) {
			// Silently downgrade rather than 403 — caller still gets
			// the public spec, just without /internal/* paths.
			includeInternal = false
		}
		body, err := c.bytes(includeInternal)
		if err != nil {
			http.Error(w, "openapi build failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
	})
}

// isLoopbackRemote returns true iff the RemoteAddr (host:port form
// from net/http) resolves to a loopback IP. Used to gate the
// include_internal=true escalation. [DS-AUDIT 4 Finding 3]
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := splitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := parseLoopbackIP(host)
	return ip
}

// splitHostPort accepts host:port and returns host, port. We
// handroll a tiny variant rather than importing net.SplitHostPort
// to keep the openapi package dependency-free for unit tests.
func splitHostPort(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return addr, "", nil
}

// parseLoopbackIP recognises the common loopback addresses without
// pulling in net.IP — cheap, branch-predictable, and obvious in
// review. Covers IPv4 127.x, IPv6 ::1, and IPv4-mapped-IPv6 forms.
func parseLoopbackIP(host string) bool {
	if host == "" {
		return true // unix socket / unparseable but local-trusted by mux
	}
	if host == "::1" || host == "[::1]" {
		return true
	}
	if len(host) >= 4 && host[:4] == "127." {
		return true
	}
	// IPv4-mapped IPv6: ::ffff:127.0.0.1
	if len(host) >= 7 && host[:7] == "::ffff:" {
		return parseLoopbackIP(host[7:])
	}
	return false
}

// InvalidateCache drops both buckets so the next request rebuilds.
// Wire to /internal/openapi/refresh on contract changes (handler
// hot-reload, registry rebuild, etc.).
func (c *Cache) InvalidateCache() {
	c.mu.Lock()
	c.cache = make(map[bool][]byte, 2)
	c.mu.Unlock()
}

// bytes returns the cached encoded JSON or builds it lazily.
func (c *Cache) bytes(includeInternal bool) ([]byte, error) {
	c.mu.RLock()
	if b, ok := c.cache[includeInternal]; ok {
		c.mu.RUnlock()
		return b, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check after upgrade — another goroutine may have built it.
	if b, ok := c.cache[includeInternal]; ok {
		return b, nil
	}
	spec := c.build(includeInternal)
	body, err := json.MarshalIndent(spec.toJSONMap(), "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	c.cache[includeInternal] = body
	return body, nil
}
