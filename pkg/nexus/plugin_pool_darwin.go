//go:build darwin

package nexus

import (
	"os/exec"
	"syscall"
)

// applyParentDeathSignal — Darwin does not expose Pdeathsig. Best we can
// do is Setpgid:true so the child is in its own process group; the
// PluginPool's StopAll path then signals the group instead of just the
// pid. For ABRUPT parent death (Nexus segfault), well-behaved plugins
// still fall through stdin-EOF detection — the read end of the pipe
// returns EOF when the kernel cleans up Nexus's fd table.
//
// True orphan containment on macOS would require launchd integration or
// a watcher subprocess; out of scope for MVP.
func applyParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
