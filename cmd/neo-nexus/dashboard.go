package main

// [SRE-85.A.1] Operator HUD Dashboard — served by Nexus on :8087.
//
// Post-Épica 245 (PILAR XXVII): embeds the React SPA built from web/dist/
// as the primary HUD, with a separate /legacy path preserving the original
// single-page HTML dashboard.
//
// Routing split:
//   - /                     → SPA entry (index.html)
//   - /assets/*             → embedded static assets (JS, CSS, svg)
//   - /favicon.svg          → embedded favicon
//   - /icons.svg            → embedded icon sprite
//   - /legacy               → the historical Operator HUD page
//   - /status               → proxy to Nexus dispatcher (fleet list)
//   - /api/v1/metrics/*     → proxy to Nexus dispatcher (summary + workspaces/<id>/metrics)
//   - /api/v1/workspaces/*  → proxy to Nexus dispatcher (start/stop/active + metrics)
//   - /api/v1/services      → proxy to Nexus dispatcher
//   - anything else         → proxy to the currently active child (SSE, MCTS, etc.)

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/nexus"
	"github.com/ensamblatec/neoanvil/pkg/workspace"
)

//go:embed all:static
var hudFS embed.FS

// NexusDashboardOpts groups dependencies for the HUD proxy server.
type NexusDashboardOpts struct {
	Host         string
	Port         int
	DispatchPort int // Nexus dispatcher listening port (default 9000). [PILAR-XXVII]
	Registry     *workspace.Registry
	Pool         *nexus.ProcessPool
}

// ListenNexusDashboard starts the Operator HUD on the given port and blocks
// isHUDAllowed returns true iff the connection came from a trusted
// network — loopback always, plus RFC 1918 private ranges and Docker
// bridge IPs (172.16/12, 10.0/8) when running in container mode.
// Public IPs are always rejected. [Bug-7 fix — Docker NAT case]
func isHUDAllowed(remoteAddr string) bool {
	host := remoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if host == "127.0.0.1" || host == "::1" || host == "localhost" {
		return true
	}
	if strings.HasPrefix(host, "127.") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	// IsPrivate covers 10/8, 172.16/12, 192.168/16, fc00::/7.
	return ip.IsPrivate() || ip.IsLoopback()
}

