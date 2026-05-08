//go:build linux

package nexus

import (
	"os/exec"
	"syscall"
)

// applyParentDeathSignal — Linux-specific. Pdeathsig tells the kernel to
// send SIGKILL to the child the moment the parent (Nexus) dies for ANY
// reason (segfault, OOM, kill -9). Belt-and-suspenders on top of the
// stdin-EOF detection that well-behaved plugins already use.
//
// Setpgid:true also places the child in its own process group. Combined
// with Pdeathsig, this guarantees no orphan plugin survives an abrupt
// Nexus crash on Linux.
func applyParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	cmd.SysProcAttr.Setpgid = true
}
