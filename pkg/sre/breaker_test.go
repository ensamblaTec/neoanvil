package sre

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCircuitBreaker_ClosedInitialState [Épica 231.A]
func TestCircuitBreaker_ClosedInitialState(t *testing.T) {
	cb := NewCircuitBreaker[int](3, 30*time.Second)
	if s := cb.currentState(); s != StateClosed {
		t.Errorf("initial state should be StateClosed, got %v", s)
	}
}

// TestCircuitBreaker_SuccessPath [Épica 231.A]
func TestCircuitBreaker_SuccessPath(t *testing.T) {
	cb := NewCircuitBreaker[int](3, 30*time.Second)
	got, err := cb.Execute(context.Background(), func(_ context.Context) (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

// TestCircuitBreaker_OpensAfterMaxFailures [Épica 231.A]
func TestCircuitBreaker_OpensAfterMaxFailures(t *testing.T) {
	cb := NewCircuitBreaker[int](3, 1*time.Hour) // long reset so we can observe the Open state
	boom := errors.New("boom")
	for i := range 3 {
		_, err := cb.Execute(context.Background(), func(_ context.Context) (int, error) {
			return 0, boom
		})
		if err == nil {
			t.Fatalf("iteration %d should have errored", i)
		}
	}
	// Next call must fail fast with ErrCircuitOpen.
	_, err := cb.Execute(context.Background(), func(_ context.Context) (int, error) {
		t.Error("action must NOT run when breaker is open")
		return 0, nil
	})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got %v", err)
	}
}

// TestCircuitBreaker_TransitionsToHalfOpenAfterTimeout [Épica 231.A]
func TestCircuitBreaker_TransitionsToHalfOpenAfterTimeout(t *testing.T) {
	cb := NewCircuitBreaker[int](1, 30*time.Millisecond)
	boom := errors.New("boom")
	_, _ = cb.Execute(context.Background(), func(_ context.Context) (int, error) { return 0, boom })
	if s := cb.currentState(); s != StateOpen {
		t.Fatalf("expected Open after 1 failure with maxFailures=1, got %v", s)
	}
	time.Sleep(40 * time.Millisecond)
	if s := cb.currentState(); s != StateHalfOpen {
		t.Errorf("expected HalfOpen after reset timeout, got %v", s)
	}
}

// TestCircuitBreaker_RecoversOnSuccessFromHalfOpen [Épica 231.A]
func TestCircuitBreaker_RecoversOnSuccessFromHalfOpen(t *testing.T) {
	cb := NewCircuitBreaker[int](1, 10*time.Millisecond)
	_, _ = cb.Execute(context.Background(), func(_ context.Context) (int, error) {
		return 0, errors.New("x")
	})
	time.Sleep(20 * time.Millisecond)
	// Successful probe in HalfOpen must close the breaker.
	got, err := cb.Execute(context.Background(), func(_ context.Context) (int, error) {
		return 99, nil
	})
	if err != nil || got != 99 {
		t.Fatalf("expected success, got %v err=%v", got, err)
	}
	if s := cb.currentState(); s != StateClosed {
		t.Errorf("breaker should close after successful probe, got %v", s)
	}
}
