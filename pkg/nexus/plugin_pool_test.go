package nexus

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

// TestMain dispatches the test binary to act as a fake plugin subprocess
// when the PLUGIN_POOL_TEST_MODE env var is set. This is the standard
// stdlib pattern (see os/exec tests) for deterministic subprocess behavior
// across platforms — avoids relying on shell-specific `trap` semantics.
//
// "stubborn" mode: install SIGTERM handler, then write READY\n to stdout
// so the parent can synchronize before sending signals.
func TestMain(m *testing.M) {
	switch os.Getenv("PLUGIN_POOL_TEST_MODE") {
	case "stubborn":
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		go func() {
			for range c {
			}
		}()
		fmt.Println("READY") // signal after handler is installed
		select {}
	}
	os.Exit(m.Run())
}

// awaitReady reads "READY\n" (6 bytes) from the plugin's stdout. Used to
// synchronize parent ↔ stubborn-child before sending signals.
func awaitReady(t *testing.T, r io.Reader) {
	t.Helper()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read READY: %v", err)
	}
	if string(buf) != "READY\n" {
		t.Fatalf("got %q want %q", buf, "READY\n")
	}
}

func makeSpec(name string) *plugin.PluginSpec {
	return &plugin.PluginSpec{
		Name:    name,
		Binary:  "/bin/sleep",
		Args:    []string{"30"},
		Tier:    plugin.TierNexus,
		Enabled: true,
	}
}

func waitDone(t *testing.T, proc *PluginProcess) {
	t.Helper()
	select {
	case <-proc.Done():
	case <-time.After(3 * time.Second):
		t.Fatalf("plugin %s did not exit within 3s", proc.Spec.Name)
	}
}

func TestPluginPool_StartAndStop(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	proc, err := pool.Start(makeSpec("p1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if proc.PID <= 0 {
		t.Errorf("PID=%d want >0", proc.PID)
	}
	if proc.Status != PluginStatusRunning {
		t.Errorf("status=%s want running", proc.Status)
	}

	if err := pool.Stop("p1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitDone(t, proc)

	if proc.Status != PluginStatusStopped && proc.Status != PluginStatusErrored {
		t.Errorf("after stop status=%s want stopped|errored", proc.Status)
	}
	if _, ok := pool.Get("p1"); ok {
		t.Error("plugin still present in active map after exit")
	}
}

func TestPluginPool_StartTwiceFails(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	spec := makeSpec("p2")
	proc, err := pool.Start(spec)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.Stop("p2")
		<-proc.Done()
	})

	if _, err := pool.Start(spec); err == nil {
		t.Error("second Start should fail")
	}
}

func TestPluginPool_StopMissingFails(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	if err := pool.Stop("nothing"); err == nil {
		t.Error("Stop on missing plugin should fail")
	}
}

func TestPluginPool_BuildEnv_VaultHitAndMiss(t *testing.T) {
	vault := func(name string) (string, bool) {
		if name == "FAKE_TOKEN" {
			return "secret-value", true
		}
		return "", false
	}
	pool := NewPluginPool(vault, t.TempDir())
	spec := &plugin.PluginSpec{
		Name:         "envtest",
		Binary:       "/bin/true",
		EnvFromVault: []string{"FAKE_TOKEN", "MISSING"},
	}
	env, missing := pool.buildEnv(spec)

	if !slices.Contains(env, "FAKE_TOKEN=secret-value") {
		t.Error("FAKE_TOKEN not injected into env")
	}
	if len(missing) != 1 || missing[0] != "MISSING" {
		t.Errorf("missing=%v want [MISSING]", missing)
	}
}

func TestPluginPool_StartNilSpecFails(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	if _, err := pool.Start(nil); err == nil {
		t.Error("Start(nil) should fail")
	}
}

func TestPluginPool_StartInvalidBinaryFails(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	spec := &plugin.PluginSpec{
		Name:   "ghost",
		Binary: "/does/not/exist/xyzqq",
	}
	if _, err := pool.Start(spec); err == nil {
		t.Error("Start with bogus binary should fail")
	}
}

func TestPluginPool_ListAndGet(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	proc, err := pool.Start(makeSpec("listtest"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.Stop("listtest")
		<-proc.Done()
	})

	if list := pool.List(); len(list) != 1 {
		t.Errorf("List len=%d want 1", len(list))
	}

	got, ok := pool.Get("listtest")
	if !ok || got.PID != proc.PID {
		t.Errorf("Get mismatch: ok=%v PID=%d want %d", ok, got.PID, proc.PID)
	}

	if _, ok := pool.Get("missing"); ok {
		t.Error("Get for missing should be false")
	}
}

