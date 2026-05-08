// Package ratelimit provides a thread-safe token bucket rate limiter (PILAR XXIV / 131.B).
//
// Default config (from neo.yaml):
//
//	deepseek.rate_limit_tokens_per_minute: 60000
//	deepseek.burst: 10000
package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TokenBucket implements a token bucket algorithm with proportional refill.
// Safe for concurrent use.
type TokenBucket struct {
	mu         sync.Mutex
	capacity   int64
	refillRate float64 // tokens per nanosecond
	current    float64
	lastRefill time.Time

	// Metrics — monotonic counters, never reset.
	waitsTotal   int64
	waitNsTotal  int64
}

// New creates a TokenBucket.
//   - capacity: maximum burst size in tokens
//   - tokensPerMinute: steady-state refill rate
func New(capacity, tokensPerMinute int64) *TokenBucket {
	return &TokenBucket{
		capacity:   capacity,
		refillRate: float64(tokensPerMinute) / float64(time.Minute),
		current:    float64(capacity),
		lastRefill: time.Now(),
	}
}

// Allow attempts to consume n tokens without blocking.
// Returns true if tokens were available; false if the bucket lacks capacity.
func (b *TokenBucket) Allow(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if float64(n) > b.current {
		return false
	}
	b.current -= float64(n)
	return true
}

// WaitFor blocks until n tokens are available or ctx is cancelled.
// Returns an error immediately if n exceeds the bucket capacity (would never be satisfiable).
// Poll interval is 100ms. Returns ctx.Err() on cancellation.
func (b *TokenBucket) WaitFor(ctx context.Context, n int64) error {
	if n > b.capacity {
		return fmt.Errorf("ratelimit: request %d tokens exceeds bucket capacity %d", n, b.capacity)
	}
	if b.Allow(n) {
		return nil
	}
	start := time.Now()
	b.mu.Lock()
	b.waitsTotal++
	b.mu.Unlock()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.waitNsTotal += int64(time.Since(start))
			b.mu.Unlock()
			return ctx.Err()
		case <-ticker.C:
			if b.Allow(n) {
				b.mu.Lock()
				b.waitNsTotal += int64(time.Since(start))
				b.mu.Unlock()
				return nil
			}
		}
	}
}

// Stats returns lifetime metric counters.
func (b *TokenBucket) Stats() (waitsTotal int64, waitMsTotal int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waitsTotal, b.waitNsTotal / int64(time.Millisecond)
}

// Available returns the current token count (informational; may change immediately).
func (b *TokenBucket) Available() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	return int64(b.current)
}

// refill adds tokens proportional to time elapsed since the last call.
// Must be called with b.mu held.
func (b *TokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill)
	b.lastRefill = now
	b.current += b.refillRate * float64(elapsed)
	if b.current > float64(b.capacity) {
		b.current = float64(b.capacity)
	}
}
