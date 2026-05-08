//go:build darwin

package sre

import (
	"errors"
	"testing"
)

// TestPowermetricsUnavailableReturnsLow verifies that when both probes fail,
// ThermalPressure returns "low" (fail-open). [132.E]
func TestPowermetricsUnavailableReturnsLow(t *testing.T) {
	failSysctl := func() (int, error) { return 0, errors.New("no xcpm") }
	failPowermetrics := func() (string, error) { return "", errors.New("no powermetrics") }

	got := thermalPressureWithProbe(failSysctl, failPowermetrics)
	if got != "low" {
		t.Errorf("thermalPressureWithProbe = %q, want %q", got, "low")
	}
}

// TestLevel3ReturnsCritical verifies that a sysctl level of 3 maps to "critical". [132.E]
func TestLevel3ReturnsCritical(t *testing.T) {
	stubSysctl := func() (int, error) { return 3, nil }
	stubPowermetrics := func() (string, error) { return "low", nil } // should not be reached

	got := thermalPressureWithProbe(stubSysctl, stubPowermetrics)
	if got != "critical" {
		t.Errorf("thermalPressureWithProbe(level=3) = %q, want %q", got, "critical")
	}
}
