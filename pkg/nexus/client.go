// Package nexus — client.go: graceful wrapper for neo-mcp → Nexus /internal/* calls.
// PILAR LXVIII / Épica 360.A.
//
// Problem: multiple features (337.A heartbeat, 351.C neo_debt, 353.A boot check,
// 352.A BRIEFING nexus-debt) make outgoing HTTP to the Nexus dispatcher. When
// Nexus is down, each call wastes 500ms–5s on timeout, and BRIEFING hangs.
//
// Solution: thin Client wrapper with per-endpoint circuit breakers. After N
// consecutive failures, the breaker opens and subsequent calls return
// ErrNexusUnavailable immediately (< 1µs) until the reset window passes.
//
// Callers use IsAvailable() to decide whether to try, and check errors.Is(err,
// ErrNexusUnavailable) to skip with a warning log + continue. No hard hangs.
package nexus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// ErrNexusUnavailable is returned by Client methods when the circuit breaker
// for the requested endpoint is open (repeated failures) OR when the Nexus
// base URL is empty (not running under Nexus). Callers should treat this as
// soft failure — log + continue — NOT as an error condition.
var ErrNexusUnavailable = errors.New("nexus: dispatcher unavailable — circuit open or not configured")

// Client issues /internal/* requests to the Nexus dispatcher with graceful
// degradation. Safe for concurrent use.
type Client struct {
	base string
	http *http.Client

	// Per-endpoint circuit breakers. Key = path (e.g. "/internal/knowledge/broadcast").
	// Value = *sre.CircuitBreaker[[]byte].
	breakers sync.Map

	// Configuration — read-only post-construction.
	maxFailures  int
	resetTimeout time.Duration

	// available is the aggregate tri-state exposed by IsAvailable(). Flipped
	// to false when the LAST call errored with circuit-open, back to true on
	// successful call. A single source of truth for BRIEFING + HUD.
	available atomic.Bool
}

// NewClient constructs a Client. base is the Nexus dispatcher URL (e.g.
// "http://127.0.0.1:9000") — empty string means "not running under Nexus"
// and all calls return ErrNexusUnavailable without hitting the network.
//
// httpClient is typically sre.SafeInternalHTTPClient(timeout). maxFailures
// and resetTimeout configure the per-endpoint breaker (recommended: 5 and
// 30s respectively — same as the embedder breaker for consistency).
func NewClient(base string, httpClient *http.Client, maxFailures int, resetTimeout time.Duration) *Client {
	if maxFailures <= 0 {
		maxFailures = 5
	}
	if resetTimeout <= 0 {
		resetTimeout = 30 * time.Second
	}
	c := &Client{
		base:         base,
		http:         httpClient,
		maxFailures:  maxFailures,
		resetTimeout: resetTimeout,
	}
	// Start optimistic: available=true when base is set.
	c.available.Store(base != "")
	return c
}

// IsAvailable returns true when the dispatcher is reachable (last call
// succeeded OR no call yet). Used by BRIEFING + HUD to decide whether to
// even attempt optional Nexus queries. Returns false when base is empty
// (no Nexus configured).
func (c *Client) IsAvailable() bool {
	return c.available.Load()
}

// Base returns the configured dispatcher URL (may be empty).
func (c *Client) Base() string { return c.base }

func (c *Client) breakerFor(endpoint string) *sre.CircuitBreaker[[]byte] {
	if cached, ok := c.breakers.Load(endpoint); ok {
		return cached.(*sre.CircuitBreaker[[]byte])
	}
	fresh := sre.NewCircuitBreaker[[]byte](c.maxFailures, c.resetTimeout)
	actual, _ := c.breakers.LoadOrStore(endpoint, fresh)
	return actual.(*sre.CircuitBreaker[[]byte])
}

// Post issues a POST to base+endpoint with application/json body. Returns
// the response body (fully drained) or an error. When the breaker is open
// or base is empty, returns ErrNexusUnavailable wrapped.
func (c *Client) Post(ctx context.Context, endpoint string, body []byte) ([]byte, error) {
	if c.base == "" {
		return nil, ErrNexusUnavailable
	}
	breaker := c.breakerFor(endpoint)
	result, err := breaker.Execute(ctx, func(execCtx context.Context) ([]byte, error) {
		req, rerr := http.NewRequestWithContext(execCtx, http.MethodPost, c.base+endpoint, bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		resp, rerr := c.http.Do(req)
		if rerr != nil {
			return nil, rerr
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("nexus: %s returned %d", endpoint, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	})
	if err != nil {
		if errors.Is(err, sre.ErrCircuitOpen) {
			c.available.Store(false)
			return nil, fmt.Errorf("%w: %v", ErrNexusUnavailable, err)
		}
		return nil, err
	}
	c.available.Store(true)
	return result, nil
}

// Get issues a GET to base+endpoint. Same semantics as Post — circuit-broken,
// returns ErrNexusUnavailable when the breaker is open.
func (c *Client) Get(ctx context.Context, endpoint string) ([]byte, error) {
	if c.base == "" {
		return nil, ErrNexusUnavailable
	}
	breaker := c.breakerFor(endpoint)
	result, err := breaker.Execute(ctx, func(execCtx context.Context) ([]byte, error) {
		req, rerr := http.NewRequestWithContext(execCtx, http.MethodGet, c.base+endpoint, nil)
		if rerr != nil {
			return nil, rerr
		}
		resp, rerr := c.http.Do(req)
		if rerr != nil {
			return nil, rerr
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("nexus: %s returned %d", endpoint, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	})
	if err != nil {
		if errors.Is(err, sre.ErrCircuitOpen) {
			c.available.Store(false)
			return nil, fmt.Errorf("%w: %v", ErrNexusUnavailable, err)
		}
		return nil, err
	}
	c.available.Store(true)
	return result, nil
}

// BreakerStats returns a snapshot of per-endpoint breaker state for observability.
// Returned map is not thread-safe to mutate — treat as read-only.
func (c *Client) BreakerStats() map[string]string {
	out := make(map[string]string)
	c.breakers.Range(func(key, _ any) bool {
		endpoint := key.(string)
		// We can't read state without exposing internal breaker API. For now,
		// just list registered endpoints. Future: breaker exposes State() method.
		out[endpoint] = "registered"
		return true
	})
	return out
}
