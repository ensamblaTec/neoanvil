package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAllow_DrainAndDeny(t *testing.T) {
	b := New(100, 60000)
	// Drain the bucket.
	for i := range 100 {
		if !b.Allow(1) {
			t.Fatalf("expected allow at i=%d", i)
		}
	}
	// Next should be denied.
	if b.Allow(1) {
		t.Error("expected deny when bucket empty")
	}
}

func TestRefill(t *testing.T) {
	// 60 tokens/minute = 1 token/second. Start empty.
	b := New(60, 60)
	b.mu.Lock()
	b.current = 0
	b.mu.Unlock()

	// After ~1s the bucket should have ~1 token.
	time.Sleep(1100 * time.Millisecond)
	if !b.Allow(1) {
		t.Error("expected token after 1s refill")
	}
}

func TestWaitFor_ContextCancel(t *testing.T) {
	b := New(10, 1) // 1 token/minute — nearly no refill
	b.mu.Lock()
	b.current = 0
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := b.WaitFor(ctx, 10)
	if err == nil {
		t.Error("expected context cancel error")
	}
}

func TestWaitFor_Succeeds(t *testing.T) {
	b := New(100, 60000)
	ctx := context.Background()
	if err := b.WaitFor(ctx, 50); err != nil {
		t.Errorf("WaitFor: %v", err)
	}
	if b.Available() != 50 {
		t.Errorf("available = %d, want 50", b.Available())
	}
}

func TestConcurrent(t *testing.T) {
	b := New(1000, 6000000) // very fast refill — focus on race detector
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			b.Allow(1)
		})
	}
	wg.Wait()
	// Just verifying no race/panic — no assertion needed beyond completing.
}
