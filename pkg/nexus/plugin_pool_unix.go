//go:build linux || darwin

package nexus

import (
	"os/exec"
	"syscall"
)

// terminateProcessGroup signals the entire process group of cmd. Requires
// applyParentDeathSignal (Setpgid:true) was applied at spawn time, which
// it is for every PluginPool.Start invocation.
//
// Why group, not just leader pid: plugins may exec sub-processes (sh -c,
// helper binaries). Signaling only the leader leaves grandchildren as
// orphans holding the inherited stderr fd open, which blocks
// cmd.Wait() (the io.MultiWriter copy goroutine drains until ALL writers
// close their pipe end). Killing the group ensures clean teardown.
//
// Falls back to direct pid signal when Getpgid fails (race: process
// already exited).
func terminateProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return cmd.Process.Signal(sig)
	}
	return syscall.Kill(-pgid, sig)
}