// until ctx is cancelled.
func ListenNexusDashboard(ctx context.Context, opts NexusDashboardOpts) {
	host := opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, opts.Port)

	mux := http.NewServeMux()

	// Localhost enforcement — same guard as the original dashboard.
	// When NEO_BIND_ADDR=0.0.0.0 (Docker mode), the operator hits HUD
	// from the host through Docker NAT — RemoteAddr is the bridge IP
	// (172.16/12) or whatever Docker assigned. Allow private ranges
	// in that case so the operator can access the dashboard from
	// the host machine. The compose stack itself is loopback-only on
	// the host side via the published port mapping. [Bug-7 fix]
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if isHUDAllowed(r.RemoteAddr) {
				next(w, r)
				return
			}
			http.Error(w, "HUD access restricted to localhost or private network", http.StatusForbidden)
		}
	}

	// resolveActivePort returns the HTTP port of the active (running) child.
	resolveActivePort := func() int {
		active := opts.Registry.Active()
		if active == nil {
			return 0
		}
		proc, ok := opts.Pool.GetProcess(active.ID)
		if !ok || proc.Status != nexus.StatusRunning {
			return 0
		}
		return proc.Port
	}

	// [PILAR-XXVIII hotfix] Reverse proxies are cached — creating a new
	// NewSingleHostReverseProxy per request allocates a new http.Transport
	// per request, each with its own connection pool. With HUD polling
	// every 2s + scatter-gather, this piled up 3000+ ESTABLISHED
	// connections to the child and moved neo-mcp near the FD ceiling.
	//
	// Child port can change across restarts, so we build a dynamic
	// Director that re-reads the active port on each request. The
	// Transport (and its keep-alive pool) is shared.
	sharedTransport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     60 * time.Second,
	}

	childProxy := &httputil.ReverseProxy{
		Transport:     sharedTransport,
		FlushInterval: 200 * time.Millisecond,
		Director: func(r *http.Request) {
			port := resolveActivePort()
			if port == 0 {
				// Mark target invalid; serve-side check below returns 502.
				r.URL.Scheme = ""
				r.URL.Host = ""
				return
			}
			r.URL.Scheme = "http"
			r.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)
			r.Host = r.URL.Host
		},
	}

	// proxyToChild forwards the request to the active child.
	proxyToChild := guard(func(w http.ResponseWriter, r *http.Request) {
		if resolveActivePort() == 0 {
			http.Error(w, `{"error":"no active child running"}`, http.StatusBadGateway)
			return
		}
		childProxy.ServeHTTP(w, r)
	})

	// proxyToDispatcher forwards the request to the Nexus dispatcher itself.
	// Used by the HUD for fleet-level endpoints (scatter-gather summary,
	// /status, workspace start/stop). [PILAR-XXVII/245.Q]
	dispatchPort := opts.DispatchPort
	if dispatchPort == 0 {
		dispatchPort = 9000
	}
	dispatchTarget, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", dispatchPort))
	dispatchProxy := httputil.NewSingleHostReverseProxy(dispatchTarget)
	dispatchProxy.Transport = sharedTransport
	proxyToDispatcher := guard(func(w http.ResponseWriter, r *http.Request) {
		dispatchProxy.ServeHTTP(w, r)
	})

	// Sub-FS rooted at static/ so http.FS maps /assets/x.js → static/assets/x.js.
	staticSub, err := fs.Sub(hudFS, "static")
	if err != nil {
		log.Fatalf("[NEXUS-HUD] embed sub failed: %v", err)
	}
	staticHandler := http.FileServer(http.FS(staticSub))

	// serveEmbedded delegates to http.FileServer with proper MIME types.
	serveEmbedded := guard(func(w http.ResponseWriter, r *http.Request) {
		// Cache-Control: long-lived for hashed asset filenames, short for index.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		staticHandler.ServeHTTP(w, r)
	})

	// /assets/* — embedded Vite output (hashed filenames).
	mux.HandleFunc("/assets/", serveEmbedded)

	// /favicon.svg, /icons.svg — embedded top-level static files.
	mux.HandleFunc("/favicon.svg", serveEmbedded)
	mux.HandleFunc("/icons.svg", serveEmbedded)

	// /legacy — preserved original Operator HUD. [PILAR-XXVII/245.Q]
	mux.HandleFunc("/legacy", guard(func(w http.ResponseWriter, _ *http.Request) {
		data, err := hudFS.ReadFile("static/legacy.html")
		if err != nil {
			http.Error(w, "legacy HUD not embedded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	}))

	// Dispatcher-level fleet endpoints. [PILAR-XXVII/245.Q]
	mux.HandleFunc("/status", proxyToDispatcher)
	mux.HandleFunc("/api/v1/services", proxyToDispatcher)
	mux.HandleFunc("/api/v1/workspaces", proxyToDispatcher)
	mux.HandleFunc("/api/v1/workspaces/", proxyToDispatcher)
	mux.HandleFunc("/api/v1/metrics/summary", proxyToDispatcher)
	mux.HandleFunc("/api/v1/presence", proxyToDispatcher)            // [337.A]
	mux.HandleFunc("/api/v1/federation/overview", proxyToDispatcher) // [341]

	// GET / — SPA entry. Any other path (not captured above) proxies to the
	// active child so SSE streams / child-local endpoints still work from
	// the HUD origin.
	mux.HandleFunc("/", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			data, err := hudFS.ReadFile("static/index.html")
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(data)
			return
		}
		// Unknown path that isn't a static asset and isn't a dispatcher
		// route → assume it's a child-local endpoint (/events, /mcts,
		// /snapshot, /api/v1/sre/*, etc.).
		proxyToChild(w, r)
	}))


	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE streams are long-lived
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("[NEXUS-HUD] Operator HUD listening at http://%s/", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[NEXUS-HUD] Dashboard server error: %v", err)
	}
}
