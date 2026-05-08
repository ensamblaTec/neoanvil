package state

import (
	"strings"
	"testing"
)

func resetErrorRing() {
	globalErrorRing.mu.Lock()
	globalErrorRing.head = 0
	globalErrorRing.count = 0
	for i := range globalErrorRing.buf {
		globalErrorRing.buf[i] = ""
	}
	globalErrorRing.mu.Unlock()
}

// TestRecordError_AppendAndRead [Épica 231.D]
func TestRecordError_AppendAndRead(t *testing.T) {
	resetErrorRing()
	RecordError("err-1")
	RecordError("err-2")
	RecordError("err-3")
	got := recentErrors(5)
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d: %v", len(got), got)
	}
	// Most-recent order: err-1, err-2, err-3 (ring buffer preserves insertion order)
	want := []string{"err-1", "err-2", "err-3"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: want %q got %q", i, w, got[i])
		}
	}
}

// TestRecentErrors_CapAt5 [Épica 231.D]
func TestRecentErrors_CapAt5(t *testing.T) {
	resetErrorRing()
	for i := range 10 {
		RecordError("err-" + string(rune('A'+i)))
	}
	got := recentErrors(10) // ask for 10, should cap at 5
	if len(got) != 5 {
		t.Errorf("recentErrors cap should be 5, got %d", len(got))
	}
}

// TestRecentErrors_RingWrapAround [Épica 231.D]
func TestRecentErrors_RingWrapAround(t *testing.T) {
	resetErrorRing()
	// Push 10 into a ring of size 8 — oldest 2 get overwritten.
	for i := range 10 {
		RecordError("err-" + string(rune('A'+i)))
	}
	// Fetch 5 most recent — should be F, G, H, I, J
	got := recentErrors(5)
	if len(got) != 5 {
		t.Fatalf("want 5 got %d: %v", len(got), got)
	}
	expected := []string{"err-F", "err-G", "err-H", "err-I", "err-J"}
	for i, w := range expected {
		if got[i] != w {
			t.Errorf("pos %d: want %q got %q", i, w, got[i])
		}
	}
}

// TestRecentErrors_Empty [Épica 231.D]
func TestRecentErrors_Empty(t *testing.T) {
	resetErrorRing()
	if got := recentErrors(5); got != nil {
		t.Errorf("expected nil on empty ring, got %v", got)
	}
}

// TestRecentErrors_ZeroN [Épica 231.D]
func TestRecentErrors_ZeroN(t *testing.T) {
	resetErrorRing()
	RecordError("err")
	if got := recentErrors(0); got != nil {
		t.Errorf("n=0 should return nil, got %v", got)
	}
}

// TestCaptureCausalContext_Shape [Épica 231.D]
func TestCaptureCausalContext_Shape(t *testing.T) {
	resetErrorRing()
	RecordError("preceding error")
	ctx := CaptureCausalContext("test_trigger", "parent-123")
	if ctx == nil {
		t.Fatal("CaptureCausalContext returned nil")
	}
	if ctx.TriggerEvent != "test_trigger" {
		t.Errorf("TriggerEvent mismatch: %s", ctx.TriggerEvent)
	}
	if ctx.ParentID != "parent-123" {
		t.Errorf("ParentID mismatch: %s", ctx.ParentID)
	}
	if ctx.HeapMB < 0 {
		t.Errorf("HeapMB should be non-negative, got %v", ctx.HeapMB)
	}
	if ctx.CPULoad < 0 || ctx.CPULoad > 1.0 {
		t.Errorf("CPULoad should be in [0, 1.0], got %v", ctx.CPULoad)
	}
	// PriorErrors should include the err we just recorded.
	if !sliceContains(ctx.PriorErrors, "preceding error") {
		t.Errorf("PriorErrors missing recorded error: %v", ctx.PriorErrors)
	}
}

// TestEstimateCPULoad_InRange [Épica 231.D]
func TestEstimateCPULoad_InRange(t *testing.T) {
	load := estimateCPULoad()
	if load < 0 || load > 1.0 {
		t.Errorf("load out of range: %v", load)
	}
}

// TestReadRAPLWatts_NoErr [Épica 231.D]
func TestReadRAPLWatts_NoErr(t *testing.T) {
	// Just ensure it doesn't panic + returns sane value (0 when sysfs missing).
	w := readRAPLWatts()
	if w < 0 {
		t.Errorf("RAPL watts should be >= 0, got %v", w)
	}
}

func sliceContains(s []string, substr string) bool {
	for _, x := range s {
		if strings.Contains(x, substr) {
			return true
		}
	}
	return false
}
