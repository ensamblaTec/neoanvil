package sre

// [SRE-90] Predictive Elastic Scaling — Oracle → Nexus bridge.
//
// OracleBridge connects the OracleEngine (per-workspace prediction) to a
// channel that Nexus can consume. When FailProb24h crosses configurable
// thresholds, Nexus can throttle low-priority children to give resources
// to the at-risk workspace.
//
// Resource throttling via cgroups v2 is platform-specific and requires
// CAP_SYS_ADMIN. The bridge layer is platform-agnostic — it only produces
// Prediction events. The actuator (cgroups/renice) lives in cmd/neo-nexus.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Prediction is the message from Oracle to Nexus. [SRE-90.A.1]
type Prediction struct {
	WorkspaceID    string    `json:"workspace_id"`
	FailProb24h    float64   `json:"fail_prob_24h"`
	DominantSignal string    `json:"dominant_signal"` // "heap"|"power"|"combined"|"stable"
	HeapTrend      float64   `json:"heap_trend_mb_per_min"`
	PowerTrend     float64   `json:"power_trend_w_per_min"`
	At             time.Time `json:"at"`
}

// OracleBridge wraps an OracleEngine and emits Predictions to a channel.
// One bridge per workspace. Nexus subscribes to all bridges. [SRE-90.A.1]
type OracleBridge struct {
	engine      *OracleEngine
	workspaceID string
	Ch          chan Prediction // unbuffered or small buffer — Nexus reads asynchronously
}

// NewOracleBridge creates a bridge for the given workspace.
func NewOracleBridge(engine *OracleEngine, workspaceID string) *OracleBridge {
	return &OracleBridge{
		engine:      engine,
		workspaceID: workspaceID,
		Ch:          make(chan Prediction, 4),
	}
}

// Emit reads the current risk from the Oracle and sends a Prediction.
// Call this from the homeostasis tick loop. Non-blocking: if the channel
// is full, the prediction is dropped (Nexus missed a tick). [SRE-90.A.1]
func (b *OracleBridge) Emit() {
	risk := b.engine.Risk()
	pred := Prediction{
		WorkspaceID:    b.workspaceID,
		FailProb24h:    risk.FailProb24h,
		DominantSignal: risk.DominantSignal,
		HeapTrend:      risk.HeapTrendMBPerMin,
		PowerTrend:     risk.PowerTrendWPerMin,
		At:             risk.At,
	}
	select {
	case b.Ch <- pred:
	default:
		// Channel full — Nexus not reading fast enough. Drop.
	}
}

// ResourceThrottle represents a CPU/memory limit applied to a process. [SRE-90.B.1]
type ResourceThrottle struct {
	PID        int    `json:"pid"`
	CPUPercent int    `json:"cpu_percent"` // 0-100, 0 = unlimited
	Reason     string `json:"reason"`
}

// SetCPULimit applies a CPU limit to a process. [SRE-90.B.1]
// On Linux: uses cgroups v2 (requires root or CAP_SYS_ADMIN).
// On macOS/other: uses renice as best-effort fallback.
func SetCPULimit(pid, cpuPercent int) error {
	if cpuPercent <= 0 || cpuPercent > 100 {
		return fmt.Errorf("cpu_percent must be 1-100, got %d", cpuPercent)
	}

	switch runtime.GOOS {
	case "linux":
		return setCPULimitCgroups(pid, cpuPercent)
	default:
		return setCPULimitRenice(pid, cpuPercent)
	}
}

// setCPULimitCgroups applies CPU limit via cgroups v2.
func setCPULimitCgroups(pid, cpuPercent int) error {
	cgroupPath := fmt.Sprintf("/sys/fs/cgroup/neo-worker-%d", pid)

	// Create cgroup if not exists.
	if err := exec.Command("mkdir", "-p", cgroupPath).Run(); err != nil { //nolint:gosec // G204-LITERAL-BIN
		return fmt.Errorf("create cgroup: %w", err)
	}

	// Set CPU max: <quota> <period>. Period=100000us, quota=percent*1000us.
	quota := cpuPercent * 1000
	cpuMax := fmt.Sprintf("%d 100000", quota)
	if err := exec.Command("sh", "-c", fmt.Sprintf("echo '%s' > %s/cpu.max", cpuMax, cgroupPath)).Run(); err != nil { //nolint:gosec // G204-SHELL-WITH-VALIDATION: cgroupPath/cpuMax derived from internal state
		return fmt.Errorf("set cpu.max: %w", err)
	}

	// Move process to cgroup.
	if err := exec.Command("sh", "-c", fmt.Sprintf("echo %d > %s/cgroup.procs", pid, cgroupPath)).Run(); err != nil { //nolint:gosec // G204-SHELL-WITH-VALIDATION: cgroupPath is derived from pid (int), no injection surface
		return fmt.Errorf("move pid to cgroup: %w", err)
	}
	return nil
}

