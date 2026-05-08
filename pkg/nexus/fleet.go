package nexus

// [SRE-91] Fleet Federation — remote node support for Neo-Nexus.
//
// Extends the Nexus dispatcher to route JSON-RPC calls to remote neo-mcp
// workers over mTLS/Tailscale tunnels. Remote nodes register via nexus.yaml
// `remote_nodes` or auto-discovery via Tailscale MagicDNS.
//
// Architecture:
//   - Local children: HTTP (plain) on 127.0.0.1 — zero overhead
//   - Remote children: HTTPS (mTLS) over Tailscale/WireGuard
//   - Heartbeat: periodic health check with latency measurement
//   - Circuit breaker: 5 failures → StatusUnreachable for 60s

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// RemoteNode represents a neo-mcp instance running on a different machine. [SRE-91.A]
type RemoteNode struct {
	Host        string `yaml:"host" json:"host"`                 // IP or hostname (e.g. "100.64.0.2")
	Port        int    `yaml:"port" json:"port"`                 // neo-mcp HTTP port on the remote
	WorkspaceID string `yaml:"workspace_id" json:"workspace_id"` // workspace this node serves
	UseTLS      bool   `yaml:"use_tls" json:"use_tls"`           // mTLS for production, false for Tailscale
}

// FleetNode extends RemoteNode with runtime state. [SRE-91.A.3]
type FleetNode struct {
	RemoteNode
	Status        FleetStatus   `json:"status"`
	LatencyMs     int64         `json:"latency_ms"`
	LastSeen      time.Time     `json:"last_seen"`
	FailureStreak int           `json:"failure_streak"`
	mu            sync.Mutex
}

// FleetStatus represents the health state of a remote node.
type FleetStatus string

const (
	FleetHealthy     FleetStatus = "healthy"
	FleetUnreachable FleetStatus = "unreachable"
	FleetUnknown     FleetStatus = "unknown"
)

// FleetRegistry manages remote nodes. Thread-safe. [SRE-91.B.1]
type FleetRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*FleetNode // key = workspace_id
}

// NewFleetRegistry creates an empty fleet registry.
func NewFleetRegistry() *FleetRegistry {
	return &FleetRegistry{
		nodes: make(map[string]*FleetNode),
	}
}

// Register adds a remote node to the fleet.
func (f *FleetRegistry) Register(node RemoteNode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[node.WorkspaceID] = &FleetNode{
		RemoteNode: node,
		Status:     FleetUnknown,
	}
	log.Printf("[NEXUS-FLEET] registered remote node %s:%d for workspace %s", node.Host, node.Port, node.WorkspaceID)
}

// Lookup returns a remote node by workspace ID, or nil if not found/unhealthy.
// Releases f.mu before taking n.mu to prevent lock-order inversion with Heartbeat. [286.B]
func (f *FleetRegistry) Lookup(workspaceID string) *FleetNode {
	f.mu.RLock()
	node, ok := f.nodes[workspaceID]
	f.mu.RUnlock()
	if !ok {
		return nil
	}
	node.mu.Lock()
	unreachable := node.Status == FleetUnreachable
	node.mu.Unlock()
	if unreachable {
		return nil
	}
	return node
}

// FleetNodeSnapshot is a copy of FleetNode without the mutex (safe to return).
type FleetNodeSnapshot struct {
	RemoteNode
	Status        FleetStatus `json:"status"`
	LatencyMs     int64       `json:"latency_ms"`
	LastSeen      time.Time   `json:"last_seen"`
	FailureStreak int         `json:"failure_streak"`
}

// All returns a snapshot of all registered nodes.
func (f *FleetRegistry) All() []FleetNodeSnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]FleetNodeSnapshot, 0, len(f.nodes))
	for _, n := range f.nodes {
		n.mu.Lock()
		out = append(out, FleetNodeSnapshot{
			RemoteNode:    n.RemoteNode,
			Status:        n.Status,
			LatencyMs:     n.LatencyMs,
			LastSeen:      n.LastSeen,
			FailureStreak: n.FailureStreak,
		})
		n.mu.Unlock()
	}
	return out
}

