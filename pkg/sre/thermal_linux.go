//go:build linux

package sre

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ThermalPressure returns the current system thermal state on Linux. [132.E]
// Returns "low", "medium", or "critical". Fail-open: returns "low" on any error.
// Reads /sys/class/thermal/thermal_zone0/temp (millidegrees Celsius).
func ThermalPressure() string {
	return thermalPressureWithProbe(thermalZoneLevel)
}

// thermalPressureWithProbe is the injectable core for testing. [132.E]
func thermalPressureWithProbe(fn func() (int, error)) string {
	if temp, err := fn(); err == nil {
		return thermalTempToStr(temp)
	}
	return "low" // fail-open
}

// thermalTempToStr maps millidegree Celsius values to a string category. [132.E]
// ≥90 000 mC (90°C) = critical, ≥70 000 mC (70°C) = medium, else = low.
func thermalTempToStr(millidegrees int) string {
	switch {
	case millidegrees >= 90000:
		return "critical"
	case millidegrees >= 70000:
		return "medium"
	default:
		return "low"
	}
}

// thermalZoneLevel reads /sys/class/thermal/thermal_zone0/temp. [132.E]
func thermalZoneLevel() (int, error) {
	data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp") //nolint:gosec // G304-DIR-WALK: fixed sysfs path, not user-supplied
	if err != nil {
		return 0, fmt.Errorf("thermal_zone: %w", err)
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("thermal_zone parse: %w", err)
	}
	return val, nil
}
