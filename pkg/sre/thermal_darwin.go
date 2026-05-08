//go:build darwin

package sre

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ThermalPressure returns the current system thermal state on macOS. [132.E]
// Returns "low", "medium", or "critical". Fail-open: returns "low" on any error.
// Tries sysctl machdep.xcpm.cpu_thermal_level (Intel) first, falls back to
// powermetrics JSON (Apple Silicon / universal).
func ThermalPressure() string {
	return thermalPressureWithProbe(sysctlThermalLevel, powermetricsThermalLevel)
}

// thermalPressureWithProbe is the injectable core for testing. [132.E]
func thermalPressureWithProbe(sysctlFn func() (int, error), powermetricsFn func() (string, error)) string {
	if level, err := sysctlFn(); err == nil {
		return thermalLevelToStr(level)
	}
	if p, err := powermetricsFn(); err == nil {
		return p
	}
	return "low" // fail-open
}

// thermalLevelToStr maps an integer thermal level to a string category. [132.E]
// Level 0 = normal, 1-2 = throttling, 3+ = critical.
func thermalLevelToStr(level int) string {
	switch {
	case level >= 3:
		return "critical"
	case level >= 1:
		return "medium"
	default:
		return "low"
	}
}

// sysctlThermalLevel reads machdep.xcpm.cpu_thermal_level via syscall. [132.E]
// Returns an error on Apple Silicon or when the sysctl key is absent.
func sysctlThermalLevel() (int, error) {
	out, err := syscall.Sysctl("machdep.xcpm.cpu_thermal_level")
	if err != nil {
		return 0, fmt.Errorf("sysctl xcpm: %w", err)
	}
	val, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("sysctl parse: %w", err)
	}
	return val, nil
}

// powermetricsThermalPayload is the relevant subset of powermetrics JSON output. [132.E]
type powermetricsThermalPayload struct {
	ThermalPressure string `json:"thermal_pressure"`
}

// powermetricsThermalLevel queries powermetrics for thermal pressure. [132.E]
// Runs with a 500ms timeout to avoid blocking the homeostasis loop.
func powermetricsThermalLevel() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	//nolint:gosec // G204-LITERAL-BIN: exec.CommandContext with literal binary path
	cmd := exec.CommandContext(ctx, "powermetrics", "--samplers", "thermal", "-n", "1", "-i", "1", "-f", "json")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("powermetrics: %w", err)
	}
	var payload powermetricsThermalPayload
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("powermetrics parse: %w", err)
	}
	switch strings.ToLower(payload.ThermalPressure) {
	case "critical", "serious":
		return "critical", nil
	case "moderate", "elevated":
		return "medium", nil
	default:
		return "low", nil
	}
}
