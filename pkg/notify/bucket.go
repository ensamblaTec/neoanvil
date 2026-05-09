package notify

import (
	"sync"
	"time"
)

// tokenBucket is a minimal per-webhook QPS limiter. Refills `burst`
// tokens once per minute (matches the BurstPerMinute config knob).
// Not allocation-free, but the notifier path is far from hot.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   int
	capacity int
	lastFill time.Time
}

func newTokenBucket(burstPerMinute int) *tokenBucket {
	if burstPerMinute < 1 {
		burstPerMinute = 1
	}
	return &tokenBucket{
		tokens:   burstPerMinute,
		capacity: burstPerMinute,
		lastFill: time.Now(),
	}
}

// take consumes one token if available. Refills before checking so
// callers always see the most recent capacity.
func (b *tokenBucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.lastFill) >= time.Minute {
		b.tokens = b.capacity
		b.lastFill = now
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}
