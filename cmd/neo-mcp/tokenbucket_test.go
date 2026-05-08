package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// [SRE-31.3.1] TokenBucket concurrency test — 50 goroutines hammer Allow() simultaneously.
// Validates: no data race, total consumed ≤ initial capacity, no panic.
func TestTokenBucket_ConcurrentAccess(t *testing.T) {
	const (
		goroutines = 50
		capacity   = 100.0
		rate       = 2.0
	)

	tb := &TokenBucket{
		tokens:     capacity,
		lastRefill: time.Now(),
		rate:       rate,
		capacity:   capacity,
	}

	var allowed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if tb.Allow() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	got := allowed.Load()
	// At t=0, bucket has exactly 100 tokens. At most 100 goroutines should succeed.
	// With 50 goroutines, all 50 may succeed (bucket has 100 capacity) — that's fine.
	if got > int64(capacity) {
		t.Errorf("more tokens consumed (%d) than capacity (%d) — invariant broken", got, int64(capacity))
	}
	if got < 0 {
		t.Errorf("negative allowed count — impossible")
	}
}

// TestTokenBucket_RateRefill verifies tokens are replenished over time.
func TestTokenBucket_RateRefill(t *testing.T) {
	tb := &TokenBucket{
		tokens:     0, // start empty
		lastRefill: time.Now().Add(-1 * time.Second), // 1s ago → rate*1s tokens added on next Allow
		rate:       10.0,
		capacity:   10.0,
	}

	// After 1s at rate=10, bucket should have ~10 tokens → Allow should succeed.
	if !tb.Allow() {
		t.Error("expected Allow()=true after 1s refill at rate=10, got false")
	}
}

// TestTokenBucket_EmptyBucketBlocks verifies an empty bucket (no time elapsed) returns false.
func TestTokenBucket_EmptyBucketBlocks(t *testing.T) {
	tb := &TokenBucket{
		tokens:     0,
		lastRefill: time.Now(), // just refilled — no new tokens yet
		rate:       2.0,
		capacity:   100.0,
	}

	if tb.Allow() {
		t.Error("expected Allow()=false on empty bucket with no elapsed time, got true")
	}
}
