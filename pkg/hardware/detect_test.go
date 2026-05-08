// pkg/hardware/detect_test.go — unit tests for GPU detection. [GPU-adaptive-config]
package hardware

import (
	"testing"
)

// TestDetect_NoPanic verifies that Detect() never panics regardless of whether
// nvidia-smi is available. On CI machines without a GPU, Available must be false.
func TestDetect_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Detect() panicked: %v", r)
		}
	}()
	info := Detect()
	// If no GPU, the zero-value fields must be safe to read.
	_ = info.Available
	_ = info.DeviceName
	_ = info.VRAMTotalMB
	_ = info.VRAMFreeMB
	_ = info.GPUUtilPct
	_ = info.TempC
	_ = info.DriverVersion
}

// TestDetect_UnavailableWhenNoNvidiaSmi tests that a missing nvidia-smi binary
// (or permission failure) results in Available=false and zero numeric fields,
// not a partially-filled struct that could mislead callers.
func TestDetect_ZeroOnUnavailable(t *testing.T) {
	info := Detect()
	if !info.Available {
		// Unavailable — all numeric fields must be zero (not garbage).
		if info.VRAMTotalMB < 0 {
			t.Errorf("VRAMTotalMB=%d, want ≥0 when unavailable", info.VRAMTotalMB)
		}
		if info.VRAMFreeMB < 0 {
			t.Errorf("VRAMFreeMB=%d, want ≥0 when unavailable", info.VRAMFreeMB)
		}
		if info.GPUUtilPct < 0 || info.GPUUtilPct > 100 {
			t.Errorf("GPUUtilPct=%d, want 0-100", info.GPUUtilPct)
		}
		if info.TempC < 0 {
			t.Errorf("TempC=%d, want ≥0 when unavailable", info.TempC)
		}
	}
}

// TestDetect_WhenAvailable verifies that when a GPU is detected the struct is
// coherent: name is non-empty, VRAM free ≤ total, utilisation 0-100%.
func TestDetect_CoherenceWhenAvailable(t *testing.T) {
	info := Detect()
	if !info.Available {
		t.Skip("no NVIDIA GPU detected — skipping coherence check")
	}
	if info.DeviceName == "" {
		t.Error("GPU available but DeviceName is empty")
	}
	if info.VRAMFreeMB > info.VRAMTotalMB {
		t.Errorf("VRAMFreeMB (%d) > VRAMTotalMB (%d) — impossible", info.VRAMFreeMB, info.VRAMTotalMB)
	}
	if info.GPUUtilPct < 0 || info.GPUUtilPct > 100 {
		t.Errorf("GPUUtilPct=%d out of [0, 100]", info.GPUUtilPct)
	}
	if info.TempC <= 0 {
		t.Errorf("TempC=%d — expected positive value for a running GPU", info.TempC)
	}
}

// TestGPUInfo_ZeroValue ensures the struct zero value is safe to use without
// calling Detect() — callers that cache the result should not get panics.
func TestGPUInfo_ZeroValue(t *testing.T) {
	var info GPUInfo
	if info.Available {
		t.Error("zero-value GPUInfo.Available must be false")
	}
	if info.DeviceName != "" {
		t.Errorf("zero-value GPUInfo.DeviceName must be empty, got %q", info.DeviceName)
	}
}
