//go:build !linux && !darwin

package nexus

import (
	"os/exec"
	"syscall"
)

// terminateProcessGroup — fallback for windows etc. Signals the leader pid
// directly. Sub-process orphans must be reaped by the OS.
func terminateProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}
