package hardware

import (
	"os/exec"
	"strconv"
	"strings"
)

// GPUInfo contains runtime GPU information detected via nvidia-smi.
// Available is false when no NVIDIA GPU is found or nvidia-smi is absent.
type GPUInfo struct {
	Available     bool
	DeviceName    string
	VRAMTotalMB   int64
	VRAMFreeMB    int64
	DriverVersion string
	GPUUtilPct    int
	TempC         int
}

// Detect probes nvidia-smi for live GPU metrics. Safe to call repeatedly —
// each call reflects current utilization and temperature.
// Returns zero-value GPUInfo with Available=false on any error
// (no NVIDIA driver, no GPU, nvidia-smi absent).
func Detect() GPUInfo {
	out, err := exec.Command("nvidia-smi", //nolint:gosec // G204-LITERAL-BIN: binary literal, args are fixed constants
		"--query-gpu=name,memory.total,memory.free,driver_version,utilization.gpu,temperature.gpu",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return GPUInfo{}
	}
	// nvidia-smi CSV with nounits: name, MiB total, MiB free, driver, util%, temp°C
	parts := strings.SplitN(strings.TrimSpace(string(out)), ", ", 6)
	if len(parts) < 4 {
		return GPUInfo{}
	}
	info := GPUInfo{
		Available:     true,
		DeviceName:    strings.TrimSpace(parts[0]),
		DriverVersion: strings.TrimSpace(parts[3]),
	}
	if v, e := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); e == nil {
		info.VRAMTotalMB = v
	}
	if v, e := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64); e == nil {
		info.VRAMFreeMB = v
	}
	if len(parts) >= 5 {
		if v, e := strconv.Atoi(strings.TrimSpace(parts[4])); e == nil {
			info.GPUUtilPct = v
		}
	}
	if len(parts) >= 6 {
		if v, e := strconv.Atoi(strings.TrimSpace(parts[5])); e == nil {
			info.TempC = v
		}
	}
	return info
}
