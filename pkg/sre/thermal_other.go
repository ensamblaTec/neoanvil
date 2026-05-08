//go:build !darwin && !linux

package sre

// ThermalPressure is a no-op stub for unsupported platforms. [132.E]
// Always returns "low" (fail-open).
func ThermalPressure() string {
	return "low"
}
