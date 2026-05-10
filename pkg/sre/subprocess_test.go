package sre

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHardenedExec_BoundedByContextPlusWaitDelay confirms the standard
// claim: a `sleep 30` aborted via 500ms ctx + 500ms WaitDelay returns
// to the caller in ~1s. Without the hardening pattern this would
// block for 30s. Replicates the operator's reported COMPILE_AUDIT
// 30-min hang scenario in miniature.
func TestHardenedExec_BoundedByContextPlusWaitDelay(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test — runs under default `go test` only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd := HardenedExec(ctx, 500*time.Millisecond, "sleep", "30")
	start := time.Now()
	_, _ = cmd.CombinedOutput()
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("hardened exec took %v — expected < 3s (Setpgid+WaitDelay broken?)", elapsed)
	}
	t.Logf("hardened exec returned in %v", elapsed)
}

// TestHardenSubprocess_RetrofitPath validates the retrofit shape: an
// exec.Cmd built with exec.CommandContext can have hardening applied
// after construction with a single 1-line call.
func TestHardenSubprocess_RetrofitPath(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & wait")
	HardenSubprocess(cmd, 1*time.Second)

	var elapsed time.Duration
	var wg sync.WaitGroup
	wg.Go(func() {
		start := time.Now()
		_, _ = cmd.CombinedOutput()
		elapsed = time.Since(start)
	})
	wg.Wait()
	if elapsed > 3*time.Second {
		t.Errorf("retrofit path took %v — expected < 3s", elapsed)
	}
}

// TestHardenSubprocess_NilSafe confirms the helper doesn't panic on a
// nil cmd. Defensive — operators that build commands conditionally
// shouldn't have to nil-check every site.
func TestHardenSubprocess_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("HardenSubprocess panicked on nil cmd: %v", r)
		}
	}()
	HardenSubprocess(nil, 0)
}

// TestHardenSubprocess_ZeroWaitDelayPicksDefault confirms the contract:
// passing 0 picks the package default. Tighter values still honored.
func TestHardenSubprocess_ZeroWaitDelayPicksDefault(t *testing.T) {
	cmd := exec.Command("true")
	HardenSubprocess(cmd, 0)
	if cmd.WaitDelay != defaultWaitDelay {
		t.Errorf("waitDelay=0 should pick defaultWaitDelay (%v), got %v",
			defaultWaitDelay, cmd.WaitDelay)
	}
	cmd2 := exec.Command("true")
	HardenSubprocess(cmd2, 250*time.Millisecond)
	if cmd2.WaitDelay != 250*time.Millisecond {
		t.Errorf("waitDelay=250ms not honored, got %v", cmd2.WaitDelay)
	}
}

// TestHardenSubprocess_HappyPathQuick confirms zero overhead on the
// success path: a cmd that finishes in 50ms shouldn't be slowed down
// by Setpgid/WaitDelay machinery.
func TestHardenSubprocess_HappyPathQuick(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := HardenedExec(ctx, 0, "sh", "-c", "echo ok && sleep 0.05")
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
	if !strings.Contains(string(out), "ok") {
		t.Errorf("expected stdout to contain 'ok', got %q", out)
	}
	if elapsed > 1*time.Second {
		t.Errorf("happy path took %v — Setpgid/WaitDelay should add no overhead", elapsed)
	}
}
