// Package storage — tsnet.go: Tailscale-network BrainStore driver. PILAR XXVI / 137.E.3.
//
// TsnetServer wraps any BrainStore and serves its API over a tsnet listener so
// other nodes on the same tailnet can reach it without a public endpoint.
//
// TsnetStore is a BrainStore client that routes all calls through a tsnet.Server
// and speaks the same HTTP API. Together they allow two-device sync scenarios like
// "phone backs up brain to desktop" or "laptop pulls from a home NAS"—all through
// the Tailscale mesh without opening firewall ports.
//
// Wire protocol (all paths under /v1/):
//
//	PUT    /v1/objects/<key>          — body is the raw blob
//	GET    /v1/objects/<key>          — response body is the raw blob
//	GET    /v1/list?prefix=<p>        — JSON []ChunkRef
//	DELETE /v1/objects/<key>
//	POST   /v1/locks/<name>           — JSON body: {"holder":"..", "ttl_ns": N}
//	DELETE /v1/locks/<name>           — JSON body: Lease
//
// Awaiting real tailnet connectivity validation before marking 137.E.3 complete.

package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"tailscale.com/tsnet"
)

// tsnetPort is the default port the TsnetServer binds on the tailnet.
const tsnetPort = "8321"

// ──────────────────────────────────────────────────────────────────────────────
// TsnetServer
// ──────────────────────────────────────────────────────────────────────────────

// TsnetServer exposes a BrainStore over HTTP on a tsnet listener. It manages
// the tsnet.Server lifecycle; callers must call Close when done.
type TsnetServer struct {
	ts       *tsnet.Server
	inner    BrainStore
	listener net.Listener
	srv      *http.Server
	done     chan struct{}
}

// TsnetServerConfig configures a TsnetServer.
type TsnetServerConfig struct {
	// AuthKey is a Tailscale auth key for headless enrollment.
	AuthKey string
	// Hostname is the node's tailnet hostname (e.g. "neo-brain-desktop").
	Hostname string
	// StateDir is where tsnet persists its state. Defaults to a subdir of
	// the inner store root when empty.
	StateDir string
	// Port is the TCP port to listen on. Defaults to tsnetPort.
	Port string
}

