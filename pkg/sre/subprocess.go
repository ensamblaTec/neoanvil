package sre

// pkg/sre/subprocess.go — subprocess hardening helpers shared across
// every site that exec's a child process under a context timeout.
//
// Background:
//   `cmd.CombinedOutput()` (and `cmd.Wait()`) block until ALL stdout/
//   stderr pipes close. When `go build` invokes cgo → gcc → ld, those
//   grandchild processes are NOT in the same process group as the parent,
//   so the SIGKILL the Go runtime sends on context cancel reaches the
//   parent only. Grandchildren survive holding the pipes open. The parent
//   is gone but the pipes stay alive → CombinedOutput hangs until the
//   grandchildren finish naturally. For heavy cgo / proto-gen / image-
//   processing repos this can be tens of minutes.
//
//   The fix is two-pronged:
//     1. Setpgid:true → child is a process-group leader, so SIGKILL
//        on context cancel reaches every descendant atomically.
//     2. cmd.WaitDelay (Go 1.20+) → caps the pipe-drain wait. Worst-
//        case bound becomes `ctx_deadline + waitDelay`, regardless
//        of how unkillable the grandchildren are.
//
// First applied to runGoBuild after operator-reported 30-min hangs in
// COMPILE_AUDIT (Nexus debt T006-related sweep, 2026-05-10). Centralised
// here so every other call site in the repo can opt-in with a single
// import + 1-line call.

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// defaultWaitDelay is the cap applied when caller doesn't pass a value.
// 5s is generous enough to drain typical compiler error output but
// short enough that an orphaned cgo grandchild can't pin the goroutine.
const defaultWaitDelay = 5 * time.Second

// HardenSubprocess applies the Setpgid + WaitDelay safety nets to an
// existing *exec.Cmd. Use this when you've already constructed the
// command with exec.Command/exec.CommandContext and want to retrofit
// the hardening with a single line.
//
//	cmd := exec.CommandContext(ctx, "go", "build", pkg)
//	sre.HardenSubprocess(cmd, 0) // 0 → defaultWaitDelay (5s)
//	out, err := cmd.CombinedOutput()
//
// waitDelay <= 0 picks defaultWaitDelay. Pass a tighter value when you
// know the subprocess never produces output (e.g. `renice`).
func HardenSubprocess(cmd *exec.Cmd, waitDelay time.Duration) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if waitDelay <= 0 {
		waitDelay = defaultWaitDelay
	}
	cmd.WaitDelay = waitDelay
}

// HardenedExec is the convenience constructor: returns a *exec.Cmd
// already wired with Setpgid + WaitDelay. For new code prefer this
// over exec.CommandContext + HardenSubprocess.
//
//	out, err := sre.HardenedExec(ctx, 0, "sh", "-c", buildCmd).CombinedOutput()
//
// waitDelay semantics match HardenSubprocess.
func HardenedExec(ctx context.Context, waitDelay time.Duration, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	HardenSubprocess(cmd, waitDelay)
	return cmd
}