func TestPluginPool_StopAll(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	p1, err := pool.Start(makeSpec("a"))
	if err != nil {
		t.Fatalf("Start a: %v", err)
	}
	p2, err := pool.Start(makeSpec("b"))
	if err != nil {
		t.Fatalf("Start b: %v", err)
	}

	if err := pool.StopAll(); err != nil {
		t.Errorf("StopAll: %v", err)
	}
	waitDone(t, p1)
	waitDone(t, p2)

	if active := pool.List(); len(active) != 0 {
		t.Errorf("after StopAll active=%d want 0", len(active))
	}
}

func TestPluginPool_LogFileWritten(t *testing.T) {
	logsDir := t.TempDir()
	pool := NewPluginPool(nil, logsDir)
	spec := &plugin.PluginSpec{
		Name:    "logtest",
		Binary:  "/bin/sh",
		Args:    []string{"-c", "echo hello-stderr 1>&2; sleep 30"},
		Tier:    plugin.TierNexus,
		Enabled: true,
	}
	proc, err := pool.Start(spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.Stop("logtest")
		<-proc.Done()
	})

	time.Sleep(300 * time.Millisecond) // let stderr flush

	logPath := filepath.Join(logsDir, "plugin-logtest.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "hello-stderr") {
		t.Errorf("log content=%q want contains 'hello-stderr'", string(data))
	}
}

func TestPluginPool_OpenLogFileMissingDirFails(t *testing.T) {
	pool := NewPluginPool(nil, "")
	if _, err := pool.openLogFile("x"); err == nil {
		t.Error("expected error for empty logsDir")
	}
}

func TestPluginPool_SigkillEscalationOnTermIgnored(t *testing.T) {
	// Use the test binary itself as a fake plugin via TestMain dispatch.
	// PLUGIN_POOL_TEST_MODE=stubborn → process ignores SIGTERM.
	vault := func(name string) (string, bool) {
		if name == "PLUGIN_POOL_TEST_MODE" {
			return "stubborn", true
		}
		return "", false
	}
	pool := NewPluginPool(vault, t.TempDir())
	spec := &plugin.PluginSpec{
		Name:         "stubborn",
		Binary:       os.Args[0],
		EnvFromVault: []string{"PLUGIN_POOL_TEST_MODE"},
		Tier:         plugin.TierNexus,
		Enabled:      true,
	}
	proc, err := pool.Start(spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitReady(t, proc.Stdout) // signal handler is now installed

	start := time.Now()
	if err := pool.StopGracefully("stubborn", 100*time.Millisecond); err != nil {
		t.Fatalf("StopGracefully: %v", err)
	}
	elapsed := time.Since(start)

	// Should escalate to SIGKILL after ~100ms grace, total well under 1s.
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned in %s, expected >= grace 100ms", elapsed)
	}
	if elapsed > 1*time.Second {
		t.Errorf("returned in %s, escalation took too long", elapsed)
	}

	select {
	case <-proc.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("process did not exit after SIGKILL")
	}
}

func TestPluginPool_StopGracefullyZeroIsNonBlocking(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	proc, err := pool.Start(makeSpec("fast"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { <-proc.Done() })

	start := time.Now()
	if err := pool.StopGracefully("fast", 0); err != nil {
		t.Fatalf("StopGracefully(0): %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("grace=0 returned in %s, should be near-instant", elapsed)
	}
}

func TestPluginPool_StopAllGracefullyParallel(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	procs := make([]*PluginProcess, 0, 3)
	for _, n := range []string{"px", "py", "pz"} {
		p, err := pool.Start(makeSpec(n))
		if err != nil {
			t.Fatalf("Start %s: %v", n, err)
		}
		procs = append(procs, p)
	}

	start := time.Now()
	if err := pool.StopAllGracefully(2 * time.Second); err != nil {
		t.Fatalf("StopAllGracefully: %v", err)
	}
	elapsed := time.Since(start)

	// Plugins respond to SIGTERM cleanly so all should exit fast. Even if
	// serialized, this would be quick — the assertion is that StopAll
	// returns before any single grace window expires.
	if elapsed > 1*time.Second {
		t.Errorf("StopAll took %s, expected fast response", elapsed)
	}
	for _, p := range procs {
		select {
		case <-p.Done():
		case <-time.After(500 * time.Millisecond):
			t.Errorf("plugin %s did not exit", p.Spec.Name)
		}
	}
	if active := pool.List(); len(active) != 0 {
		t.Errorf("after StopAll active=%d want 0", len(active))
	}
}

func TestPluginPool_StopAllEmptyPoolIsNoop(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	if err := pool.StopAll(); err != nil {
		t.Errorf("StopAll on empty pool: %v", err)
	}
}

func TestNewPluginPool_NilVaultIsSafe(t *testing.T) {
	pool := NewPluginPool(nil, t.TempDir())
	spec := &plugin.PluginSpec{Name: "x", EnvFromVault: []string{"ANY"}}
	env, missing := pool.buildEnv(spec)
	if len(missing) != 1 {
		t.Errorf("missing=%v want [ANY]", missing)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANY=") {
			t.Error("nil vault should not inject env vars")
		}
	}
}