// NewTsnetServer creates and starts a TsnetServer backed by inner.
// The server is fully started (listening) when this returns.
func NewTsnetServer(inner BrainStore, cfg TsnetServerConfig) (*TsnetServer, error) {
	port := cfg.Port
	if port == "" {
		port = tsnetPort
	}
	ts := &tsnet.Server{
		AuthKey:  cfg.AuthKey,
		Hostname: cfg.Hostname,
		Dir:      cfg.StateDir,
		Logf:     func(string, ...any) {}, // suppress tsnet verbose logs
	}
	if err := ts.Start(); err != nil {
		return nil, fmt.Errorf("TsnetServer: start tsnet: %w", err)
	}
	ln, err := ts.Listen("tcp", ":"+port)
	if err != nil {
		_ = ts.Close()
		return nil, fmt.Errorf("TsnetServer: listen :%s: %w", port, err)
	}
	s := &TsnetServer{
		ts:       ts,
		inner:    inner,
		listener: ln,
		done:     make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/objects/", s.handleObjects)
	mux.HandleFunc("/v1/list", s.handleList)
	mux.HandleFunc("/v1/locks/", s.handleLocks)
	s.srv = &http.Server{Handler: mux}
	go func() {
		defer close(s.done)
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// Close shuts down the HTTP server, the tsnet node, and the inner BrainStore.
func (s *TsnetServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	<-s.done
	_ = s.ts.Close()
	return s.inner.Close()
}

// Addr returns the tsnet address at which this server is reachable on the
// tailnet. Format: "<tailnet-ip>:<port>".
func (s *TsnetServer) Addr() string {
	ip4, _ := s.ts.TailscaleIPs()
	return fmt.Sprintf("%s:%s", ip4, tsnetPort)
}

// ── HTTP handlers ────────────────────────────────────────────────────────────

func (s *TsnetServer) handleObjects(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/v1/objects/")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		n, err := s.inner.Put(key, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Bytes-Written", fmt.Sprint(n))
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		rc, err := s.inner.Get(key)
		if err != nil {
			if err == ErrNotFound {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, rc)
	case http.MethodDelete:
		if err := s.inner.Delete(key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *TsnetServer) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	refs, err := s.inner.List(prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if refs == nil {
		refs = []ChunkRef{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(refs)
}

type lockRequest struct {
	Holder string `json:"holder"`
	TTLNS  int64  `json:"ttl_ns"`
}

func (s *TsnetServer) handleLocks(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	if name == "" || name == "." {
		http.Error(w, "missing lock name", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPost: // acquire
		var req lockRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		lease, err := s.inner.Lock(name, req.Holder, time.Duration(req.TTLNS))
		if err != nil {
			if err == ErrLockHeld {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lease)
	case http.MethodDelete: // release
		var lease Lease
		if err := json.NewDecoder(r.Body).Decode(&lease); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.inner.Unlock(lease); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TsnetStore
// ──────────────────────────────────────────────────────────────────────────────

// TsnetStore is a BrainStore that routes all operations to a remote
// TsnetServer over the Tailscale mesh network.
type TsnetStore struct {
	ts      *tsnet.Server
	baseURL string       // e.g. "http://100.64.1.1:8321"
	client  *http.Client // tsnet-aware HTTP client
	closed  bool
}

// TsnetStoreConfig configures a TsnetStore.
type TsnetStoreConfig struct {
	// AuthKey is a Tailscale auth key for headless enrollment.
	AuthKey string
	// Hostname is this client node's hostname in the tailnet.
	Hostname string
	// StateDir is where tsnet persists client state.
	StateDir string
	// ServerAddr is the tailnet address of the TsnetServer in host:port form
	// (e.g. "neo-brain-desktop:8321" or "100.64.1.1:8321").
	ServerAddr string
}

// NewTsnetStore creates a TsnetStore that connects to the given server address.
// The tsnet node is enrolled in the tailnet immediately; subsequent operations
// are lazy (no TCP connection is held between calls).
func NewTsnetStore(cfg TsnetStoreConfig) (*TsnetStore, error) {
	if cfg.ServerAddr == "" {
		return nil, fmt.Errorf("TsnetStore: ServerAddr required")
	}
	ts := &tsnet.Server{
		AuthKey:  cfg.AuthKey,
		Hostname: cfg.Hostname,
		Dir:      cfg.StateDir,
		Logf:     func(string, ...any) {},
	}
	if err := ts.Start(); err != nil {
		return nil, fmt.Errorf("TsnetStore: start tsnet: %w", err)
	}
	baseURL := "http://" + cfg.ServerAddr
	if _, err := url.Parse(baseURL); err != nil {
		_ = ts.Close()
		return nil, fmt.Errorf("TsnetStore: invalid server addr: %w", err)
	}
	return &TsnetStore{
		ts:      ts,
		baseURL: baseURL,
		client:  ts.HTTPClient(),
	}, nil
}

// Put streams r to the remote store under key.
func (s *TsnetStore) Put(key string, r io.Reader) (int64, error) {
	u := s.baseURL + "/v1/objects/" + url.PathEscape(key)
	req, err := http.NewRequest(http.MethodPut, u, r)
	if err != nil {
		return 0, fmt.Errorf("TsnetStore.Put: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("TsnetStore.Put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("TsnetStore.Put: server error %d: %s", resp.StatusCode, body)
	}
	return 0, nil // server writes the byte count as a header; callers don't need it
}

// Get returns a reader for the remote object. Caller MUST Close it.
func (s *TsnetStore) Get(key string) (io.ReadCloser, error) {
	u := s.baseURL + "/v1/objects/" + url.PathEscape(key)
	resp, err := s.client.Get(u) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: baseURL is operator-controlled TsnetStoreConfig.ServerAddr; routed through tsnet loopback
	if err != nil {
		return nil, fmt.Errorf("TsnetStore.Get: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("TsnetStore.Get: server error %d: %s", resp.StatusCode, body)
	}
	return resp.Body, nil
}

// List returns refs whose key has the given prefix.
func (s *TsnetStore) List(prefix string) ([]ChunkRef, error) {
	u := s.baseURL + "/v1/list?prefix=" + url.QueryEscape(prefix)
	resp, err := s.client.Get(u) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: as above
	if err != nil {
		return nil, fmt.Errorf("TsnetStore.List: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("TsnetStore.List: server error %d: %s", resp.StatusCode, body)
	}
	var refs []ChunkRef
	if err := json.NewDecoder(resp.Body).Decode(&refs); err != nil {
		return nil, fmt.Errorf("TsnetStore.List: decode: %w", err)
	}
	return refs, nil
}

// Delete removes the remote object. Idempotent.
func (s *TsnetStore) Delete(key string) error {
	u := s.baseURL + "/v1/objects/" + url.PathEscape(key)
	req, err := http.NewRequest(http.MethodDelete, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("TsnetStore.Delete: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("TsnetStore.Delete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("TsnetStore.Delete: server error %d: %s", resp.StatusCode, body)
	}
	return nil
}

// Lock acquires a named distributed lock via the remote server.
func (s *TsnetStore) Lock(name, holder string, ttl time.Duration) (Lease, error) {
	payload := lockRequest{Holder: holder, TTLNS: int64(ttl)}
	body, _ := json.Marshal(payload)
	u := s.baseURL + "/v1/locks/" + url.PathEscape(name)
	resp, err := s.client.Post(u, "application/json", bytes.NewReader(body)) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT: as above
	if err != nil {
		return Lease{}, fmt.Errorf("TsnetStore.Lock: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return Lease{}, ErrLockHeld
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Lease{}, fmt.Errorf("TsnetStore.Lock: server error %d: %s", resp.StatusCode, errBody)
	}
	var lease Lease
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		return Lease{}, fmt.Errorf("TsnetStore.Lock: decode lease: %w", err)
	}
	return lease, nil
}

// Unlock releases the remote lock. Idempotent.
func (s *TsnetStore) Unlock(lease Lease) error {
	body, _ := json.Marshal(lease)
	u := s.baseURL + "/v1/locks/" + url.PathEscape(lease.Name)
	req, err := http.NewRequest(http.MethodDelete, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("TsnetStore.Unlock: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("TsnetStore.Unlock: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("TsnetStore.Unlock: server error %d: %s", resp.StatusCode, errBody)
	}
	return nil
}

// Close shuts down the tsnet node. Subsequent operations will fail.
func (s *TsnetStore) Close() error {
	s.closed = true
	return s.ts.Close()
}
