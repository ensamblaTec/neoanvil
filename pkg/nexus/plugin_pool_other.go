//go:build !linux && !darwin

package nexus

import "os/exec"

// applyParentDeathSignal — fallback for unsupported OSes (windows, etc.).
// No-op; relies entirely on plugins reading stdin and exiting on EOF.
func applyParentDeathSignal(cmd *exec.Cmd) {}