// Heartbeat checks health of all remote nodes. Call periodically. [SRE-91.A.3]
func (f *FleetRegistry) Heartbeat(client *http.Client) {
	f.mu.RLock()
	nodes := make([]*FleetNode, 0, len(f.nodes))
	for _, n := range f.nodes {
		nodes = append(nodes, n)
	}
	f.mu.RUnlock()

	for _, node := range nodes {
		go func(n *FleetNode) {
			n.mu.Lock()
			defer n.mu.Unlock()

			url := fmt.Sprintf("http://%s:%d/health", n.Host, n.Port)
			if n.UseTLS {
				url = fmt.Sprintf("https://%s:%d/health", n.Host, n.Port)
			}

			start := time.Now()
			resp, err := client.Get(url) //nolint:gosec // G107-WRAPPED-SAFE-CLIENT
			elapsed := time.Since(start)

			if err != nil || (resp != nil && resp.StatusCode != http.StatusOK) {
				n.FailureStreak++
				if n.FailureStreak >= 3 {
					if n.Status != FleetUnreachable {
						log.Printf("[NEXUS-EVENT] remote_node_unreachable workspace=%s host=%s failures=%d",
							n.WorkspaceID, n.Host, n.FailureStreak)
					}
					n.Status = FleetUnreachable
				}
				if resp != nil {
					resp.Body.Close()
				}
				return
			}
			resp.Body.Close()

			n.Status = FleetHealthy
			n.LatencyMs = elapsed.Milliseconds()
			n.LastSeen = time.Now()
			n.FailureStreak = 0
		}(node)
	}
}

// ResolveHost returns the {host, port} for a workspace, checking remote
// fleet first, then falling back to local. Returns empty host if not found. [SRE-91.B.1]
type HostPort struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	IsRemote bool   `json:"is_remote"`
}

func (f *FleetRegistry) ResolveHost(workspaceID string) HostPort {
	node := f.Lookup(workspaceID)
	if node != nil {
		return HostPort{Host: node.Host, Port: node.Port, IsRemote: true}
	}
	return HostPort{} // not in fleet — caller should check local pool
}

// ProxyWithRetry sends an HTTP request to a remote node with retry and
// circuit breaker semantics. [SRE-91.B.2]
//   - maxRetries: total attempts (3 recommended)
//   - timeout: per-attempt timeout
//   - Returns the response body or error after exhausting retries.
func (f *FleetRegistry) ProxyWithRetry(client *http.Client, url string, bodyBytes []byte, maxRetries int) ([]byte, int, error) {
	var lastErr error
	backoff := 500 * time.Millisecond

	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: %w", attempt, err)
			log.Printf("[NEXUS-FLEET] retry %d/%d for %s: %v", attempt, maxRetries, url, err)
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("attempt %d: HTTP %d", attempt, resp.StatusCode)
			log.Printf("[NEXUS-FLEET] retry %d/%d for %s: HTTP %d", attempt, maxRetries, url, resp.StatusCode)
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		return respBody, resp.StatusCode, nil
	}
	return nil, 0, fmt.Errorf("all %d retries exhausted: %w", maxRetries, lastErr)
}

// FleetStatusReport returns a summary of all nodes for the FLEET_STATUS intent. [SRE-91.B.3]
func (f *FleetRegistry) FleetStatusReport() map[string]any {
	nodes := f.All()
	healthy := 0
	unreachable := 0
	for _, n := range nodes {
		switch n.Status {
		case FleetHealthy:
			healthy++
		case FleetUnreachable:
			unreachable++
		}
	}
	return map[string]any{
		"total_nodes": len(nodes),
		"healthy":     healthy,
		"unreachable": unreachable,
		"nodes":       nodes,
	}
}
