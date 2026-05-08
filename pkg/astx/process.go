package astx

import (
	"fmt"
	"os/exec"
	"sync"
)

var (
	processMu sync.Mutex
	processes = make(map[int]*exec.Cmd)
)

func RegisterProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	processMu.Lock()
	defer processMu.Unlock()
	processes[cmd.Process.Pid] = cmd
}

func UnregisterProcess(pid int) {
	processMu.Lock()
	defer processMu.Unlock()
	delete(processes, pid)
}

func KillProcess(pid int) error {
	processMu.Lock()
	cmd, exists := processes[pid]
	processMu.Unlock()

	if !exists {
		return fmt.Errorf("Process PID %d not found in SRE tracker", pid)
	}

	if cmd.Process != nil {
		err := cmd.Process.Kill()
		UnregisterProcess(pid)
		return err
	}
	return nil
}