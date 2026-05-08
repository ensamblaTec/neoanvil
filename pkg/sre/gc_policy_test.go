package sre

import (
	"errors"
	"testing"
)

// TestApplyMemoryLimit_Zero verifies that limitMB=0 is a no-op — no crash,
// no log. [Épica 365.A]
func TestApplyMemoryLimit_Zero(t *testing.T) {
	ApplyMemoryLimit(0) // should not panic
}

// TestApplyMemoryLimit_Negative verifies graceful rejection of invalid input.
func TestApplyMemoryLimit_Negative(t *testing.T) {
	ApplyMemoryLimit(-100) // should log warning and return without crash
}

// TestApplyMemoryLimit_Positive verifies the limit is applied. We can't
// easily read back the value without hitting private runtime internals, so
// we just verify the call doesn't panic at a reasonable limit.
func TestApplyMemoryLimit_Positive(t *testing.T) {
	// Use a very high limit so we don't accidentally throttle the test binary.
	ApplyMemoryLimit(16384) // 16 GB
	// Restore unlimited via math.MaxInt64 so subsequent tests aren't constrained.
	ApplyMemoryLimit(0)
}

// TestGCWithPolicy_IdleNoOverride verifies that PhaseIdle runs fn directly
// without GOGC changes.
func TestGCWithPolicy_IdleNoOverride(t *testing.T) {
	called := false
	err := GCWithPolicy(PhaseIdle, func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Error("fn was not called")
	}
	if CurrentGCPolicy() != 0 {
		t.Errorf("idle phase should leave policy at 0, got %d", CurrentGCPolicy())
	}
}

// TestGCWithPolicy_CertifyAppliesAndRestores verifies that PhaseCertify sets
// GOGC=200 during execution and restores on exit.
func TestGCWithPolicy_CertifyAppliesAndRestores(t *testing.T) {
	// Track the in-fn policy value.
	var inside int
	err := GCWithPolicy(PhaseCertify, func() error {
		inside = CurrentGCPolicy()
		return nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if inside != 200 {
		t.Errorf("inside certify phase, GOGC should be 200, got %d", inside)
	}
	if CurrentGCPolicy() != 0 {
		t.Errorf("after exit, policy should restore to 0, got %d", CurrentGCPolicy())
	}
}

// TestGCWithPolicy_BulkIngest verifies PhaseBulkIngest applies GOGC=300.
func TestGCWithPolicy_BulkIngest(t *testing.T) {
	var inside int
	err := GCWithPolicy(PhaseBulkIngest, func() error {
		inside = CurrentGCPolicy()
		return nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if inside != 300 {
		t.Errorf("inside bulk_ingest phase, GOGC should be 300, got %d", inside)
	}
}

// TestGCWithPolicy_RestoresOnError verifies that an error return from fn
// still triggers the defer-restore of GOGC.
func TestGCWithPolicy_RestoresOnError(t *testing.T) {
	sentinel := errors.New("simulated failure")
	err := GCWithPolicy(PhaseCertify, func() error {
		return sentinel
	})
	if err != sentinel {
		t.Errorf("error should propagate, got %v", err)
	}
	if CurrentGCPolicy() != 0 {
		t.Errorf("policy should restore after error, got %d", CurrentGCPolicy())
	}
}
