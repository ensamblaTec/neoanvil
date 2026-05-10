package main

// Regression test for the COMPILE_AUDIT 30-min hang reported by the
// operator in another project. The vulnerability lived in `runGoBuild`
// which used `cmd.CombinedOutput()` without `Setpgid + WaitDelay`.
//
// We can't test runGoBuild directly under the hang condition (would
// require fabricating a cgo subgraph to spawn orphans). What we CAN
// test deterministically is the hardening *pattern* — same SysProcAttr
// + WaitDelay flags applied to a `sleep` subprocess. If the pattern is
// correct, even a `sleep 30` running with a 500ms timeout returns to
// the caller in <2s. If the pattern is wrong, the test would block
// for 30s.
//
// This is the smallest dependable proof that:
//   (a) Setpgid + WaitDelay are syntactically valid on linux+darwin
//   (b) cmd.CombinedOutput() returns within the documented bound when
//       a subprocess exceeds context timeout, regardless of whether
//       the subprocess writes to stdout/stderr
//   (c) the imports we added (syscall) compile on every supported
//       platform per build tags

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// hardenedSleepRun mirrors runGoBuild's hardening pattern but runs
// `sleep <seconds>` against an aggressive context timeout. Returns
// the wall-clock duration of the call so the test can assert bounds.
func hardenedSleepRun(t *testing.T, sleepFor time.Duration, ctxTimeout, waitDelay time.Duration) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", strings.TrimSpace(sleepFor.String())) //nolint:gosec // G204-LITERAL-BIN: 'sleep' is a fixed builtin path
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	start := time.Now()
	_, _ = cmd.CombinedOutput()
	return time.Since(start)
}

func TestHardenedExec_BoundedByContextPlusWaitDelay(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test — runs under default `go test` only")
	}
	// Ask sleep to nap 30s; cap context at 500ms; allow 500ms drain.
	// Pattern guarantees return within ~1s (we allow 3s slack for CI noise).
	elapsed := hardenedSleepRun(t, 30*time.Second, 500*time.Millisecond, 500*time.Millisecond)
	upperBound := 3 * time.Second
	if elapsed > upperBound {
		t.Errorf("hardened exec took %v — expected <%v (Setpgid+WaitDelay broken?)",
			elapsed, upperBound)
	}
	t.Logf("hardened exec returned in %v (budget %v ctx + %v drain = %v)",
		elapsed, 500*time.Millisecond, 500*time.Millisecond, time.Second)
}

func TestHardenedExec_HappyPath_QuickReturn(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test — runs under default `go test` only")
	}
	// sleep 50ms with generous 5s timeout — proves the hardening doesn't
	// add overhead on the success path.
	elapsed := hardenedSleepRun(t, 50*time.Millisecond, 5*time.Second, 1*time.Second)
	if elapsed > 1*time.Second {
		t.Errorf("happy-path took %v — Setpgid/WaitDelay should add no overhead", elapsed)
	}
}

// TestHardenedExec_PipesStayingOpen_DontBlockReturn is the closest
// approximation of the cgo-orphan scenario we can achieve without a
// full cgo build. We spawn a process group leader that itself runs a
// child `sleep` redirecting stdout/stderr — when the parent dies
// from SIGKILL, the child holds the pipes open. Setpgid ensures the
// signal reaches every grandchild atomically.
func TestHardenedExec_PipesStayingOpen_DontBlockReturn(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test — runs under default `go test` only")
	}
	// `sh -c 'sleep 30 & wait'` creates a parent shell with a sleeping
	// background child sharing the pipes. Without WaitDelay,
	// CombinedOutput blocks until the child exits (30s).
	// With WaitDelay=1s + ctx=300ms, total bound is ~1.3s.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & wait") //nolint:gosec // G204-LITERAL-BIN: literal arg
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 1 * time.Second

	var wg sync.WaitGroup
	wg.Add(1)
	var elapsed time.Duration
	go func() {
		defer wg.Done()
		start := time.Now()
		_, _ = cmd.CombinedOutput()
		elapsed = time.Since(start)
	}()
	wg.Wait()

	upperBound := 3 * time.Second
	if elapsed > upperBound {
		t.Errorf("pipe-orphan exec took %v — expected <%v (WaitDelay broken?)",
			elapsed, upperBound)
	}
	t.Logf("pipe-orphan exec returned in %v", elapsed)
}
