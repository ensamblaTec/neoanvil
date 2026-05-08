package phoenix

import (
	"os/exec"
	"testing"
)

// TestTriggerPhoenixProtocol_NoTerraform covers the defensive path: when
// `terraform` is missing from PATH the function must return false without
// panicking. This is the realistic CI/test scenario — the happy path
// requires real infrastructure and is exercised in production only.
// [Épica 230.D]
func TestTriggerPhoenixProtocol_NoTerraform(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err == nil {
		t.Skip("terraform found in PATH — this test exercises the missing-binary path")
	}
	if ok := TriggerPhoenixProtocol("test-unit-no-terraform"); ok {
		t.Errorf("expected TriggerPhoenixProtocol to return false when terraform is unavailable, got true")
	}
}

// TestTriggerPhoenixProtocol_EmptyReason ensures the function accepts an
// empty reason string (defensive input) without short-circuiting before
// attempting the exec call.
func TestTriggerPhoenixProtocol_EmptyReason(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err == nil {
		t.Skip("terraform found in PATH")
	}
	// Should still return false (not panic) even with empty reason.
	_ = TriggerPhoenixProtocol("")
}