// setCPULimitRenice uses renice as a best-effort fallback on non-Linux.
func setCPULimitRenice(pid, cpuPercent int) error {
	// Map percentage to nice value: 100%=0 (normal), 50%=10, 10%=19 (lowest)
	nice := min(max(20-(cpuPercent*20/100), 0), 19)
	out, err := exec.Command("renice", "-n", fmt.Sprintf("%d", nice), "-p", fmt.Sprintf("%d", pid)).CombinedOutput() //nolint:gosec // G204-LITERAL-BIN
	if err != nil {
		return fmt.Errorf("renice: %s — %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// SetMemoryLimitCgroup applies an RSS limit via cgroups v2 (Linux only). [P-QUOTA]
// On non-Linux systems this is a documented no-op — callers must also start
// WatchProcessMemory as the cross-platform enforcement layer.
// Requires CAP_SYS_ADMIN or a pre-delegated cgroup subtree; fails gracefully
// when the capability is absent (log warning, plugin still starts).
func SetMemoryLimitCgroup(pid, limitMB int) error {
	if limitMB <= 0 {
		return nil
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	cgroupPath := fmt.Sprintf("/sys/fs/cgroup/neo-plugin-%d", pid)
	if err := exec.Command("mkdir", "-p", cgroupPath).Run(); err != nil { //nolint:gosec // G204-LITERAL-BIN
		return fmt.Errorf("create cgroup: %w", err)
	}
	memMax := strconv.FormatInt(int64(limitMB)*1024*1024, 10)
	if err := exec.Command("sh", "-c", fmt.Sprintf("echo %s > %s/memory.max", memMax, cgroupPath)).Run(); err != nil { //nolint:gosec // G204-SHELL-WITH-VALIDATION: cgroupPath derived from pid (int), memMax from int64 — no injection surface
		return fmt.Errorf("set memory.max: %w", err)
	}
	if err := exec.Command("sh", "-c", fmt.Sprintf("echo %d > %s/cgroup.procs", pid, cgroupPath)).Run(); err != nil { //nolint:gosec // G204-SHELL-WITH-VALIDATION: pid is int, cgroupPath from pid
		return fmt.Errorf("move pid to cgroup: %w", err)
	}
	return nil
}

// WatchProcessMemory polls process RSS every 2 seconds and calls killFn when
// RSS > limitMB. Stops automatically when ctx is cancelled or the process exits.
// Runs in its own goroutine — returns immediately without blocking.
//
// Uses /proc/<pid>/status on Linux and `ps -o rss=` on macOS, both returning
// physical RSS. This avoids the RLIMIT_AS virtual-address false-positive that
// would crash Go-based plugins mapping large VA ranges at startup. [P-QUOTA]
func WatchProcessMemory(ctx context.Context, pid, limitMB int, killFn func()) {
	if limitMB <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rss, err := processRSSMB(pid)
				if err != nil {
					return // process exited
				}
				if rss > limitMB {
					log.Printf("[PLUGIN-OOM-WATCH] pid=%d rss=%dMB > limit=%dMB — triggering kill", pid, rss, limitMB)
					killFn()
					return
				}
			}
		}
	}()
}

func processRSSMB(pid int) (int, error) {
	switch runtime.GOOS {
	case "linux":
		return processRSSLinux(pid)
	default:
		return processRSSPS(pid)
	}
}

// processRSSLinux reads VmRSS from /proc/<pid>/status.
func processRSSLinux(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)) //nolint:gosec // G304-DIR-WALK: path derived from pid (int), no injection surface
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			break
		}
		return kb / 1024, nil
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/%d/status", pid)
}

// processRSSPS uses `ps -o rss=` as the cross-platform fallback (macOS/other).
// Returns RSS in MB; errors when the process no longer exists.
func processRSSPS(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // G204-LITERAL-BIN
	if err != nil {
		return 0, err
	}
	kb, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parse rss from ps: %w", err)
	}
	return kb / 1024, nil
}
